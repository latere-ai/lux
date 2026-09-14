// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/retry"
	"latere.ai/x/pkg/s3"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The figures of spec 012's batching table.
const (
	// FlushSize is the records one object holds at most.
	FlushSize = 5000
	// FlushInterval is how long a partial batch waits.
	FlushInterval = 30 * time.Second
	// BufferCap is the ring's capacity, ten flushes of headroom.
	BufferCap = 50_000
	// ContentType is what every archive object is uploaded as.
	ContentType = "application/x-ndjson"
	// DefaultPrefix is LUX_S3_PREFIX's default.
	DefaultPrefix = "lux/"
	// MetricDropped is spec 019's counter: records dropped from the ring
	// because the bucket did not keep up.
	MetricDropped = "lux_requestlog_dropped_total"
	// DrainTimeout bounds the final flush at stop.
	DrainTimeout = 30 * time.Second
	// warnEvery spaces the WARN lines about drops.
	warnEvery = time.Minute
)

// WritePolicy is the retry of one PutObject: five attempts from a second
// to a minute, each bounded to thirty seconds, given to the client
// through s3.WithRetry.
var WritePolicy = retry.Policy{MaxAttempts: 5, Base: time.Second, Max: time.Minute, Timeout: 30 * time.Second}

// Bucket is the seam over latere.ai/x/pkg/s3's client: the three calls
// this package makes, which *s3.Client satisfies.
type Bucket interface {
	PutObject(ctx context.Context, key string, body s3.Body) (string, error)
	ListObjects(ctx context.Context, opts s3.ListOptions) (s3.ListResult, error)
	GetObject(ctx context.Context, key, ifNoneMatch string) (io.ReadCloser, s3.Object, error)
}

// ExporterOptions is what the Exporter runs under.
type ExporterOptions struct {
	// Bucket receives the objects; required.
	Bucket Bucket
	// Prefix is LUX_S3_PREFIX; empty is DefaultPrefix.
	Prefix string
	// Replica names this process in every object key; empty is the
	// hostname reduced by Replica.
	Replica string
	// FlushSize, FlushInterval, and Cap replace the figures above when
	// positive.
	FlushSize     int
	FlushInterval time.Duration
	Cap           int
	// Metrics receives MetricDropped; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints the ULID of an object key from the clock; nil mints
	// through v1.NewID.
	NewID func(now time.Time) string
}

// Exporter is the archive side of the Recorder: Append is the
// non-blocking send from the request path, Run the worker that writes.
type Exporter struct {
	o       ExporterOptions
	ring    *ring
	dropped *metrics.Counter
	wake    chan struct{}

	mu        sync.Mutex
	drops     uint64    // every record dropped since start
	unwarned  uint64    // drops since the last WARN line
	lastWarn  time.Time // when the last WARN line was written
	warnFirst bool      // whether a WARN line was ever written
}

// NewExporter constructs the exporter; Run starts its flushes.
func NewExporter(o ExporterOptions) *Exporter {
	if o.Prefix == "" {
		o.Prefix = DefaultPrefix
	}
	if o.Replica == "" {
		host, _ := os.Hostname()
		o.Replica = Replica(host)
	}
	if o.FlushSize <= 0 {
		o.FlushSize = FlushSize
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = FlushInterval
	}
	if o.Cap <= 0 {
		o.Cap = BufferCap
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = func(now time.Time) string { return v1.NewID("", now, nil) }
	}
	e := &Exporter{o: o, ring: newRing(o.Cap), wake: make(chan struct{}, 1)}
	if o.Metrics != nil {
		e.dropped = o.Metrics.Counter(MetricDropped, droppedHelp)
		e.dropped.Add(nil, 0) // a zero series from the start, so the metric table holds before the first drop
	}
	return e
}

