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

type tunnels struct{ s *Store }

// Register implements store.Tunnels: the newest session wins, and the
// row's expiry is now plus the ttl.
func (t tunnels) Register(ctx context.Context, row store.Tunnel, ttl time.Duration) error {
	if row.ProviderID == "" || row.Session == "" {
		return errors.New("postgres: Tunnels.Register needs a provider id and a session")
	}
	if ttl <= 0 {
		return errors.New("postgres: Tunnels.Register needs a positive ttl")
	}
	err := t.s.read(ctx, func(q querier) error {
		now := t.s.now()
		if row.ConnectedAt.IsZero() {
			row.ConnectedAt = now
		}
		_, err := q.Exec(ctx, `
			INSERT INTO tunnels (provider_id, session, replica, subject, agent, connected_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (provider_id) DO UPDATE SET
				session = excluded.session, replica = excluded.replica, subject = excluded.subject,
				agent = excluded.agent, connected_at = excluded.connected_at, expires_at = excluded.expires_at`,
			row.ProviderID, row.Session, row.Replica, row.Subject, row.Agent, row.ConnectedAt, now.Add(ttl))
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: Tunnels.Register: %w", err)
	}
	return nil
}

// Heartbeat implements store.Tunnels: the row is renewed when the
// session still holds it, and nothing happens otherwise, which is the
// false answer.
func (t tunnels) Heartbeat(ctx context.Context, providerID, session string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, errors.New("postgres: Tunnels.Heartbeat needs a positive ttl")
	}
	held := false
	err := t.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE tunnels SET expires_at = $3 WHERE provider_id = $1 AND session = $2`,
			providerID, session, t.s.now().Add(ttl))
		held = tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("postgres: Tunnels.Heartbeat: %w", err)
	}
	return held, nil
}

// Get implements store.Tunnels: the row while it is live.
func (t tunnels) Get(ctx context.Context, providerID string) (store.Tunnel, error) {
	var out store.Tunnel
	err := t.s.read(ctx, func(q querier) error {
		err := q.QueryRow(ctx, `
			SELECT provider_id, session, replica, subject, agent, connected_at, expires_at
			FROM tunnels WHERE provider_id = $1 AND expires_at > $2`, providerID, t.s.now()).
			Scan(&out.ProviderID, &out.Session, &out.Replica, &out.Subject, &out.Agent, &out.ConnectedAt, &out.ExpiresAt)
		if errors.Is(err, errNoRows) {
			return fmt.Errorf("%w: provider %s has no live tunnel", store.ErrNotFound, providerID)
		}
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Tunnel{}, err
		}
		return store.Tunnel{}, fmt.Errorf("postgres: Tunnels.Get: %w", err)
	}
	out.ConnectedAt, out.ExpiresAt = out.ConnectedAt.UTC(), out.ExpiresAt.UTC()
	return out, nil
}

// Unregister implements store.Tunnels.
func (t tunnels) Unregister(ctx context.Context, providerID, session string) error {
	err := t.s.read(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `DELETE FROM tunnels WHERE provider_id = $1 AND session = $2`, providerID, session)
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: Tunnels.Unregister: %w", err)
	}
	return nil
}

var _ store.Tunnels = tunnels{}
