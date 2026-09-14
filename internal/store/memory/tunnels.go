// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
)

type tunnels struct{ s *Store }

// Register implements store.Tunnels.
func (t tunnels) Register(ctx context.Context, row store.Tunnel, ttl time.Duration) error {
	if row.ProviderID == "" || row.Session == "" {
		return errors.New("memory: Tunnels.Register needs a provider id and a session")
	}
	if ttl <= 0 {
		return errors.New("memory: Tunnels.Register needs a positive ttl")
	}
	return t.s.write(ctx, func(st *state) error {
		now := t.s.now()
		if row.ConnectedAt.IsZero() {
			row.ConnectedAt = now
		}
		row.ExpiresAt = now.Add(ttl)
		st.tunnels[row.ProviderID] = row
		return nil
	})
}

// Heartbeat implements store.Tunnels.
func (t tunnels) Heartbeat(ctx context.Context, providerID, session string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, errors.New("memory: Tunnels.Heartbeat needs a positive ttl")
	}
	held := false
	err := t.s.write(ctx, func(st *state) error {
		row, ok := st.tunnels[providerID]
		if !ok || row.Session != session {
			return nil
		}
		row.ExpiresAt = t.s.now().Add(ttl)
		st.tunnels[providerID] = row
		held = true
		return nil
	})
	return held, err
}

// Get implements store.Tunnels.
func (t tunnels) Get(ctx context.Context, providerID string) (store.Tunnel, error) {
	var out store.Tunnel
	err := t.s.read(ctx, func(st *state) error {
		row, ok := st.tunnels[providerID]
		if !ok || !row.ExpiresAt.After(t.s.now()) {
			return fmt.Errorf("%w: provider %s has no live tunnel", store.ErrNotFound, providerID)
		}
		out = row
		return nil
	})
	return out, err
}

// Unregister implements store.Tunnels.
func (t tunnels) Unregister(ctx context.Context, providerID, session string) error {
	return t.s.write(ctx, func(st *state) error {
		if row, ok := st.tunnels[providerID]; ok && row.Session == session {
			delete(st.tunnels, providerID)
		}
		return nil
	})
}
