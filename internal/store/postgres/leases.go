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

type leases struct{ s *Store }

// Acquire implements store.Leases: one INSERT ... ON CONFLICT DO UPDATE
// whose predicate admits the holder that has the lease, which renews
// it, and any holder once the row lapsed; a row another holder still
// has updates nothing, which is the false answer.
func (l leases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if name == "" || holder == "" {
		return false, errors.New("postgres: Leases.Acquire needs a name and a holder")
	}
	if ttl <= 0 {
		return false, errors.New("postgres: Leases.Acquire needs a positive ttl")
	}
	held := false
	err := l.s.read(ctx, func(q querier) error {
		now := l.s.now()
		tag, err := q.Exec(ctx, `
			INSERT INTO leases (name, holder, expires_at) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
			WHERE leases.holder = excluded.holder OR leases.expires_at <= $4`, name, holder, now.Add(ttl), now)
		held = tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("postgres: Leases.Acquire: %w", err)
	}
	return held, nil
}

// Release implements store.Leases.
func (l leases) Release(ctx context.Context, name, holder string) error {
	err := l.s.read(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM leases WHERE name = $1 AND holder = $2`, name, holder)
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: Leases.Release: %w", err)
	}
	return nil
}

var _ store.Leases = leases{}
