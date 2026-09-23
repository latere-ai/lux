// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/retry"

	"latere.ai/x/lux/internal/store"
)

// The delivery figures of spec 012.
const (
	// GiveUpAfter is how long a row is retried from its At before it is
	// dropped.
	GiveUpAfter = 24 * time.Hour
	// MetricPending is spec 019's gauge: the unacknowledged rows, read
	// by the holder of the journal lease at each tick.
	MetricPending = "lux_events_pending"
	// DefaultPoll is how often the holder reads the pending rows.
	DefaultPoll = time.Second
	// DefaultBatch is how many due rows one tick delivers.
	DefaultBatch = 100
)

// DeliveryPolicy is the backoff of a failed delivery: from a second to
// five minutes with full jitter, the delay of the attempt written into
// the row's next attempt through Journal.Defer.
var DeliveryPolicy = retry.Policy{Base: time.Second, Max: 5 * time.Minute, Jitter: 1}

// sinceKey is the counter that records when the sink was first set, as
// unix seconds, so every holder after a restart acknowledges the same
// rows unsent.
const sinceKey = "events:sink:since"

// leaseRenew is how often the holder renews: a third of the TTL.
const leaseRenew = store.LeaseTTL / 3

// WorkerOptions is what the Worker runs under.
type WorkerOptions struct {
	// Store holds the journal the worker delivers, the lease it runs
	// under, and the counter that remembers when the sink was set.
	Store store.Store
	// Sink is the operator's endpoint.
	Sink *Sink
	// Holder names this replica in the lease row; empty is the host and
	// the process id.
	Holder string
	// Poll is how often Run reads the pending rows; zero is DefaultPoll.
	Poll time.Duration
	// Batch is how many due rows one tick delivers; zero is DefaultBatch.
	Batch int
	// Policy is the backoff; the zero value is DeliveryPolicy.
	Policy retry.Policy
	// GiveUp is how long a row is retried from its At; zero is
	// GiveUpAfter.
	GiveUp time.Duration
	// Metrics receives MetricPending; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Worker is the delivery worker of spec 012 on one replica. Under the
// journal lease it reads Journal.Pending on the poll, at most one due
// row per object and oldest first, posts each to the Sink, acknowledges
// a 2xx, defers a failure on the policy, and drops a row that has failed
// for GiveUp. A row whose At is before the moment the sink was first
// set is acknowledged without a POST.
type Worker struct {
	o WorkerOptions

	mu      sync.Mutex
	held    bool
	since   time.Time // the moment the sink was first set, once read
	pending int       // the unacknowledged rows as of the last tick
}

// NewWorker constructs the worker; Run starts it.
func NewWorker(o WorkerOptions) *Worker {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Holder == "" {
		o.Holder = defaultHolder()
	}
	if o.Poll <= 0 {
		o.Poll = DefaultPoll
	}
	if o.Batch <= 0 {
		o.Batch = DefaultBatch
	}
	if o.Policy == (retry.Policy{}) {
		o.Policy = DeliveryPolicy
	}
	if o.GiveUp <= 0 {
		o.GiveUp = GiveUpAfter
	}
	w := &Worker{o: o}
	if o.Metrics != nil {
		o.Metrics.Gauge(MetricPending, pendingHelp, func() []metrics.LabeledValue {
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.held {
				return nil
			}
			return []metrics.LabeledValue{{Labels: map[string]string{}, Value: float64(w.pending)}}
		})
	}
	return w
}

// Run acquires and renews the lease, ticks on the poll, and releases the
// lease when ctx ends.
func (w *Worker) Run(ctx context.Context) {
	w.acquire(ctx)
	w.Tick(ctx)
	renew := time.NewTicker(leaseRenew)
	defer renew.Stop()
	poll := time.NewTicker(w.o.Poll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			w.release(ctx)
			return
		case <-renew.C:
			w.acquire(ctx)
		case <-poll.C:
			w.Tick(ctx)
		}
	}
}

