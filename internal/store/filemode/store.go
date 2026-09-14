// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filemode

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Subject is the rendered subject that owns every object the directory
// declares and every Model discovery finds in this mode, since the mode
// has no issuer and no subject of its own.
const Subject = "file|manifest-dir"

// Options are what Load reads the directory under. Dir is required; the
// rest have the defaults the zero value gives.
type Options struct {
	// Dir is LUX_MANIFEST_DIR.
	Dir string
	// Getenv reads the variables valueFrom.env names; nil is os.Getenv.
	Getenv func(string) string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Defaults, AllowPrivateUpstreams, and PublicURL are the resolver's
	// options from the configuration, as the API sets them.
	Defaults              manifest.Defaults
	AllowPrivateUpstreams bool
	PublicURL             *url.URL
}

func (o Options) getenv(name string) string {
	if o.Getenv != nil {
		return o.Getenv(name)
	}
	return os.Getenv(name)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Summary is what one read of the directory produced, for the start-up
// line and for luxd check's manifest dir row.
type Summary struct {
	Dir   string
	Files int
	// Kinds is the count of declared objects per kind.
	Kinds map[string]int
}

// Store is the file mode: the memory store beneath a read-only view,
// with the credential values the directory's Providers name.
type Store struct {
	opts Options
	mem  *memory.Store
	view view

	reload sync.Mutex // one re-read at a time
	mu     sync.RWMutex
	ids    map[string]string // kind/name of a declared object to its id
	creds  map[string]string // provider id to its credential value
	sum    Summary
}

// Load reads the directory and returns the store serving it, or the
// start-up failure naming the file and the path inside it. There is no
// partial start: a directory that half applies is a gateway serving a
// policy nobody wrote.
func Load(ctx context.Context, o Options) (*Store, error) {
	if o.Dir == "" {
		return nil, fmt.Errorf("filemode: no directory")
	}
	mem := memory.New(memory.WithClock(o.now))
	s := &Store{opts: o, mem: mem, view: view{inner: mem, dir: o.Dir}, ids: map[string]string{}, creds: map[string]string{}}
	if err := s.Reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the directory and swaps the snapshot in one Transact.
// A failure leaves the running snapshot in place and is returned with
// the file and the path inside it, for the caller to log.
func (s *Store) Reload(ctx context.Context) error {
	s.reload.Lock()
	defer s.reload.Unlock()
	s.mu.RLock()
	prev := s.ids
	s.mu.RUnlock()
	snap, err := s.load(ctx, prev)
	if err != nil {
		return fmt.Errorf("manifest dir %s: %w", s.opts.Dir, err)
	}
	if err := s.swap(ctx, prev, snap); err != nil {
		return fmt.Errorf("manifest dir %s: %w", s.opts.Dir, err)
	}
	s.mu.Lock()
	s.ids, s.creds, s.sum = snap.ids(), snap.creds, snap.summary
	s.mu.Unlock()
	return nil
}

// Dir is the directory the store reads.
func (s *Store) Dir() string { return s.opts.Dir }

// Summary is the last successful read.
func (s *Store) Summary() Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sum
}

// CredentialValue is the value the Provider's credential names, held in
// memory for the life of the process, and false for a Provider without
// one. It is the seam the gateway's credential source reads in this
// mode, since Credentials refuses every call.
func (s *Store) CredentialValue(providerID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.creds[providerID]
	return v, ok
}

// Notice is the start-up line's substance: what was read, that ids are
// per start, and that spend windows and leases are per replica.
func (s *Store) Notice() string {
	sum := s.Summary()
	return fmt.Sprintf("manifest dir %s: %d files read, %d providers, %d budgets, %d models, %d keys; ids are minted per start and differ between starts and replicas, so address objects by name; spend limits, budgets, and leases are per replica",
		sum.Dir, sum.Files, sum.Kinds[v1.KindProvider], sum.Kinds[v1.KindBudget], sum.Kinds[v1.KindModel], sum.Kinds[v1.KindKey])
}

// The store.Store methods, through the read-only view.

// Objects implements store.Store.
func (s *Store) Objects() store.Objects { return s.view.Objects() }

// Keys implements store.Store.
func (s *Store) Keys() store.Keys { return s.view.Keys() }

// Credentials implements store.Store.
func (s *Store) Credentials() store.Credentials { return s.view.Credentials() }

// Counters implements store.Store.
func (s *Store) Counters() store.Counters { return s.view.Counters() }

// Leases implements store.Store.
func (s *Store) Leases() store.Leases { return s.view.Leases() }

// Journal implements store.Store.
func (s *Store) Journal() store.Journal { return s.view.Journal() }

// Tunnels implements store.Store.
func (s *Store) Tunnels() store.Tunnels { return s.view.Tunnels() }

// Usage implements store.Store: the aggregates and the rings are the
// gateway's and are written in this mode as in any other.
func (s *Store) Usage() store.Usage { return s.view.Usage() }

// Transact implements store.Store: the memory store's Transact with the
// read-only rules on the Store fn is handed.
func (s *Store) Transact(ctx context.Context, fn func(tx store.Store) error) error {
	return s.view.Transact(ctx, fn)
}

// Ready implements store.Store.
func (s *Store) Ready(ctx context.Context) error { return s.view.Ready(ctx) }

// Close implements store.Store.
func (s *Store) Close() error { return s.view.Close() }
