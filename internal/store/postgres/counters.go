// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
)

type counters struct{ s *Store }

// Add implements store.Counters: one INSERT ... ON CONFLICT DO UPDATE
// that adds the delta and returns the total, so two replicas adding at
// once produce one total and neither reads before it writes. expires_at
// is written by the insert and left alone by the update; a zero
// expiresAt is a null column, a window that never resets.
func (c counters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	if key == "" {
		return 0, errors.New("postgres: Counters.Add with no key")
	}
	var total int64
	err := c.s.read(ctx, func(q querier) error {
		return q.QueryRow(ctx, `
			INSERT INTO counters (key, value, expires_at) VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET value = counters.value + excluded.value
			RETURNING value`, key, delta, nullTime(expiresAt)).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: Counters.Add: %w", err)
	}
	return total, nil
}

// Read implements store.Counters.
func (c counters) Read(ctx context.Context, keys []string) (map[string]int64, error) {
	out := make(map[string]int64, len(keys))
	err := c.s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT key, value FROM counters WHERE key = ANY($1::text[])`, keys)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var v int64
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			out[k] = v
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Counters.Read: %w", err)
	}
	return out, nil
}

// Prune implements store.Counters.
func (c counters) Prune(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := c.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM counters WHERE expires_at IS NOT NULL AND expires_at <= $1`, before)
		n = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: Counters.Prune: %w", err)
	}
	return n, nil
}

// nullTime is a timestamptz that is null for the zero time.
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

var _ store.Counters = counters{}
