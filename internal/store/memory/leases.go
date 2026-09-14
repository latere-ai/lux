// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"time"
)

type leaseRow struct {
	holder    string
	expiresAt time.Time
}

type leases struct{ s *Store }

// Acquire implements store.Leases.
func (l leases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if name == "" || holder == "" {
		return false, errors.New("memory: Leases.Acquire needs a name and a holder")
	}
	if ttl <= 0 {
		return false, errors.New("memory: Leases.Acquire needs a positive ttl")
	}
	held := false
	err := l.s.write(ctx, func(st *state) error {
		now := l.s.now()
		row, ok := st.leases[name]
		if ok && row.holder != holder && row.expiresAt.After(now) {
			return nil
		}
		st.leases[name] = leaseRow{holder: holder, expiresAt: now.Add(ttl)}
		held = true
		return nil
	})
	return held, err
}

// Release implements store.Leases.
func (l leases) Release(ctx context.Context, name, holder string) error {
	return l.s.write(ctx, func(st *state) error {
		if row, ok := st.leases[name]; ok && row.holder == holder {
			delete(st.leases, name)
		}
		return nil
	})
}
