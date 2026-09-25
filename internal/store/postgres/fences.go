// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

type keyFences struct{ s *Store }

// All credential writers take this lock before inspecting names. Transaction
// scope includes the enclosing apply/rotation, not merely a Put savepoint.
// Reads and inference never take it. Two int32 keys reserve a separate advisory
// lock namespace from the schema migrator's bigint lock.
func lockKeyWrites(ctx context.Context, q pgx.Tx) error {
	var isolation string
	if err := q.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return err
	}
	if isolation != "read committed" {
		return errors.New("key fences require READ COMMITTED isolation")
	}
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(1819637859, 1)`)
	return err
}

func readFence(ctx context.Context, q querier, name string) (store.KeyFence, error) {
	var f store.KeyFence
	err := q.QueryRow(ctx, `SELECT name,owner,labels,created_at FROM key_fences WHERE name=$1`, name).Scan(&f.Name, &f.Owner, &f.Labels, &f.CreatedAt)
	if errors.Is(err, errNoRows) {
		return f, store.ErrNotFound
	}
	return f, err
}
func (f keyFences) Get(ctx context.Context, name string) (store.KeyFence, error) {
	var result store.KeyFence
	err := f.s.read(ctx, func(q querier) error { var err error; result, err = readFence(ctx, q, name); return err })
	return result, named("KeyFences.Get", err)
}
func (f keyFences) Put(ctx context.Context, input store.KeyFence) (store.KeyFence, bool, error) {
	if err := input.Validate(); err != nil {
		return store.KeyFence{}, false, err
	}
	var result store.KeyFence
	var inserted bool
	err := f.s.write(ctx, func(q pgx.Tx) error {
		if err := lockKeyWrites(ctx, q); err != nil {
			return err
		}
		old, err := readFence(ctx, q, input.Name)
		if err == nil {
			if !old.Matches(input.Owner, input.Labels) {
				return store.ErrFenceConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var owner string
		var labels map[string]string
		err = q.QueryRow(ctx, `SELECT owner,labels FROM objects WHERE kind=$1 AND name=$2 AND deleted_at IS NULL`, v1.KindKey, input.Name).Scan(&owner, &labels)
		if err == nil && !input.Matches(owner, labels) {
			return store.ErrFenceConflict
		}
		if err != nil && !errors.Is(err, errNoRows) {
			return err
		}
		input.CreatedAt = f.s.now()
		if input.Labels == nil {
			input.Labels = map[string]string{}
		}
		text, err := labelsText(input.Labels)
		if err != nil {
			return err
		}
		err = q.QueryRow(ctx, `INSERT INTO key_fences(name,owner,labels,created_at) VALUES($1,$2,$3,$4) RETURNING created_at`, input.Name, input.Owner, text, input.CreatedAt).Scan(&input.CreatedAt)
		if err == nil {
			result = input.Clone()
			inserted = true
		}
		return err
	})
	return result, inserted && err == nil, named("KeyFences.Put", err)
}

func checkKeyFence(ctx context.Context, q pgx.Tx, next *v1.Key, version int64) error {
	if err := lockKeyWrites(ctx, q); err != nil {
		return err
	}
	var fenced bool
	// Include the stored name so a rename cannot escape an existing fence.
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM key_fences WHERE name=$1 OR name IN (SELECT name FROM objects WHERE id=$2 AND kind=$3))`, next.Name(), next.ID(), v1.KindKey).Scan(&fenced); err != nil {
		return err
	}
	if !fenced {
		return nil
	}
	if version == 0 {
		return store.ErrKeyFenced
	}
	var spec, status []byte
	err := q.QueryRow(ctx, `SELECT spec,status FROM objects WHERE id=$1 AND kind=$2 AND deleted_at IS NULL`, next.ID(), v1.KindKey).Scan(&spec, &status)
	if errors.Is(err, errNoRows) {
		return store.ErrKeyFenced
	}
	if err != nil {
		return err
	}
	obj, err := decode(v1.KindKey, spec, status)
	if err != nil {
		return err
	}
	old, ok := obj.(*v1.Key)
	if !ok || !store.DisableOnly(old, next) {
		return store.ErrKeyFenced
	}
	return nil
}
