// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// MetricOperations is the counter every store method increments once,
// with op the method's name as Collection.Method and result one of the
// three below.
const MetricOperations = "lux_store_operations_total"

// The result label's closed set. ok is an answer, ErrNotFound included,
// because a lookup that finds nothing is the store working; conflict is
// a refusal the contract names, a stale version, a taken name or hash,
// a read-only mode, a foreign cursor; error is the store failing, which
// is the one value an alert reads.
const (
	ResultOK       = "ok"
	ResultConflict = "conflict"
	ResultError    = "error"
)

// SpanStore is the span of spec 019's table every store method opens
// under a lux.request or lux.api span, with the three attributes below;
// a method called with no span in its context, the jobs' reads and the
// flushes, opens none, so a trace is a request's and never a tick's.
const (
	SpanStore  = "lux.store"
	AttrOp     = "lux.op"
	AttrKind   = "lux.kind"
	AttrResult = "lux.result"
)

// tracerScope names this package as the instrumentation scope.
const tracerScope = "latere.ai/x/lux/internal/store"

// Instrument wraps s so every method of every collection, Transact, and
// Ready counts one MetricOperations on reg and, under a parent span,
// opens one SpanStore; the Store handed to a Transact's fn counts the
// same way. The registry is spec 019's one registry, constructed by the
// serve role and passed here rather than reached for, because the
// metrics package has no default on purpose.
func Instrument(s Store, reg *metrics.Registry) Store {
	return &instrumented{inner: s, counter: reg.Counter(MetricOperations, "Store operations by method and result.")}
}

// resultOf classifies an error into the result label.
func resultOf(err error) string {
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		return ResultOK
	case errors.Is(err, ErrVersionConflict), errors.Is(err, ErrNameTaken), errors.Is(err, ErrHashTaken),
		errors.Is(err, ErrReadOnly), errors.Is(err, ErrInvalidCursor), errors.Is(err, ErrKeyFenced), errors.Is(err, ErrFenceConflict):
		return ResultConflict
	default:
		return ResultError
	}
}

type instrumented struct {
	inner   Store
	counter *metrics.Counter
}

// begin opens one operation: the span when ctx carries a parent, and the
// count either way, recorded by the returned done with the operation's
// error.
func (s *instrumented) begin(ctx context.Context, op, kind string) (context.Context, func(error)) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, func(err error) { s.count(op, err) }
	}
	attrs := []attribute.KeyValue{attribute.String(AttrOp, op)}
	if kind != "" {
		attrs = append(attrs, attribute.String(AttrKind, kind))
	}
	ctx, span := otel.Tracer(tracerScope).Start(ctx, SpanStore, trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		result := resultOf(err)
		s.counter.Inc(map[string]string{"op": op, "result": result})
		span.SetAttributes(attribute.String(AttrResult, result))
		span.End()
	}
}

func (s *instrumented) count(op string, err error) {
	s.counter.Inc(map[string]string{"op": op, "result": resultOf(err)})
}

// kindOf is an object's kind for the span, empty for a nil object.
func kindOf(obj v1.Object) string {
	if obj == nil {
		return ""
	}
	return obj.Kind()
}

func (s *instrumented) Objects() Objects         { return iObjects{s.inner.Objects(), s} }
func (s *instrumented) KeyFences() KeyFences     { return iKeyFences{s.inner.KeyFences(), s} }
func (s *instrumented) Keys() Keys               { return iKeys{s.inner.Keys(), s} }
func (s *instrumented) Credentials() Credentials { return iCredentials{s.inner.Credentials(), s} }
func (s *instrumented) Counters() Counters       { return iCounters{s.inner.Counters(), s} }
func (s *instrumented) Leases() Leases           { return iLeases{s.inner.Leases(), s} }
func (s *instrumented) Journal() Journal         { return iJournal{s.inner.Journal(), s} }
func (s *instrumented) Tunnels() Tunnels         { return iTunnels{s.inner.Tunnels(), s} }
func (s *instrumented) Usage() Usage             { return iUsage{s.inner.Usage(), s} }

