// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// CounterStore is the store's counter table as spec 010 declares it,
// which store.Store.Counters satisfies directly: Add is atomic and
// returns the new total, so one flush both writes a delta and refreshes
// the replica's view; expiresAt is written when the key is first added
// and a zero expiresAt is a window that never resets.
type CounterStore interface {
	Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (total int64, err error)
	Read(ctx context.Context, keys []string) (map[string]int64, error)
}

// DefaultFlush is the flush interval when NewCounters is given none,
// spec 009's LUX_METERING_FLUSH default.
const DefaultFlush = time.Second

// Counters holds one replica's unflushed deltas over a CounterStore.
// Add and Total run on the hot path and never touch the store; Flush is
// the one method that does, one Add per dirty key, and the total it
// returns replaces the replica's view of that key. A hard limit's check
// is Total plus the request's estimate against the amount, which reads
// this replica's own spending at once and every other replica's within
// one flush; Overshoot states the bound that gives.
type Counters struct {
	store CounterStore
	flush time.Duration
	now   func() time.Time

	mu   sync.Mutex
	rows map[string]*row
}

// row is one counter key as this replica sees it.
type row struct {
	known     int64     // the store's total as of the last flush
	pending   int64     // added since, not yet flushed
	expiresAt time.Time // the window's reset, zero for none
	touched   time.Time // the last Add or Flush, for dropping finished windows
}

// Option configures NewCounters.
type Option func(*Counters)

// WithClock sets the clock Counters reads to drop the rows of finished
// windows; the default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(c *Counters) { c.now = now }
}

// NewCounters returns an empty ledger over store that Run flushes every
// flush; a flush of zero or less is DefaultFlush.
func NewCounters(store CounterStore, flush time.Duration, opts ...Option) *Counters {
	if flush <= 0 {
		flush = DefaultFlush
	}
	c := &Counters{store: store, flush: flush, now: time.Now, rows: map[string]*row{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Add records n against key locally. expiresAt is the window's reset,
// written to the store when the key's row is first created there; a
// zero expiresAt is a window that never resets.
func (c *Counters) Add(key string, n int64, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rows[key]
	if r == nil {
		r = &row{expiresAt: expiresAt}
		c.rows[key] = r
	}
	r.pending += n
	r.touched = c.now()
}

// Total is what this replica believes key holds: the store's total as of
// the last flush plus the unflushed delta. A key this replica has never
// added to or flushed is zero.
func (c *Counters) Total(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.rows[key]; ok {
		return r.known + r.pending
	}
	return 0
}

// Pending is this replica's unflushed delta for key.
func (c *Counters) Pending(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.rows[key]; ok {
		return r.pending
	}
	return 0
}

// Flush writes every unflushed delta to the store, one Add per dirty
// key, and takes each returned total as the key's new known value. A
// key whose Add fails keeps its delta for the next flush, and the error
// names it; the other keys still flush. Rows of windows that reset
// before the previous flush and hold no delta are dropped, so a replica
// that lives through many windows does not keep every one.
func (c *Counters) Flush(ctx context.Context) error {
	type dirty struct {
		key       string
		delta     int64
		expiresAt time.Time
	}
	c.mu.Lock()
	now := c.now()
	batch := make([]dirty, 0, len(c.rows))
	for key, r := range c.rows {
		if r.pending != 0 {
			batch = append(batch, dirty{key, r.pending, r.expiresAt})
			r.pending = 0
			continue
		}
		if !r.expiresAt.IsZero() && r.expiresAt.Before(now) && r.touched.Add(c.flush).Before(now) {
			delete(c.rows, key)
		}
	}
	c.mu.Unlock()
	var errs []error
	for _, d := range batch {
		total, err := c.store.Add(ctx, d.key, d.delta, d.expiresAt)
		c.mu.Lock()
		r := c.rows[d.key]
		if r == nil {
			r = &row{expiresAt: d.expiresAt}
			c.rows[d.key] = r
		}
		if err != nil {
			r.pending += d.delta
			errs = append(errs, fmt.Errorf("counter %s: %w", d.key, err))
		} else {
			r.known = total
			r.touched = now
		}
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Run flushes on the interval until ctx ends, then flushes once more on
// a context that outlives the cancellation, so a stopping replica
// leaves no delta behind, and returns that last flush's error.
func (c *Counters) Run(ctx context.Context) error {
	t := time.NewTicker(c.flush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.flush)
			defer cancel()
			return c.Flush(last)
		case <-t.C:
			_ = c.Flush(ctx) // a failed flush keeps its deltas for the next tick
		}
	}
}

// Claim adds one to a marker counter and reports whether this call was
// the first: the store's add is atomic, so of every replica that
// observes a window at or over its amount exactly one is answered true
// and announces the exhaustion, and the marker resets with the window
// because it is keyed by the window's start.
func Claim(ctx context.Context, store CounterStore, key string, expiresAt time.Time) (first bool, err error) {
	total, err := store.Add(ctx, key, 1, expiresAt)
	if err != nil {
		return false, fmt.Errorf("marker %s: %w", key, err)
	}
	return total == 1, nil
}