// acquire takes or renews the lease and records whether it is held.
func (w *Worker) acquire(ctx context.Context) {
	held, err := w.o.Store.Leases().Acquire(ctx, store.LeaseJournal, w.o.Holder, store.LeaseTTL)
	if err != nil {
		w.o.Logger.ErrorContext(ctx, "events: acquiring the lease", "holder", w.o.Holder, "err", err)
		held = false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held = held
}

// release gives the lease back on a clean stop, so the next holder does
// not wait out the TTL, on a context that outlives the stop.
func (w *Worker) release(ctx context.Context) {
	w.mu.Lock()
	held := w.held
	w.held = false
	w.mu.Unlock()
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := w.o.Store.Leases().Release(ctx, store.LeaseJournal, w.o.Holder); err != nil {
		w.o.Logger.ErrorContext(ctx, "events: releasing the lease", "holder", w.o.Holder, "err", err)
	}
}

// Held reports whether this replica holds the lease as of its last
// acquire.
func (w *Worker) Held() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.held
}

// Pending is the count of unacknowledged rows as of the last tick, what
// the gauge reads.
func (w *Worker) Pending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending
}

// Tick delivers one batch of due rows when this replica holds the lease,
// then counts what is still unacknowledged.
func (w *Worker) Tick(ctx context.Context) {
	if !w.Held() {
		return
	}
	since, ok := w.sinceSet(ctx)
	if !ok {
		return
	}
	rows, err := w.o.Store.Journal().Pending(ctx, w.o.Batch)
	if err != nil {
		w.o.Logger.ErrorContext(ctx, "events: reading the pending rows", "err", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		w.deliver(ctx, row, since)
	}
	w.count(ctx)
}

// deliver posts one row and settles it: acknowledged on a 2xx or when
// it predates the sink, deferred on the policy after a failure, dropped
// once it has failed for GiveUp.
func (w *Worker) deliver(ctx context.Context, row store.Event, since time.Time) {
	j := w.o.Store.Journal()
	if row.At.Before(since) {
		if err := j.Acknowledge(ctx, row.ID); err != nil {
			w.o.Logger.ErrorContext(ctx, "events: acknowledging a row from before the sink was set", "id", row.ID, "type", row.Type, "err", err)
			return
		}
		w.o.Logger.InfoContext(ctx, "events: acknowledged unsent; the row predates the sink", "id", row.ID, "type", row.Type, "at", row.At, "since", since)
		return
	}
	err := w.o.Sink.Deliver(ctx, row.Payload)
	if err == nil {
		if err := j.Acknowledge(ctx, row.ID); err != nil {
			w.o.Logger.ErrorContext(ctx, "events: acknowledging a delivered event; it is delivered again", "id", row.ID, "type", row.Type, "err", err)
		}
		return
	}
	attempts := row.Attempts + 1
	now := w.o.Now()
	if now.Sub(row.At) >= w.o.GiveUp {
		if err := j.Drop(ctx, row.ID); err != nil {
			w.o.Logger.ErrorContext(ctx, "events: dropping an event that failed for the whole retry window", "id", row.ID, "type", row.Type, "err", err)
			return
		}
		w.o.Logger.ErrorContext(ctx, "events: dropped an event; the sink did not acknowledge it within the retry window", "id", row.ID, "type", row.Type, "object", row.ObjectID, "attempts", attempts, "since", row.At, "window", w.o.GiveUp, "err", err)
		return
	}
	next := now.Add(w.o.Policy.Delay(attempts))
	if err := j.Defer(ctx, row.ID, attempts, next); err != nil {
		w.o.Logger.ErrorContext(ctx, "events: deferring a failed delivery", "id", row.ID, "type", row.Type, "err", err)
		return
	}
	w.o.Logger.WarnContext(ctx, "events: delivery failed; retrying", "id", row.ID, "type", row.Type, "object", row.ObjectID, "attempt", attempts, "next", next, "err", err)
}

// sinceSet is the moment the sink was first set, read once from the
// counter and remembered. The first holder to run with a sink adds its
// clock to the empty counter and reads its own value back; a holder that
// finds the row reads it. Two holders adding at once, which the lease
// allows only while a stalled holder's row lapses, read a sum: the one
// that did not see its own value back subtracts what it added and takes
// the other's, and a reader that sees a sum in between, a time past
// tomorrow, delivers nothing this tick and reads the row at the next. ok
// is false when the store did not answer or the row is not yet settled.
func (w *Worker) sinceSet(ctx context.Context) (since time.Time, ok bool) {
	w.mu.Lock()
	since = w.since
	w.mu.Unlock()
	if !since.IsZero() {
		return since, true
	}
	counters := w.o.Store.Counters()
	now := w.o.Now().UTC().Truncate(time.Second)
	settled := func(v int64) (time.Time, bool) {
		if v <= 0 || v > now.Add(24*time.Hour).Unix() {
			w.o.Logger.WarnContext(ctx, "events: the moment the sink was set is not settled; another holder is writing it, so nothing is delivered this tick", "value", v, "now", now)
			return time.Time{}, false
		}
		since := time.Unix(v, 0).UTC()
		w.remember(since)
		return since, true
	}
	have, err := counters.Read(ctx, []string{sinceKey})
	if err != nil {
		w.o.Logger.ErrorContext(ctx, "events: reading when the sink was set", "err", err)
		return time.Time{}, false
	}
	if v := have[sinceKey]; v > 0 {
		return settled(v)
	}
	total, err := counters.Add(ctx, sinceKey, now.Unix(), time.Time{})
	if err != nil {
		w.o.Logger.ErrorContext(ctx, "events: recording the moment the sink was set", "err", err)
		return time.Time{}, false
	}
	if total == now.Unix() {
		w.o.Logger.InfoContext(ctx, "events: the sink is set from now; rows journalled before are acknowledged unsent", "since", now, "sink", w.o.Sink.o.URL)
		w.remember(now)
		return now, true
	}
	if _, err := counters.Add(ctx, sinceKey, -now.Unix(), time.Time{}); err != nil {
		w.o.Logger.ErrorContext(ctx, "events: withdrawing this holder's moment after another holder set the sink first", "err", err)
		return time.Time{}, false
	}
	return settled(total - now.Unix())
}

func (w *Worker) remember(since time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.since = since
}

// count reads the journal in pages and notes how many rows are not yet
// acknowledged, the value of the pending gauge.
func (w *Worker) count(ctx context.Context) {
	const page = 500
	n, after := 0, int64(0)
	for {
		rows, err := w.o.Store.Journal().Since(ctx, after, page)
		if err != nil {
			w.o.Logger.ErrorContext(ctx, "events: counting the pending rows", "err", err)
			return
		}
		for _, r := range rows {
			if r.AckedAt.IsZero() {
				n++
			}
		}
		if len(rows) < page {
			break
		}
		after = rows[len(rows)-1].GSeq
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = n
}

// defaultHolder is the lease holder's name when none is given: the host,
// which is what an operator reading a lease row recognizes, and the
// process, so two replicas on one host differ.
func defaultHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "replica"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
}

// pendingHelp is MetricPending's help text, one string for the worker and
// the idle registration.
const pendingHelp = "Events journalled and not yet acknowledged by the sink, as the lease holder counts them."

// RegisterIdle registers MetricPending reading zero, for a process with no
// sink configured, so the registry carries every metric of the table
// whether or not delivery is on. A process with a Worker must not call it,
// since the Worker registers the gauge itself.
func RegisterIdle(reg *metrics.Registry) {
	reg.Gauge(MetricPending, pendingHelp, func() []metrics.LabeledValue {
		return []metrics.LabeledValue{{Labels: map[string]string{}, Value: 0}}
	})
}