func (s *instrumented) Transact(ctx context.Context, fn func(tx Store) error) error {
	ctx, done := s.begin(ctx, "Transact", "")
	err := s.inner.Transact(ctx, func(tx Store) error {
		return fn(&instrumented{inner: tx, counter: s.counter})
	})
	done(err)
	return err
}

func (s *instrumented) Ready(ctx context.Context) error {
	ctx, done := s.begin(ctx, "Ready", "")
	err := s.inner.Ready(ctx)
	done(err)
	return err
}

func (s *instrumented) Close() error { return s.inner.Close() }

type iObjects struct {
	Objects
	s *instrumented
}

func (o iObjects) Put(ctx context.Context, obj v1.Object, ifVersion int64) (int64, error) {
	ctx, done := o.s.begin(ctx, "Objects.Put", kindOf(obj))
	v, err := o.Objects.Put(ctx, obj, ifVersion)
	done(err)
	return v, err
}

func (o iObjects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	ctx, done := o.s.begin(ctx, "Objects.Get", kind)
	obj, v, err := o.Objects.Get(ctx, kind, id)
	done(err)
	return obj, v, err
}

func (o iObjects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	ctx, done := o.s.begin(ctx, "Objects.ByName", kind)
	obj, v, err := o.Objects.ByName(ctx, kind, name)
	done(err)
	return obj, v, err
}

func (o iObjects) List(ctx context.Context, kind string, f Filter, p Page) ([]v1.Object, string, error) {
	ctx, done := o.s.begin(ctx, "Objects.List", kind)
	objs, next, err := o.Objects.List(ctx, kind, f, p)
	done(err)
	return objs, next, err
}

func (o iObjects) Delete(ctx context.Context, kind, id string) error {
	ctx, done := o.s.begin(ctx, "Objects.Delete", kind)
	err := o.Objects.Delete(ctx, kind, id)
	done(err)
	return err
}

func (o iObjects) PutStatus(ctx context.Context, kind, id string, observed any) error {
	ctx, done := o.s.begin(ctx, "Objects.PutStatus", kind)
	err := o.Objects.PutStatus(ctx, kind, id, observed)
	done(err)
	return err
}

func (o iObjects) Prune(ctx context.Context, before time.Time) (int, error) {
	ctx, done := o.s.begin(ctx, "Objects.Prune", "")
	n, err := o.Objects.Prune(ctx, before)
	done(err)
	return n, err
}

type iKeys struct {
	Keys
	s *instrumented
}

func (k iKeys) Put(ctx context.Context, keyID, hash string) error {
	ctx, done := k.s.begin(ctx, "Keys.Put", "")
	err := k.Keys.Put(ctx, keyID, hash)
	done(err)
	return err
}

func (k iKeys) ByHash(ctx context.Context, hash string) (string, error) {
	ctx, done := k.s.begin(ctx, "Keys.ByHash", "")
	id, err := k.Keys.ByHash(ctx, hash)
	done(err)
	return id, err
}

func (k iKeys) Delete(ctx context.Context, keyID string) error {
	ctx, done := k.s.begin(ctx, "Keys.Delete", "")
	err := k.Keys.Delete(ctx, keyID)
	done(err)
	return err
}

type iCredentials struct {
	Credentials
	s *instrumented
}

func (c iCredentials) Put(ctx context.Context, providerID string, sealed Sealed) error {
	ctx, done := c.s.begin(ctx, "Credentials.Put", "")
	err := c.Credentials.Put(ctx, providerID, sealed)
	done(err)
	return err
}

func (c iCredentials) Rewrap(ctx context.Context, providerID string, ifVersion int, wrappedKey, wrappedNonce []byte) error {
	ctx, done := c.s.begin(ctx, "Credentials.Rewrap", "")
	err := c.Credentials.Rewrap(ctx, providerID, ifVersion, wrappedKey, wrappedNonce)
	done(err)
	return err
}

