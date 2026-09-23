// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// pausedRead captures the store result before blocking. A concurrent mutation
// therefore cannot change the answer already returned by the underlying store.
type pausedRead struct {
	store.Store
	started, release chan struct{}
	calls            atomic.Int32
	hash             bool
}

func (p *pausedRead) pause(ctx context.Context) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
}
func (p *pausedRead) Objects() store.Objects { return pausedObjects{Objects: p.Store.Objects(), p: p} }
func (p *pausedRead) Keys() store.Keys       { return pausedKeys{Keys: p.Store.Keys(), p: p} }

type pausedObjects struct {
	store.Objects
	p *pausedRead
}

func (o pausedObjects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	obj, version, err := o.Objects.Get(ctx, kind, id)
	if !o.p.hash {
		o.p.pause(ctx)
	}
	return obj, version, err
}

type pausedKeys struct {
	store.Keys
	p *pausedRead
}

func (k pausedKeys) ByHash(ctx context.Context, hash string) (string, error) {
	id, err := k.Keys.ByHash(ctx, hash)
	if k.p.hash {
		k.p.pause(ctx)
	}
	return id, err
}
func TestKeyCacheRejectsDelayedAuthority(t *testing.T) {
	for _, change := range []string{"disable", "delete", "rotate", "reset", "ttl", "newer lookup"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			key, value := h.key(t, "delayed", nil)
			paused := &pausedRead{Store: h.st, started: make(chan struct{}), release: make(chan struct{})}
			cache := h.cache(paused, nil, DefaultKeyCache)
			hash := HashKeyValue(value)
			type answer struct {
				k   *v1.Key
				err error
			}
			done := make(chan answer, 1)
			go func() { k, err := cache.ByHash(t.Context(), hash); done <- answer{k, err} }()
			<-paused.started
			switch change {
			case "delete":
				if err := h.st.Objects().Delete(t.Context(), v1.KindKey, key.ID()); err != nil {
					t.Fatal(err)
				}
			case "rotate":
				if err := h.st.Keys().Put(t.Context(), key.ID(), HashKeyValue("new-value")); err != nil {
					t.Fatal(err)
				}
			default:
				key.Spec.Disabled = true
				if _, err := h.st.Objects().Put(t.Context(), key, key.Status.Version); err != nil {
					t.Fatal(err)
				}
			}
			switch change {
			case "reset":
				cache.Reset()
			case "ttl":
				h.advance(DefaultKeyCache)
			case "newer lookup":
				if k, err := cache.ByHash(t.Context(), hash); err != nil || !k.Spec.Disabled {
					t.Fatal(k, err)
				}
			default:
				cache.evictObject(key.ID())
			}
			close(paused.release)
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			missing := change == "delete" || change == "rotate"
			if missing && got.k != nil || !missing && (got.k == nil || !got.k.Spec.Disabled) {
				t.Fatalf("delayed lookup returned stale authority: %+v", got.k)
			}
			current, err := cache.ByHash(t.Context(), hash)
			if err != nil || missing && current != nil || !missing && (current == nil || !current.Spec.Disabled) {
				t.Fatal("stale cache was repopulated", current, err)
			}
		})
	}
}
func TestBudgetCacheRejectsDelayedAuthority(t *testing.T) {
	for _, change := range []string{"invalidate", "reset", "ttl", "newer lookup"} {
		t.Run(change, func(t *testing.T) {
			h := newHarness(t)
			budget := h.budget(t, "team", "10", "USD", v1.WindowMonth, true)
			paused := &pausedRead{Store: h.st, started: make(chan struct{}), release: make(chan struct{})}
			cache := h.cache(paused, nil, DefaultKeyCache)
			type answer struct {
				b   *v1.Budget
				err error
			}
			done := make(chan answer, 1)
			go func() { b, err := cache.Budget(t.Context(), budget.ID()); done <- answer{b, err} }()
			<-paused.started
			amount := v1.Money(1_000_000)
			budget.Spec.Amount = &amount
			if _, err := h.st.Objects().Put(t.Context(), budget, budget.Status.Version); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "reset":
				cache.Reset()
			case "ttl":
				h.advance(DefaultKeyCache)
			case "newer lookup":
				if b, err := cache.Budget(t.Context(), budget.ID()); err != nil || *b.Spec.Amount != amount {
					t.Fatal(b, err)
				}
			default:
				cache.evictObject(budget.ID())
			}
			close(paused.release)
			got := <-done
			if got.err != nil || got.b == nil || *got.b.Spec.Amount != amount {
				t.Fatal("stale budget returned", got.b, got.err)
			}
		})
	}
}
func TestKeyCacheRejectsDelayedNegativeAfterReset(t *testing.T) {
	h := newHarness(t)
	paused := &pausedRead{Store: h.st, started: make(chan struct{}), release: make(chan struct{}), hash: true}
	cache := h.cache(paused, nil, time.Minute)
	value := "new-key-value"
	done := make(chan *v1.Key, 1)
	go func() {
		k, err := cache.ByHash(t.Context(), HashKeyValue(value))
		if err != nil {
			t.Error(err)
		}
		done <- k
	}()
	<-paused.started
	key := h.storeKey(t, &v1.Key{Metadata: v1.ObjectMeta{Name: "appeared"}}, value, true)
	cache.Reset()
	close(paused.release)
	if got := <-done; got == nil || got.ID() != key.ID() {
		t.Fatal("stale absence returned", got)
	}
}

func TestCacheLookupFailsClosed(t *testing.T) {
	for _, mode := range []string{"invalidated", "expired", "canceled before", "canceled during", "store failure"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			cache := h.cache(h.st, nil, DefaultKeyCache)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled before" {
				cancel()
			}
			calls := 0
			_, _, err := cache.lookup(ctx, "test", func() (*entry, error) {
				calls++
				switch mode {
				case "invalidated":
					cache.Reset()
				case "expired":
					h.advance(DefaultKeyCache)
				case "canceled during":
					cancel()
				case "store failure":
					return nil, store.ErrNotFound
				}
				return &entry{}, nil
			})
			if err == nil || cache.Len() != 0 {
				t.Fatalf("failed open: error=%v entries=%d", err, cache.Len())
			}
			want := 3
			switch mode {
			case "canceled before":
				want = 0
			case "canceled during", "store failure":
				want = 1
			}
			if calls != want {
				t.Fatalf("reads=%d, want %d", calls, want)
			}
		})
	}
}

func TestCacheRetryUsesConcurrentAnswer(t *testing.T) {
	h := newHarness(t)
	cache := h.cache(h.st, nil, DefaultKeyCache)
	calls := 0
	got, hit, err := cache.lookup(t.Context(), "test", func() (*entry, error) {
		calls++
		cache.Reset()
		if _, _, err := cache.lookup(t.Context(), "test", func() (*entry, error) { return &entry{id: "current"}, nil }); err != nil {
			t.Fatal(err)
		}
		return &entry{id: "stale"}, nil
	})
	if err != nil || hit != fromStore || got.id != "current" || calls != 1 {
		t.Fatal(got, hit, err, calls)
	}
}
