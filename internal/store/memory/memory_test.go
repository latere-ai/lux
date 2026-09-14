// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// clock is a settable clock for the rows that read one.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func provider(name string) *v1.Provider {
	return &v1.Provider{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1"},
		Status:   v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, time.Now(), nil), Owner: "https://login.example.com|alice", Warnings: []string{}},
	}
}

// TestClockDrivesTimestampsLeasesAndPruning holds the store to reading
// the clock it was given, which is what lets a lease lapse and a window
// end without waiting.
func TestClockDrivesTimestampsLeasesAndPruning(t *testing.T) {
	ctx := t.Context()
	c := &clock{at: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)}
	s := memory.New(memory.WithClock(c.now))

	p := provider("openai")
	if _, err := s.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	if !p.Status.CreatedAt.Equal(c.at) || !p.Status.UpdatedAt.Equal(c.at) {
		t.Fatalf("timestamps %v %v, want the clock %v", p.Status.CreatedAt, p.Status.UpdatedAt, c.at)
	}
	c.at = c.at.Add(time.Minute)
	p.Status.UpdatedAt = time.Time{}
	if _, err := s.Objects().Put(ctx, p, 1); err != nil {
		t.Fatal(err)
	}
	if !p.Status.CreatedAt.Equal(c.at.Add(-time.Minute)) || !p.Status.UpdatedAt.Equal(c.at) {
		t.Fatalf("an update keeps createdAt and moves updatedAt: %v %v", p.Status.CreatedAt, p.Status.UpdatedAt)
	}
	given := c.at.Add(-time.Hour)
	q := provider("azure")
	q.Status.CreatedAt, q.Status.UpdatedAt = given, given
	if _, err := s.Objects().Put(ctx, q, 0); err != nil {
		t.Fatal(err)
	}
	if !q.Status.CreatedAt.Equal(given) || !q.Status.UpdatedAt.Equal(given) {
		t.Fatalf("a caller's timestamps are kept on a create: %v %v", q.Status.CreatedAt, q.Status.UpdatedAt)
	}

	held, err := s.Leases().Acquire(ctx, store.LeaseHealth, "a", store.LeaseTTL)
	if err != nil || !held {
		t.Fatalf("Acquire a = %v, %v", held, err)
	}
	c.at = c.at.Add(store.LeaseTTL - time.Second)
	if held, _ := s.Leases().Acquire(ctx, store.LeaseHealth, "b", store.LeaseTTL); held {
		t.Fatal("b acquired a lease that has not lapsed")
	}
	c.at = c.at.Add(time.Second)
	if held, _ := s.Leases().Acquire(ctx, store.LeaseHealth, "b", store.LeaseTTL); !held {
		t.Fatal("b did not acquire a lapsed lease")
	}

	if err := s.Tunnels().Register(ctx, store.Tunnel{ProviderID: "prv_1", Session: "tun_1"}, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	row, err := s.Tunnels().Get(ctx, "prv_1")
	if err != nil || !row.ConnectedAt.Equal(c.at) || !row.ExpiresAt.Equal(c.at.Add(30*time.Second)) {
		t.Fatalf("Register did not read the clock: %+v, %v", row, err)
	}
	c.at = c.at.Add(31 * time.Second)
	if _, err := s.Tunnels().Get(ctx, "prv_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a lapsed tunnel row = %v, want ErrNotFound", err)
	}

	if _, err := s.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: p.Status.ID}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Journal().Since(ctx, 0, 0)
	if !rows[0].At.Equal(c.at) {
		t.Fatalf("Append At = %v, want the clock %v", rows[0].At, c.at)
	}
	if err := s.Objects().Delete(ctx, v1.KindProvider, p.Status.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Journal().Acknowledge(ctx, "evt_1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Objects().Prune(ctx, c.at.Add(-time.Second)); n != 0 {
		t.Fatalf("Prune before the deletion removed %d", n)
	}
	if n, _ := s.Objects().Prune(ctx, c.at); n != 1 {
		t.Fatalf("Prune at the deletion removed %d, want 1", n)
	}
}

// TestTransactRollsBackOnPanic: a panic inside fn puts the snapshot back
// and is re-raised, so a handler panic mapped to internal by the API
// leaves no half-written apply behind.
func TestTransactRollsBackOnPanic(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	p := provider("openai")
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v, want the panic re-raised", r)
			}
		}()
		_ = s.Transact(ctx, func(tx store.Store) error {
			if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	}()
	if _, _, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get after a panicking Transact = %v, want ErrNotFound", err)
	}
	// The store is still usable: the lock was released.
	if _, err := s.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
}

// TestTransactSharesTheState: writes through tx are visible to reads
// through tx before the commit, and the outer store's collections see
// them once fn returns.
func TestTransactSharesTheState(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	p := provider("openai")
	err := s.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
			return err
		}
		got, v, err := tx.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
		if err != nil || v != 1 || got.Name() != "openai" {
			t.Fatalf("Get inside the Transact = %v, %d, %v", got, v, err)
		}
		if err := tx.Ready(ctx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, v, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID); err != nil || v != 1 {
		t.Fatalf("Get after the commit = %d, %v", v, err)
	}
}

// TestPlainErrorsCarryTheDetail reads the developer detail of the
// refusals a caller's mistake produces.
func TestPlainErrorsCarryTheDetail(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	p := provider("openai")
	if _, err := s.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		err    error
		detail string
	}{
		"kind":             {call(s.Objects().PutStatus(ctx, "Thing", p.Status.ID, nil)), `kind "Thing"`},
		"observed type":    {s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, 42), "takes store.ProviderObserved, got int"},
		"hash length":      {s.Keys().Put(ctx, "key_1", "abc"), "64 lower-case hex characters, this one is 3"},
		"hash alphabet":    {s.Keys().Put(ctx, "key_1", strings.Repeat("0", 63)+"G"), "byte 63 is 'G'"},
		"name taken":       {second(s.Objects().Put(ctx, provider("openai"), 0)), `Provider "openai" is ` + p.Status.ID},
		"stale version":    {second(s.Objects().Put(ctx, p, 7)), "is at version 1, the write at 7"},
		"missing row":      {second(s.Objects().Put(ctx, provider("ghost"), 1)), "has no live row"},
		"used id":          {second(s.Objects().Put(ctx, withID(provider("other"), p.Status.ID), 0)), "is already used"},
		"rewrap missing":   {s.Credentials().Rewrap(ctx, "prv_x", 1, nil, nil), "has no credential"},
		"unknown kind put": {second(s.Objects().Put(ctx, otherKind{}, 0)), "not one of the four kinds"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.detail) {
			t.Errorf("%s: %v lacks %q", name, tc.err, tc.detail)
		}
	}
}

func call(err error) error { return err }

func second(_ int64, err error) error { return err }

func withID(p *v1.Provider, id string) *v1.Provider {
	p.Status.ID = id
	return p
}

// otherKind is an Object of no kind the store holds.
type otherKind struct{}

func (otherKind) Kind() string  { return "Thing" }
func (otherKind) ID() string    { return "thg_1" }
func (otherKind) Owner() string { return "" }
func (otherKind) Name() string  { return "thing" }

func TestNoticeNamesTheThreeConsequences(t *testing.T) {
	for _, want := range []string{"nothing is recovered after a restart", "every window starts empty", "only replica"} {
		if !strings.Contains(memory.Notice, want) {
			t.Errorf("Notice lacks %q: %s", want, memory.Notice)
		}
	}
}