func (c iCredentials) Get(ctx context.Context, providerID string) (Sealed, error) {
	ctx, done := c.s.begin(ctx, "Credentials.Get", "")
	sealed, err := c.Credentials.Get(ctx, providerID)
	done(err)
	return sealed, err
}

func (c iCredentials) Delete(ctx context.Context, providerID string) error {
	ctx, done := c.s.begin(ctx, "Credentials.Delete", "")
	err := c.Credentials.Delete(ctx, providerID)
	done(err)
	return err
}

func (c iCredentials) List(ctx context.Context) ([]string, error) {
	ctx, done := c.s.begin(ctx, "Credentials.List", "")
	ids, err := c.Credentials.List(ctx)
	done(err)
	return ids, err
}

type iCounters struct {
	Counters
	s *instrumented
}

func (c iCounters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	ctx, done := c.s.begin(ctx, "Counters.Add", "")
	total, err := c.Counters.Add(ctx, key, delta, expiresAt)
	done(err)
	return total, err
}

func (c iCounters) Read(ctx context.Context, keys []string) (map[string]int64, error) {
	ctx, done := c.s.begin(ctx, "Counters.Read", "")
	out, err := c.Counters.Read(ctx, keys)
	done(err)
	return out, err
}

func (c iCounters) Prune(ctx context.Context, before time.Time) (int, error) {
	ctx, done := c.s.begin(ctx, "Counters.Prune", "")
	n, err := c.Counters.Prune(ctx, before)
	done(err)
	return n, err
}

type iLeases struct {
	Leases
	s *instrumented
}

func (l iLeases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	ctx, done := l.s.begin(ctx, "Leases.Acquire", "")
	held, err := l.Leases.Acquire(ctx, name, holder, ttl)
	done(err)
	return held, err
}

func (l iLeases) Release(ctx context.Context, name, holder string) error {
	ctx, done := l.s.begin(ctx, "Leases.Release", "")
	err := l.Leases.Release(ctx, name, holder)
	done(err)
	return err
}

type iJournal struct {
	Journal
	s *instrumented
}

func (j iJournal) Append(ctx context.Context, e Event) (int64, error) {
	ctx, done := j.s.begin(ctx, "Journal.Append", "")
	seq, err := j.Journal.Append(ctx, e)
	done(err)
	return seq, err
}

func (j iJournal) Pending(ctx context.Context, limit int) ([]Event, error) {
	ctx, done := j.s.begin(ctx, "Journal.Pending", "")
	events, err := j.Journal.Pending(ctx, limit)
	done(err)
	return events, err
}

func (j iJournal) Acknowledge(ctx context.Context, id string) error {
	ctx, done := j.s.begin(ctx, "Journal.Acknowledge", "")
	err := j.Journal.Acknowledge(ctx, id)
	done(err)
	return err
}

func (j iJournal) Defer(ctx context.Context, id string, attempts int, next time.Time) error {
	ctx, done := j.s.begin(ctx, "Journal.Defer", "")
	err := j.Journal.Defer(ctx, id, attempts, next)
	done(err)
	return err
}

func (j iJournal) Drop(ctx context.Context, id string) error {
	ctx, done := j.s.begin(ctx, "Journal.Drop", "")
	err := j.Journal.Drop(ctx, id)
	done(err)
	return err
}

func (j iJournal) ByObject(ctx context.Context, objectID string, p Page) ([]Event, string, error) {
	ctx, done := j.s.begin(ctx, "Journal.ByObject", "")
	events, next, err := j.Journal.ByObject(ctx, objectID, p)
	done(err)
	return events, next, err
}

func (j iJournal) Since(ctx context.Context, afterGSeq int64, limit int) ([]Event, error) {
	ctx, done := j.s.begin(ctx, "Journal.Since", "")
	events, err := j.Journal.Since(ctx, afterGSeq, limit)
	done(err)
	return events, err
}

func (j iJournal) Prune(ctx context.Context, before time.Time) (int, error) {
	ctx, done := j.s.begin(ctx, "Journal.Prune", "")
	n, err := j.Journal.Prune(ctx, before)
	done(err)
	return n, err
}

