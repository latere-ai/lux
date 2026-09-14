// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lux/internal/store"
)

type keys struct{ s *Store }

// hashLength is SHA-256 as lower-case hex.
const hashLength = 64

// checkHash holds a hash to 64 lower-case hex characters; another shape
// is a caller's mistake and never a row.
func checkHash(hash string) error {
	if len(hash) != hashLength {
		return fmt.Errorf("postgres: a key hash is %d lower-case hex characters, this one is %d", hashLength, len(hash))
	}
	for i := range len(hash) {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("postgres: a key hash is lower-case hex; byte %d is %q", i, c)
		}
	}
	return nil
}

// Put implements store.Keys: the key's previous hash goes, the new one
// is written, and a hash another key holds is ErrHashTaken, whether the
// read or the primary key says so.
func (k keys) Put(ctx context.Context, keyID, hash string) error {
	if keyID == "" {
		return errors.New("postgres: Keys.Put with no key id")
	}
	if err := checkHash(hash); err != nil {
		return err
	}
	err := k.s.write(ctx, func(q pgx.Tx) error {
		var other string
		err := q.QueryRow(ctx, `SELECT key_id FROM key_hashes WHERE hash = $1`, hash).Scan(&other)
		switch {
		case err == nil && other != keyID:
			return fmt.Errorf("%w: the hash is registered to another key", store.ErrHashTaken)
		case err == nil:
			return nil // the same hash on the same key
		case !errors.Is(err, errNoRows):
			return err
		}
		if _, err := q.Exec(ctx, `DELETE FROM key_hashes WHERE key_id = $1`, keyID); err != nil {
			return err
		}
		_, err = q.Exec(ctx, `INSERT INTO key_hashes (hash, key_id, created_at) VALUES ($1, $2, $3)`, hash, keyID, k.s.now())
		if violates(err, "key_hashes_pkey") {
			return fmt.Errorf("%w: the hash is registered to another key", store.ErrHashTaken)
		}
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrHashTaken) {
			return err
		}
		return fmt.Errorf("postgres: Keys.Put: %w", err)
	}
	return nil
}

// ByHash implements store.Keys: the hot path's one lookup, on the
// primary key.
func (k keys) ByHash(ctx context.Context, hash string) (string, error) {
	var id string
	err := k.s.read(ctx, func(q querier) error {
		err := q.QueryRow(ctx, `SELECT key_id FROM key_hashes WHERE hash = $1`, hash).Scan(&id)
		if errors.Is(err, errNoRows) {
			return fmt.Errorf("%w: no key has this hash", store.ErrNotFound)
		}
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		return "", fmt.Errorf("postgres: Keys.ByHash: %w", err)
	}
	return id, nil
}

// Delete implements store.Keys.
func (k keys) Delete(ctx context.Context, keyID string) error {
	err := k.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM key_hashes WHERE key_id = $1`, keyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: key %s has no hash", store.ErrNotFound, keyID)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return err
		}
		return fmt.Errorf("postgres: Keys.Delete: %w", err)
	}
	return nil
}

var _ store.Keys = keys{}
