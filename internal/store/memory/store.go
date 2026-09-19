// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// Notice is the start-up line's substance: the three consequences of
// state that lives with the process. The serve role prints it after the
// fixed prefix an operator reads.
const Notice = "state in memory: nothing is recovered after a restart, every window starts empty, and this process is assumed to be the only replica"

// Option configures New.
type Option func(*Store)

// WithClock sets the clock the store reads for timestamps, lease and
// tunnel expiry, and pruning. The default is time.Now; a test passes a
// clock it moves.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Store is the memory store. One RWMutex guards the state: a read takes
// it shared, a write exclusive, and Transact holds it exclusive for the
// whole of fn, handing fn a Store that shares the state and skips the
// lock, which is why fn writes through tx and never through the outer
// Store.
type Store struct {
	mu     sync.RWMutex
	now    func() time.Time
	st     *state
	inTx   bool
	closed *atomic.Bool
}

// New returns an empty store.
func New(opts ...Option) *Store {
	s := &Store{now: time.Now, st: newState(), closed: new(atomic.Bool)}
	for _, o := range opts {
		o(s)
	}
	return s
}

// state is every collection's rows. The maps are replaced wholesale by
// a rollback and cloned shallowly for a snapshot, so a row is never
// edited in place: a write copies the row, changes the copy, and stores
// it back.
type state struct {
	objects  map[string]objectRow    // by id
	names    map[string]string       // kind/name of a live row to its id
	hashes   map[string]string       // key hash to key id
	keyHash  map[string]string       // key id to its hash
	creds    map[string]store.Sealed // provider id to its sealed row
	counters map[string]counterRow
	leases   map[string]leaseRow
	journal  map[string]store.Event // by event id
	seqs     map[string]int64       // object id to its last Seq
	gseq     int64
	tunnels  map[string]store.Tunnel // by provider id
	// The usage side of spec 009: the hourly aggregate rows by their
	// primary key, and the ring of records per Key id, oldest first.
	// The rings are the gateway's own and never part of an apply, so an
	// append may grow a ring's array in place; a snapshot keeps the
	// shorter header and a rollback shows the ring as it was.
	aggregates map[metering.AggregateKey]metering.Aggregate
	records    map[string][]metering.Record
}

func newState() *state {
	return &state{
		objects:  map[string]objectRow{},
		names:    map[string]string{},
		hashes:   map[string]string{},
		keyHash:  map[string]string{},
		creds:    map[string]store.Sealed{},
		counters: map[string]counterRow{},
		leases:   map[string]leaseRow{},
		journal:  map[string]store.Event{},
		seqs:     map[string]int64{},
		tunnels:  map[string]store.Tunnel{},

		aggregates: map[metering.AggregateKey]metering.Aggregate{},
		records:    map[string][]metering.Record{},
	}
}

// clone is the snapshot Transact keeps: every map copied one level
// deep, which is enough because no row is ever edited in place.
func (st *state) clone() *state {
	return &state{
		objects:  maps.Clone(st.objects),
		names:    maps.Clone(st.names),
		hashes:   maps.Clone(st.hashes),
		keyHash:  maps.Clone(st.keyHash),
		creds:    maps.Clone(st.creds),
		counters: maps.Clone(st.counters),
		leases:   maps.Clone(st.leases),
		journal:  maps.Clone(st.journal),
		seqs:     maps.Clone(st.seqs),
		gseq:     st.gseq,
		tunnels:  maps.Clone(st.tunnels),

		aggregates: maps.Clone(st.aggregates),
		records:    maps.Clone(st.records),
	}
}

// read runs fn over the state under the shared lock, or under no lock
// inside a Transact, which already holds the exclusive one. A context
// that has ended is answered with its error before anything is read.
func (s *Store) read(ctx context.Context, fn func(st *state) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.inTx {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	return fn(s.st)
}

// write is read's exclusive twin.
func (s *Store) write(ctx context.Context, fn func(st *state) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.inTx {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return fn(s.st)
}

// Objects implements store.Store.
func (s *Store) Objects() store.Objects { return objects{s} }

// Keys implements store.Store.
func (s *Store) Keys() store.Keys { return keys{s} }

// Credentials implements store.Store.
func (s *Store) Credentials() store.Credentials { return credentials{s} }

// Counters implements store.Store.
func (s *Store) Counters() store.Counters { return counters{s} }

// Leases implements store.Store.
func (s *Store) Leases() store.Leases { return leases{s} }

// Journal implements store.Store.
func (s *Store) Journal() store.Journal { return journal{s} }

// Tunnels implements store.Store.
func (s *Store) Tunnels() store.Tunnels { return tunnels{s} }

// Usage implements store.Store.
func (s *Store) Usage() store.Usage { return usage{s} }

// Transact holds the write lock for the call and runs fn against a
// Store that shares the state. A failure of fn, an error or a panic,
// puts back the snapshot taken before it, so an apply that fails
// half-way leaves no object, hash, credential, or journal row behind.
// A Transact on the Store fn was handed is ErrNested.
func (s *Store) Transact(ctx context.Context, fn func(tx store.Store) error) (err error) {
	if s.inTx {
		return store.ErrNested
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot := s.st.clone()
	defer func() {
		if r := recover(); r != nil {
			s.st = snapshot
			panic(r)
		}
		if err != nil {
			s.st = snapshot
		}
	}()
	if err := fn(&Store{now: s.now, st: s.st, inTx: true, closed: s.closed}); err != nil {
		return err
	}
	return ctx.Err()
}

// Ready implements store.Store: nil until Close, because memory answers
// as long as the process runs.
func (s *Store) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errors.New("memory: the store is closed")
	}
	return nil
}

// Close implements store.Store. The state is the process's and goes with
// it; Close only makes Ready say so.
func (s *Store) Close() error {
	s.closed.Store(true)
	return nil
}
