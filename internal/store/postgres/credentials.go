// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"

	"latere.ai/x/lux/internal/store"
)

type credentials struct{ s *Store }

// Put implements store.Credentials: a new value writes every column.
func (c credentials) Put(ctx context.Context, providerID string, row store.Sealed) error {
	if providerID == "" {
		return errors.New("postgres: Credentials.Put with no provider id")
	}
	err := c.s.read(ctx, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO credentials (provider_id, version, wrapped_key, wrapped_nonce, ciphertext, nonce, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (provider_id) DO UPDATE SET
				version = excluded.version, wrapped_key = excluded.wrapped_key, wrapped_nonce = excluded.wrapped_nonce,
				ciphertext = excluded.ciphertext, nonce = excluded.nonce, updated_at = excluded.updated_at`,
			providerID, row.Version, bytesOf(row.WrappedKey), bytesOf(row.WrappedNonce), bytesOf(row.Ciphertext), bytesOf(row.Nonce), c.s.now())
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: Credentials.Put: %w", err)
	}
	return nil
}

// Rewrap implements store.Credentials: the two wrap columns of the row
// at ifVersion, and nothing else; zero rows moved is a stale version
// when the row exists and ErrNotFound when it does not.
func (c credentials) Rewrap(ctx context.Context, providerID string, ifVersion int, wrappedKey, wrappedNonce []byte) error {
	err := c.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE credentials SET wrapped_key = $3, wrapped_nonce = $4 WHERE provider_id = $1 AND version = $2`,
			providerID, ifVersion, bytesOf(wrappedKey), bytesOf(wrappedNonce))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var version int
		err = q.QueryRow(ctx, `SELECT version FROM credentials WHERE provider_id = $1`, providerID).Scan(&version)
		switch {
		case errors.Is(err, errNoRows):
			return fmt.Errorf("%w: provider %s has no credential", store.ErrNotFound, providerID)
		case err != nil:
			return err
		default:
			return fmt.Errorf("%w: credential of %s is at version %d, the re-wrap at %d", store.ErrVersionConflict, providerID, version, ifVersion)
		}
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrVersionConflict) {
			return err
		}
		return fmt.Errorf("postgres: Credentials.Rewrap: %w", err)
	}
	return nil
}

// Get implements store.Credentials.
func (c credentials) Get(ctx context.Context, providerID string) (store.Sealed, error) {
	var out store.Sealed
	err := c.s.read(ctx, func(q querier) error {
		err := q.QueryRow(ctx, `SELECT version, wrapped_key, wrapped_nonce, ciphertext, nonce FROM credentials WHERE provider_id = $1`, providerID).
			Scan(&out.Version, &out.WrappedKey, &out.WrappedNonce, &out.Ciphertext, &out.Nonce)
		if errors.Is(err, errNoRows) {
			return fmt.Errorf("%w: provider %s has no credential", store.ErrNotFound, providerID)
		}
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Sealed{}, err
		}
		return store.Sealed{}, fmt.Errorf("postgres: Credentials.Get: %w", err)
	}
	return out, nil
}

// Delete implements store.Credentials.
func (c credentials) Delete(ctx context.Context, providerID string) error {
	err := c.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM credentials WHERE provider_id = $1`, providerID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: provider %s has no credential", store.ErrNotFound, providerID)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return err
		}
		return fmt.Errorf("postgres: Credentials.Delete: %w", err)
	}
	return nil
}

// List implements store.Credentials.
func (c credentials) List(ctx context.Context) ([]string, error) {
	ids := []string{}
	err := c.s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT provider_id FROM credentials ORDER BY provider_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Credentials.List: %w", err)
	}
	return ids, nil
}

// bytesOf is b, or an empty slice for nil, so a column declared NOT
// NULL takes an absent member as no bytes.
func bytesOf(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

var _ store.Credentials = credentials{}