type iTunnels struct {
	Tunnels
	s *instrumented
}

func (t iTunnels) Register(ctx context.Context, row Tunnel, ttl time.Duration) error {
	ctx, done := t.s.begin(ctx, "Tunnels.Register", "")
	err := t.Tunnels.Register(ctx, row, ttl)
	done(err)
	return err
}

func (t iTunnels) Heartbeat(ctx context.Context, providerID, session string, ttl time.Duration) (bool, error) {
	ctx, done := t.s.begin(ctx, "Tunnels.Heartbeat", "")
	held, err := t.Tunnels.Heartbeat(ctx, providerID, session, ttl)
	done(err)
	return held, err
}

func (t iTunnels) Get(ctx context.Context, providerID string) (Tunnel, error) {
	ctx, done := t.s.begin(ctx, "Tunnels.Get", "")
	row, err := t.Tunnels.Get(ctx, providerID)
	done(err)
	return row, err
}

func (t iTunnels) Unregister(ctx context.Context, providerID, session string) error {
	ctx, done := t.s.begin(ctx, "Tunnels.Unregister", "")
	err := t.Tunnels.Unregister(ctx, providerID, session)
	done(err)
	return err
}

type iUsage struct {
	Usage
	s *instrumented
}

func (u iUsage) AddRows(ctx context.Context, rows []metering.Aggregate) error {
	ctx, done := u.s.begin(ctx, "Usage.AddRows", "")
	err := u.Usage.AddRows(ctx, rows)
	done(err)
	return err
}

func (u iUsage) QueryRows(ctx context.Context, q metering.Query) ([]metering.Row, error) {
	ctx, done := u.s.begin(ctx, "Usage.QueryRows", "")
	rows, err := u.Usage.QueryRows(ctx, q)
	done(err)
	return rows, err
}

func (u iUsage) AppendRecord(ctx context.Context, r metering.Record) error {
	ctx, done := u.s.begin(ctx, "Usage.AppendRecord", "")
	err := u.Usage.AppendRecord(ctx, r)
	done(err)
	return err
}

func (u iUsage) Records(ctx context.Context, q metering.RecordQuery, p Page) ([]metering.Record, string, error) {
	ctx, done := u.s.begin(ctx, "Usage.Records", "")
	recs, next, err := u.Usage.Records(ctx, q, p)
	done(err)
	return recs, next, err
}

func (u iUsage) Hourly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	ctx, done := u.s.begin(ctx, "Usage.Hourly", "")
	rows, err := u.Usage.Hourly(ctx, before, after, limit)
	done(err)
	return rows, err
}

func (u iUsage) Monthly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	ctx, done := u.s.begin(ctx, "Usage.Monthly", "")
	rows, err := u.Usage.Monthly(ctx, before, after, limit)
	done(err)
	return rows, err
}

func (u iUsage) Fold(ctx context.Context, moves []metering.Move, expired []metering.AggregateKey) (int, int, error) {
	ctx, done := u.s.begin(ctx, "Usage.Fold", "")
	folded, deleted, err := u.Usage.Fold(ctx, moves, expired)
	done(err)
	return folded, deleted, err
}

func (u iUsage) RedactOwner(ctx context.Context, owner string) (int, error) {
	ctx, done := u.s.begin(ctx, "Usage.RedactOwner", "")
	n, err := u.Usage.RedactOwner(ctx, owner)
	done(err)
	return n, err
}

type iKeyFences struct {
	KeyFences
	s *instrumented
}

func (f iKeyFences) Put(ctx context.Context, input KeyFence) (KeyFence, bool, error) {
	ctx, done := f.s.begin(ctx, "KeyFences.Put", "")
	result, inserted, err := f.KeyFences.Put(ctx, input)
	done(err)
	return result, inserted, err
}
func (f iKeyFences) Get(ctx context.Context, name string) (KeyFence, error) {
	ctx, done := f.s.begin(ctx, "KeyFences.Get", "")
	result, err := f.KeyFences.Get(ctx, name)
	done(err)
	return result, err
}
