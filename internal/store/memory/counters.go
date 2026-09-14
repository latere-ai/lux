// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"time"

	"latere.ai/x/lux/internal/store"
)

type counterRow struct {
	value     int64
	expiresAt time.Time
}

type counters struct{ s *Store }

// Add implements store.Counters. The write lock is what makes it atomic:
// a thousand callers add a thousand deltas and each reads a total no
// other caller is half-way through changing.
func (c counters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	if key == "" {
		return 0, errors.New("memory: Counters.Add with no key")
	}
	var total int64
	err := c.s.write(ctx, func(st *state) error {
		row, ok := st.counters[key]
		if !ok {
			row.expiresAt = expiresAt
		}
		row.value += delta
		st.counters[key] = row
		total = row.value
		return nil
	})
	return total, err
}

// Read implements store.Counters.
func (c counters) Read(ctx context.Context, keys []string) (map[string]int64, error) {
	out := make(map[string]int64, len(keys))
	err := c.s.read(ctx, func(st *state) error {
		for _, k := range keys {
			if row, ok := st.counters[k]; ok {
				out[k] = row.value
			}
		}
		return nil
	})
	return out, err
}

// Prune implements store.Counters.
func (c counters) Prune(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := c.s.write(ctx, func(st *state) error {
		for k, row := range st.counters {
			if !row.expiresAt.IsZero() && !row.expiresAt.After(before) {
				delete(st.counters, k)
				n++
			}
		}
		return nil
	})
	return n, err
}

var _ store.Counters = counters{}