// Append hands one record to the ring without blocking: the oldest
// record is dropped when the ring is at its cap, counted, and said once
// a minute; a ring at the flush size wakes the worker.
func (e *Exporter) Append(r metering.Record) {
	if e.ring.push(r) {
		e.countDrops(1)
	}
	if e.ring.len() >= e.o.FlushSize {
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
}

// countDrops adds n to the counter and writes the WARN line when a
// minute has passed since the last.
func (e *Exporter) countDrops(n int) {
	if e.dropped != nil {
		e.dropped.Add(nil, uint64(n))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.drops += uint64(n)
	e.unwarned += uint64(n)
	now := e.o.Now()
	if e.warnFirst && now.Sub(e.lastWarn) < warnEvery {
		return
	}
	e.o.Logger.Warn("requestlog: dropped the oldest records; the buffer is at its cap and the archive is not keeping up", "dropped", e.unwarned, "cap", e.o.Cap, "total", e.drops)
	e.unwarned, e.lastWarn, e.warnFirst = 0, now, true
}

// Dropped is every record dropped since start.
func (e *Exporter) Dropped() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drops
}

// Len is how many records wait in the ring.
func (e *Exporter) Len() int { return e.ring.len() }

// Flush writes one batch, the oldest records up to the flush size, as
// one object, or nothing when the ring is empty. The batch stays in the
// ring while it is written and is removed once the bucket has it, so a
// write that fails after the client's attempts leaves it at the head for
// the next flush and returns the error.
func (e *Exporter) Flush(ctx context.Context) error {
	batch, through := e.ring.peek(e.o.FlushSize)
	if len(batch) == 0 {
		return nil
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, r := range batch {
		if err := enc.Encode(r); err != nil {
			// A Record is scalars, slices, and string maps, so this does
			// not happen; the batch is dropped rather than retried into
			// the same error forever.
			e.ring.ack(through)
			e.countDrops(len(batch))
			return fmt.Errorf("encoding the batch: %w", err)
		}
	}
	key := objectKey(e.o.Prefix, e.o.Replica, batch[0].At, e.o.NewID(e.o.Now()))
	b := s3.BytesBody(body.Bytes())
	b.ContentType = ContentType
	if _, err := e.o.Bucket.PutObject(ctx, key, b); err != nil {
		return fmt.Errorf("writing %s with %d records: %w", key, len(batch), err)
	}
	e.ring.ack(through)
	return nil
}

// Run flushes a full batch when the ring reaches the flush size and a
// partial one on the interval, until ctx ends, then drains the ring on
// a context that outlives the cancellation, bounded by DrainTimeout. A
// failed write is logged and its batch waits for the next flush.
func (e *Exporter) Run(ctx context.Context) {
	t := time.NewTicker(e.o.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.drain(ctx)
			return
		case <-e.wake:
			for e.ring.len() >= e.o.FlushSize {
				if err := e.Flush(ctx); err != nil {
					e.o.Logger.WarnContext(ctx, "requestlog: writing a batch; it stays in the buffer for the next flush", "waiting", e.ring.len(), "err", err)
					break
				}
			}
		case <-t.C:
			if err := e.Flush(ctx); err != nil {
				e.o.Logger.WarnContext(ctx, "requestlog: writing a batch; it stays in the buffer for the next flush", "waiting", e.ring.len(), "err", err)
			}
		}
	}
}

// drain writes what the ring holds, batch by batch, at stop.
func (e *Exporter) drain(ctx context.Context) {
	last, cancel := context.WithTimeout(context.WithoutCancel(ctx), DrainTimeout)
	defer cancel()
	for e.ring.len() > 0 {
		if err := e.Flush(last); err != nil {
			e.o.Logger.ErrorContext(last, "requestlog: the final flush at stop; the records still in the buffer are lost", "lost", e.ring.len(), "err", err)
			return
		}
	}
}

// objectKey is <prefix>/yyyy/mm/dd/hh/<replica>-<ulid>.ndjson, the hour
// that of first in UTC.
func objectKey(prefix, replica string, first time.Time, ulid string) string {
	return path.Join(prefix, hourPrefix(first), replica+"-"+ulid+".ndjson")
}

// hourPrefix is the yyyy/mm/dd/hh of t in UTC.
func hourPrefix(t time.Time) string {
	return t.UTC().Format("2006/01/02/15")
}

// Replica reduces a hostname to the [a-z0-9-] it appears as in a key:
// lowered, every other character a dash, the ends trimmed, and replica
// when nothing is left.
func Replica(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "replica"
	}
	return out
}

// droppedHelp is MetricDropped's help text, one string for the exporter
// and the idle registration.
const droppedHelp = "Request log records dropped because the buffer was at its cap while the archive did not keep up."

// RegisterIdle registers MetricDropped at zero, for a process with no
// exporter configured, so the registry carries every metric of the table
// whether or not the archive is on. A process with an Exporter must not
// call it, since the Exporter registers the counter itself.
func RegisterIdle(reg *metrics.Registry) {
	reg.Counter(MetricDropped, droppedHelp).Add(nil, 0)
}
