// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
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

// Instrument wraps s so every method of every collection, Transact, and
// Ready counts one MetricOperations on reg, and the Store handed to a
// Transact's fn counts the same way. The registry is spec 019's one
// registry, constructed by the serve role and passed here rather than
// reached for, because the metrics package has no default on purpose.
func Instrument(s Store, reg *metrics.Registry) Store {
	return &instrumented{inner: s, counter: reg.Counter(MetricOperations, "Store operations by method and result.")}
}

// resultOf classifies an error into the result label.
func resultOf(err error) string {
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		return ResultOK
	case errors.Is(err, ErrVersionConflict), errors.Is(err, ErrNameTaken), errors.Is(err, ErrHashTaken),
		errors.Is(err, ErrReadOnly), errors.Is(err, ErrInvalidCursor):
		return ResultConflict
	default:
		return ResultError
	}
}

type instrumented struct {
	inner   Store
	counter *metrics.Counter
}

func (s *instrumented) count(op string, err error) {
	s.counter.Inc(map[string]string{"op": op, "result": resultOf(err)})
}

func (s *instrumented) Objects() Objects         { return iObjects{s.inner.Objects(), s} }
func (s *instrumented) Keys() Keys               { return iKeys{s.inner.Keys(), s} }
func (s *instrumented) Credentials() Credentials { return iCredentials{s.inner.Credentials(), s} }
func (s *instrumented) Counters() Counters       { return iCounters{s.inner.Counters(), s} }
func (s *instrumented) Leases() Leases           { return iLeases{s.inner.Leases(), s} }
func (s *instrumented) Journal() Journal         { return iJournal{s.inner.Journal(), s} }
func (s *instrumented) Tunnels() Tunnels         { return iTunnels{s.inner.Tunnels(), s} }

func (s *instrumented) Transact(ctx context.Context, fn func(tx Store) error) error {
	err := s.inner.Transact(ctx, func(tx Store) error {
		return fn(&instrumented{inner: tx, counter: s.counter})
	})
	s.count("Transact", err)
	return err
}

func (s *instrumented) Ready(ctx context.Context) error {
	err := s.inner.Ready(ctx)
	s.count("Ready", err)
	return err
}

func (s *instrumented) Close() error { return s.inner.Close() }

type iObjects struct {
	Objects
	s *instrumented
}

func (o iObjects) Put(ctx context.Context, obj v1.Object, ifVersion int64) (int64, error) {
	v, err := o.Objects.Put(ctx, obj, ifVersion)
	o.s.count("Objects.Put", err)
	return v, err
}

func (o iObjects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	obj, v, err := o.Objects.Get(ctx, kind, id)
	o.s.count("Objects.Get", err)
	return obj, v, err
}

func (o iObjects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	obj, v, err := o.Objects.ByName(ctx, kind, name)
	o.s.count("Objects.ByName", err)
	return obj, v, err
}

func (o iObjects) List(ctx context.Context, kind string, f Filter, p Page) ([]v1.Object, string, error) {
	objs, next, err := o.Objects.List(ctx, kind, f, p)
	o.s.count("Objects.List", err)
	return objs, next, err
}

func (o iObjects) Delete(ctx context.Context, kind, id string) error {
	err := o.Objects.Delete(ctx, kind, id)
	o.s.count("Objects.Delete", err)
	return err
}

func (o iObjects) PutStatus(ctx context.Context, kind, id string, observed any) error {
	err := o.Objects.PutStatus(ctx, kind, id, observed)
	o.s.count("Objects.PutStatus", err)
	return err
}

func (o iObjects) Prune(ctx context.Context, before time.Time) (int, error) {
	n, err := o.Objects.Prune(ctx, before)
	o.s.count("Objects.Prune", err)
	return n, err
}

type iKeys struct {
	Keys
	s *instrumented
}

func (k iKeys) Put(ctx context.Context, keyID, hash string) error {
	err := k.Keys.Put(ctx, keyID, hash)
	k.s.count("Keys.Put", err)
	return err
}

func (k iKeys) ByHash(ctx context.Context, hash string) (string, error) {
	id, err := k.Keys.ByHash(ctx, hash)
	k.s.count("Keys.ByHash", err)
	return id, err
}

func (k iKeys) Delete(ctx context.Context, keyID string) error {
	err := k.Keys.Delete(ctx, keyID)
	k.s.count("Keys.Delete", err)
	return err
}

type iCredentials struct {
	Credentials
	s *instrumented
}

func (c iCredentials) Put(ctx context.Context, providerID string, sealed Sealed) error {
	err := c.Credentials.Put(ctx, providerID, sealed)
	c.s.count("Credentials.Put", err)
	return err
}

