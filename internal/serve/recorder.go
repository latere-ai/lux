// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The three metrics of spec 019 that are spec 009's, recorded at the
// record and at the flush.
const (
	// MetricTokens adds each record's input, output, cached_input, and
	// cache_write counts by direction.
	MetricTokens = "lux_tokens_total"
	// MetricSpend adds each priced record's cost in micro-units by
	// currency.
	MetricSpend = "lux_spend_microunits_total"
	// MetricFlushLag is the seconds since this replica's last successful
	// flush of its spend counters and its aggregates, so a stalled store
	// shows as a growing gauge before any limit is wrong by more than the
	// bound.
	MetricFlushLag = "lux_metering_flush_lag_seconds"
)

// pricingTimeout bounds the catalog read that prices one record. The
// read runs after the response is finished, so it adds nothing to the
// caller's latency, and a store that does not answer in this long
// leaves the record unpriced rather than holding the handler.
const pricingTimeout = 5 * time.Second

// countRoutes are the door table's token counts, whose records carry no
// price: the count is answered from the estimate or by an upstream
// that bills nothing for it, and pricing it would bill a number no
// completion was made from.
var countRoutes = map[string]bool{
	"/anthropic/v1/messages/count_tokens":       true,
	"/gemini/v1beta/models/{model}:countTokens": true,
	"/lux/v1/count_tokens":                      true,
}

// RecorderOptions is what the Recorder runs under.
type RecorderOptions struct {
	// Store receives the hourly aggregates at each flush and every
	// record into its per-Key ring.
	Store store.Store
	// Catalog is where the resolved Model's pricing is read from, by the
	// name the record carries; nil prices nothing.
	Catalog gateway.Catalog
	// Limiter is the Limiter whose spend counters flush beside these
	// aggregates; the lag gauge reads the older of the two flushes. Nil
	// reads the Recorder's own.
	Limiter *Limiter
	// Metrics receives the three metrics; nil records none.
	Metrics *metrics.Registry
	// Flush is LUX_METERING_FLUSH, how often Run writes the aggregates;
	// zero is metering.DefaultFlush.
	Flush time.Duration
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Recorder is spec 004's Recorder as spec 009 builds it: the one
// metering.Record of every request, the gateway's record with the cost
// the Model's pricing gives its tokens, priced false on a count and an
// opaque route and for a Model with no pricing. Each record goes into
// the store's per-Key ring at once and into this replica's hourly
// aggregate deltas, which Flush upserts through Store.Usage() on
// LUX_METERING_FLUSH. The Recorder adds to none of the Key's or the
// Budget's counters: those are the Limiter's, fed at admission and at
// the settle, or every admitted request would count twice.
type Recorder struct {
	o      RecorderOptions
	tokens *metrics.Counter
	spend  *metrics.Counter

	mu        sync.Mutex
	rows      map[metering.AggregateKey]*metering.Aggregate
	lastFlush time.Time
}

// NewRecorder constructs the Recorder; Run starts its flush.
func NewRecorder(o RecorderOptions) *Recorder {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Flush <= 0 {
		o.Flush = metering.DefaultFlush
	}
	r := &Recorder{o: o, rows: map[metering.AggregateKey]*metering.Aggregate{}, lastFlush: o.Now()}
	if o.Metrics != nil {
		r.tokens = o.Metrics.Counter(MetricTokens, "Tokens metered by direction: input, output, cached_input, cache_write.")
		r.spend = o.Metrics.Counter(MetricSpend, "Spend metered in micro-units of the currency, priced records only.")
		o.Metrics.Gauge(MetricFlushLag, "Seconds since this replica last flushed its spend counters and usage aggregates.", func() []metrics.LabeledValue {
			return []metrics.LabeledValue{{Labels: map[string]string{}, Value: r.lag().Seconds()}}
		})
	}
	return r
}

// Record implements gateway.Recorder.
func (r *Recorder) Record(rec gateway.Record) {
	m := r.convert(rec)
	ctx, cancel := context.WithTimeout(context.Background(), pricingTimeout)
	defer cancel()
	if err := r.o.Store.Usage().AppendRecord(ctx, m); err != nil {
		r.o.Logger.ErrorContext(ctx, "metering: appending the record to the ring", "request", m.ID, "key", m.Key.ID, "err", err)
	}
	a := metering.AggregateOf(m)
	r.mu.Lock()
	if have, ok := r.rows[a.Key()]; ok {
		have.Add(a.Sums)
		have.Labels = a.Labels
	} else {
		r.rows[a.Key()] = &a
	}
	r.mu.Unlock()
	r.observe(m)
}

// convert builds the metering record from the gateway's, adding the
// cost. The Model's pricing is read through the catalog by the name the
// record resolved; a count route, an opaque route, a request that
// resolved no Model, and a Model without pricing are unpriced.
func (r *Recorder) convert(rec gateway.Record) metering.Record {
	tokens := metering.Tokens{
		Input: rec.Tokens.Input, Output: rec.Tokens.Output, CachedInput: rec.Tokens.CachedInput,
		CacheWrite: rec.Tokens.CacheWrite, Reasoning: rec.Tokens.Reasoning, Estimated: rec.Tokens.Estimated,
	}
	m := metering.Record{
		ID: rec.ID, At: rec.At.UTC(), EndedAt: rec.EndedAt.UTC(),
		Key:   metering.KeyRef{ID: rec.KeyID, Prefix: rec.KeyPrefix},
		Owner: rec.Owner,
		Model: metering.Ref{Name: rec.Model, ID: rec.ModelID}, Provider: metering.Ref{Name: rec.Provider, ID: rec.ProviderID},
		UpstreamModel: rec.UpstreamModel,
		Door:          rec.Door, TargetDialect: rec.TargetDialect, Route: rec.Route, Translated: rec.Translated,
		Loss:     append([]string{}, rec.Loss...),
		Attempts: make([]metering.Attempt, 0, len(rec.Attempts)),
		Status:   metering.Status(rec.Status), Error: string(rec.Error), UpstreamStatus: rec.UpstreamStatus,
		LatencyMs: rec.Latency.Milliseconds(), TTFBMs: rec.TTFB.Milliseconds(),
		Tokens: tokens, Cost: metering.Charged(tokens, r.pricing(rec)), Stream: rec.Stream,
		Labels: maps.Clone(rec.Labels), RequestLabels: maps.Clone(rec.RequestLabels),
	}
	for _, a := range rec.Attempts {
		m.Attempts = append(m.Attempts, metering.Attempt{
			Provider: a.Provider, UpstreamModel: a.UpstreamModel, Status: metering.Status(a.Status),
			HTTPStatus: a.HTTPStatus, Error: string(a.Error), DurationMs: a.Duration.Milliseconds(),
		})
	}
	if m.Labels == nil {
		m.Labels = map[string]string{}
	}
	if m.RequestLabels == nil {
		m.RequestLabels = map[string]string{}
	}
	return m
}

// pricing is the Model's pricing for rec, or nil where the record is
// unpriced by rule or the catalog does not answer.
func (r *Recorder) pricing(rec gateway.Record) *v1.Pricing {
	if rec.Class == gateway.ClassOpaque || countRoutes[rec.Route] || rec.Model == "" || r.o.Catalog == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pricingTimeout)
	defer cancel()
	m, err := r.o.Catalog.Model(ctx, rec.Model)
	if err != nil {
		r.o.Logger.ErrorContext(ctx, "metering: reading the Model's pricing; the record is unpriced", "request", rec.ID, "model", rec.Model, "err", err)
		return nil
	}
	if m == nil {
		return nil
	}
	return m.Spec.Pricing
}