func (c iCredentials) Rewrap(ctx context.Context, providerID string, ifVersion int, wrappedKey, wrappedNonce []byte) error {
	err := c.Credentials.Rewrap(ctx, providerID, ifVersion, wrappedKey, wrappedNonce)
	c.s.count("Credentials.Rewrap", err)
	return err
}

func (c iCredentials) Get(ctx context.Context, providerID string) (Sealed, error) {
	sealed, err := c.Credentials.Get(ctx, providerID)
	c.s.count("Credentials.Get", err)
	return sealed, err
}

func (c iCredentials) Delete(ctx context.Context, providerID string) error {
	err := c.Credentials.Delete(ctx, providerID)
	c.s.count("Credentials.Delete", err)
	return err
}

func (c iCredentials) List(ctx context.Context) ([]string, error) {
	ids, err := c.Credentials.List(ctx)
	c.s.count("Credentials.List", err)
	return ids, err
}

type iCounters struct {
	Counters
	s *instrumented
}

func (c iCounters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	total, err := c.Counters.Add(ctx, key, delta, expiresAt)
	c.s.count("Counters.Add", err)
	return total, err
}

func (c iCounters) Read(ctx context.Context, keys []string) (map[string]int64, error) {
	out, err := c.Counters.Read(ctx, keys)
	c.s.count("Counters.Read", err)
	return out, err
}

func (c iCounters) Prune(ctx context.Context, before time.Time) (int, error) {
	n, err := c.Counters.Prune(ctx, before)
	c.s.count("Counters.Prune", err)
	return n, err
}

type iLeases struct {
	Leases
	s *instrumented
}

func (l iLeases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	held, err := l.Leases.Acquire(ctx, name, holder, ttl)
	l.s.count("Leases.Acquire", err)
	return held, err
}

func (l iLeases) Release(ctx context.Context, name, holder string) error {
	err := l.Leases.Release(ctx, name, holder)
	l.s.count("Leases.Release", err)
	return err
}

type iJournal struct {
	Journal
	s *instrumented
}

func (j iJournal) Append(ctx context.Context, e Event) (int64, error) {
	seq, err := j.Journal.Append(ctx, e)
	j.s.count("Journal.Append", err)
	return seq, err
}

func (j iJournal) Pending(ctx context.Context, limit int) ([]Event, error) {
	events, err := j.Journal.Pending(ctx, limit)
	j.s.count("Journal.Pending", err)
	return events, err
}

func (j iJournal) Acknowledge(ctx context.Context, id string) error {
	err := j.Journal.Acknowledge(ctx, id)
	j.s.count("Journal.Acknowledge", err)
	return err
}

func (j iJournal) Defer(ctx context.Context, id string, attempts int, next time.Time) error {
	err := j.Journal.Defer(ctx, id, attempts, next)
	j.s.count("Journal.Defer", err)
	return err
}

func (j iJournal) Drop(ctx context.Context, id string) error {
	err := j.Journal.Drop(ctx, id)
	j.s.count("Journal.Drop", err)
	return err
}

func (j iJournal) ByObject(ctx context.Context, objectID string, p Page) ([]Event, string, error) {
	events, next, err := j.Journal.ByObject(ctx, objectID, p)
	j.s.count("Journal.ByObject", err)
	return events, next, err
}

func (j iJournal) Since(ctx context.Context, afterGSeq int64, limit int) ([]Event, error) {
	events, err := j.Journal.Since(ctx, afterGSeq, limit)
	j.s.count("Journal.Since", err)
	return events, err
}

func (j iJournal) Prune(ctx context.Context, before time.Time) (int, error) {
	n, err := j.Journal.Prune(ctx, before)
	j.s.count("Journal.Prune", err)
	return n, err
}

type iTunnels struct {
	Tunnels
	s *instrumented
}

func (t iTunnels) Register(ctx context.Context, row Tunnel, ttl time.Duration) error {
	err := t.Tunnels.Register(ctx, row, ttl)
	t.s.count("Tunnels.Register", err)
	return err
}

func (t iTunnels) Heartbeat(ctx context.Context, providerID, session string, ttl time.Duration) (bool, error) {
	held, err := t.Tunnels.Heartbeat(ctx, providerID, session, ttl)
	t.s.count("Tunnels.Heartbeat", err)
	return held, err
}

func (t iTunnels) Get(ctx context.Context, providerID string) (Tunnel, error) {
	row, err := t.Tunnels.Get(ctx, providerID)
	t.s.count("Tunnels.Get", err)
	return row, err
}

func (t iTunnels) Unregister(ctx context.Context, providerID, session string) error {
	err := t.Tunnels.Unregister(ctx, providerID, session)
	t.s.count("Tunnels.Unregister", err)
	return err
}