// observe records the token and spend metrics of one record.
func (r *Recorder) observe(m metering.Record) {
	if r.tokens == nil {
		return
	}
	for direction, n := range map[string]int64{
		"input": m.Tokens.Input, "output": m.Tokens.Output, "cached_input": m.Tokens.CachedInput, "cache_write": m.Tokens.CacheWrite,
	} {
		if n > 0 {
			r.tokens.Add(map[string]string{"direction": direction}, uint64(n))
		}
	}
	if m.Cost.Priced && m.Cost.Amount > 0 {
		r.spend.Add(map[string]string{"currency": m.Cost.Currency}, uint64(m.Cost.Amount))
	}
}

// Flush upserts this replica's hourly deltas through Store.Usage() and
// notes the time. A store that cannot answer keeps the deltas for the
// next flush and is returned, and the lag gauge keeps growing.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	batch := make([]metering.Aggregate, 0, len(r.rows))
	for _, a := range r.rows {
		batch = append(batch, *a)
	}
	r.rows = map[metering.AggregateKey]*metering.Aggregate{}
	r.mu.Unlock()
	if len(batch) > 0 {
		if err := r.o.Store.Usage().AddRows(ctx, batch); err != nil {
			r.mu.Lock()
			for _, a := range batch {
				if have, ok := r.rows[a.Key()]; ok {
					have.Add(a.Sums)
					continue
				}
				a := a
				r.rows[a.Key()] = &a
			}
			r.mu.Unlock()
			return err
		}
	}
	r.mu.Lock()
	r.lastFlush = r.o.Now()
	r.mu.Unlock()
	return nil
}

// FlushLag is the time since this replica's last successful Flush of
// the aggregates.
func (r *Recorder) FlushLag() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return max(r.o.Now().Sub(r.lastFlush), 0)
}

// lag is what the gauge reads: the older of the Recorder's flush and
// the Limiter's, when one is watched.
func (r *Recorder) lag() time.Duration {
	lag := r.FlushLag()
	if r.o.Limiter != nil {
		lag = max(lag, r.o.Limiter.FlushLag())
	}
	return lag
}

// Pending is the number of hourly rows waiting for the next flush.
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// Run flushes on the interval until ctx ends, then once more on a
// context that outlives the cancellation, so a stopping replica leaves
// no aggregate behind. A failed flush is logged and its rows wait.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(r.o.Flush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.o.Flush)
			defer cancel()
			if err := r.Flush(last); err != nil {
				r.o.Logger.ErrorContext(last, "metering: the final flush of the aggregates", "rows", r.Pending(), "err", err)
			}
			return
		case <-t.C:
			if err := r.Flush(ctx); err != nil {
				r.o.Logger.ErrorContext(ctx, "metering: flushing the aggregates", "rows", r.Pending(), "err", err)
			}
		}
	}
}

// The seam: the doors take the Recorder through spec 004's interface.
var _ gateway.Recorder = (*Recorder)(nil)
