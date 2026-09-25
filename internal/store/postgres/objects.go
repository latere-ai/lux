// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

type objects struct{ s *Store }

// objectColumns is what every read scans: the spec half, the status
// half with the observed column laid over it, and the version.
const objectColumns = `spec, status || observed, version`

// named wraps a contract error so the caller's errors.Is finds it, and
// any other error with the operation's name.
func named(op string, err error) error {
	if err == nil {
		return nil
	}
	for _, e := range store.Errors() {
		if errors.Is(err, e) {
			return err
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("postgres: %s: %w", op, err)
}

// Put implements store.Objects.
func (o objects) Put(ctx context.Context, obj v1.Object, ifVersion int64) (int64, error) {
	if obj == nil {
		return 0, errors.New("postgres: Put of a nil object")
	}
	if _, err := fieldsOf(obj); err != nil {
		return 0, err
	}
	kind, name, id := obj.Kind(), obj.Name(), obj.ID()
	if name == "" {
		return 0, fmt.Errorf("postgres: Put of a %s with no metadata.name", kind)
	}
	if ifVersion < 0 {
		return 0, fmt.Errorf("postgres: Put of %s %q at version %d, below 0", kind, name, ifVersion)
	}
	if id == "" {
		return 0, fmt.Errorf("postgres: Put of %s %q with no status.id; the store mints no id", kind, name)
	}
	e, err := encode(obj)
	if err != nil {
		return 0, err
	}
	f, _ := fieldsOf(obj)
	createdAt, updatedAt := *f.createdAt, *f.updatedAt
	var version int64
	err = o.s.write(ctx, func(q pgx.Tx) error {
		if key, ok := obj.(*v1.Key); ok {
			if err := checkKeyFence(ctx, q, key, ifVersion); err != nil {
				return err
			}
		}
		now := o.s.now()
		if updatedAt.IsZero() {
			updatedAt = now
		}
		if ifVersion == 0 {
			var err error
			id, version, createdAt, err = o.create(ctx, q, e, id, createdAt, updatedAt, now)
			return err
		}
		var err error
		version, createdAt, err = o.update(ctx, q, e, id, ifVersion, updatedAt)
		return err
	})
	if err != nil {
		return 0, named("Objects.Put", err)
	}
	setRow(obj, id, version, createdAt, updatedAt)
	return version, nil
}

// create is Put at version 0: the id must be unused, live or deleted;
// a live row of the kind and name is ErrNameTaken unless a declared
// Model lands on a discovered one, which is replaced in place.
func (o objects) create(ctx context.Context, q pgx.Tx, e *encoded, id string, createdAt, updatedAt, now time.Time) (string, int64, time.Time, error) {
	var used bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM objects WHERE id = $1)`, id).Scan(&used); err != nil {
		return "", 0, time.Time{}, err
	}
	if used {
		return "", 0, time.Time{}, fmt.Errorf("%w: id %s is already used", store.ErrVersionConflict, id)
	}
	var live struct {
		id, source string
		version    int64
		createdAt  string
	}
	err := q.QueryRow(ctx, `SELECT id, source, version, status->>'createdAt' FROM objects WHERE kind = $1 AND name = $2 AND deleted_at IS NULL FOR UPDATE`, e.kind, e.name).
		Scan(&live.id, &live.source, &live.version, &live.createdAt)
	switch {
	case err == nil:
		if e.kind != v1.KindModel || live.source != string(v1.SourceDiscovered) || e.source != string(v1.SourceDeclared) {
			return "", 0, time.Time{}, fmt.Errorf("%w: %s %q is %s", store.ErrNameTaken, e.kind, e.name, live.id)
		}
		// A declared Model shadows the discovered one in place: the id
		// kept, the version advanced, the observed half untouched.
		id, createdAt = live.id, parseTime(live.createdAt, now)
		version := live.version + 1
		status, err := e.stamp(id, version, createdAt, updatedAt)
		if err != nil {
			return "", 0, time.Time{}, err
		}
		_, err = q.Exec(ctx, `
			UPDATE objects SET owner = $2, source = $3, version = $4, spec = $5, status = $6, labels = $7, providers = $8, updated_at = $9
			WHERE id = $1`, id, e.owner, e.source, version, e.spec, status, e.labels, e.providers, updatedAt)
		return id, version, createdAt, err
	case !errors.Is(err, errNoRows):
		return "", 0, time.Time{}, err
	}
	if createdAt.IsZero() {
		createdAt = now
	}
	status, err := e.stamp(id, 1, createdAt, updatedAt)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO objects (kind, id, name, owner, source, version, spec, status, labels, providers, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $8, $9, $10, $11)`,
		e.kind, id, e.name, e.owner, e.source, e.spec, status, e.labels, e.providers, createdAt, updatedAt)
	switch {
	case violates(err, "objects_pkey"):
		return "", 0, time.Time{}, fmt.Errorf("%w: id %s is already used", store.ErrVersionConflict, id)
	case violates(err, "objects_live_name"):
		return "", 0, time.Time{}, fmt.Errorf("%w: %s %q is live", store.ErrNameTaken, e.kind, e.name)
	case err != nil:
		return "", 0, time.Time{}, err
	}
	return id, 1, createdAt, nil
}

// update is Put at a version: the live row of the id and kind, at that
// version, moves to the next one; a row that moved in between, here or
// under the row lock, is ErrVersionConflict.
func (o objects) update(ctx context.Context, q pgx.Tx, e *encoded, id string, ifVersion int64, updatedAt time.Time) (int64, time.Time, error) {
	var live struct {
		version   int64
		createdAt string
	}
	err := q.QueryRow(ctx, `SELECT version, status->>'createdAt' FROM objects WHERE id = $1 AND kind = $2 AND deleted_at IS NULL FOR UPDATE`, id, e.kind).
		Scan(&live.version, &live.createdAt)
	switch {
	case errors.Is(err, errNoRows):
		return 0, time.Time{}, fmt.Errorf("%w: %s %s has no live row", store.ErrNotFound, e.kind, id)
	case err != nil:
		return 0, time.Time{}, err
	case live.version != ifVersion:
		return 0, time.Time{}, fmt.Errorf("%w: %s %s is at version %d, the write at %d", store.ErrVersionConflict, e.kind, id, live.version, ifVersion)
	}
	createdAt := parseTime(live.createdAt, updatedAt)
	version := ifVersion + 1
	status, err := e.stamp(id, version, createdAt, updatedAt)
	if err != nil {
		return 0, time.Time{}, err
	}
	tag, err := q.Exec(ctx, `
		UPDATE objects SET name = $3, owner = $4, source = $5, version = $6, spec = $7, status = $8, labels = $9, providers = $10, updated_at = $11
		WHERE id = $1 AND version = $2`, id, ifVersion, e.name, e.owner, e.source, version, e.spec, status, e.labels, e.providers, updatedAt)
	switch {
	case violates(err, "objects_live_name"):
		return 0, time.Time{}, fmt.Errorf("%w: %s %q is live", store.ErrNameTaken, e.kind, e.name)
	case err != nil:
		return 0, time.Time{}, err
	case tag.RowsAffected() == 0:
		return 0, time.Time{}, fmt.Errorf("%w: %s %s moved past version %d", store.ErrVersionConflict, e.kind, id, ifVersion)
	}
	return version, createdAt, nil
}

// parseTime reads a stored RFC 3339 time, or answers def when the
// column carries none.
func parseTime(s string, def time.Time) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return def
	}
	return t
}

// one reads one object by a WHERE clause.
func (o objects) one(ctx context.Context, kind, op, where string, args ...any) (v1.Object, int64, error) {
	var obj v1.Object
	var version int64
	err := o.s.read(ctx, func(q querier) error {
		var spec, status []byte
		err := q.QueryRow(ctx, `SELECT `+objectColumns+` FROM objects WHERE kind = $1 AND deleted_at IS NULL AND `+where, args...).Scan(&spec, &status, &version)
		if errors.Is(err, errNoRows) {
			return fmt.Errorf("%w: %s %v", store.ErrNotFound, kind, args[1])
		}
		if err != nil {
			return err
		}
		obj, err = decode(kind, spec, status)
		return err
	})
	if err != nil {
		return nil, 0, named(op, err)
	}
	return obj, version, nil
}

// Get implements store.Objects.
func (o objects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	return o.one(ctx, kind, "Objects.Get", "id = $2", kind, id)
}

// ByName implements store.Objects.
func (o objects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	return o.one(ctx, kind, "Objects.ByName", "name = $2", kind, name)
}

// List implements store.Objects: the live rows of one kind by name, each
// filter a predicate on the column its index serves, one more row than
// the page so the cursor knows whether rows remain.
func (o objects) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]v1.Object, string, error) {
	after := ""
	if p.Cursor != "" {
		var err error
		if after, err = store.DecodeCursor(p.Cursor, kind, f); err != nil {
			return nil, "", err
		}
	}
	out := []v1.Object{}
	next := ""
	err := o.s.read(ctx, func(q querier) error {
		where := []string{"kind = $1", "deleted_at IS NULL"}
		args := []any{kind}
		arg := func(v any) string {
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
		if after != "" {
			where = append(where, "name > "+arg(after))
		}
		if f.Owner != "" {
			where = append(where, "owner = "+arg(f.Owner))
		}
		if len(f.Labels) > 0 {
			labels, err := labelsText(f.Labels)
			if err != nil {
				return err
			}
			where = append(where, "labels @> "+arg(labels)+"::jsonb")
		}
		if f.Source != "" {
			where = append(where, "source = "+arg(f.Source))
		}
		if f.Provider != "" {
			refs := []string{f.Provider}
			var name string
			err := q.QueryRow(ctx, `SELECT name FROM objects WHERE kind = $1 AND id = $2 AND deleted_at IS NULL`, v1.KindProvider, f.Provider).Scan(&name)
			switch {
			case err == nil:
				refs = append(refs, name)
			case !errors.Is(err, errNoRows):
				return err
			}
			where = append(where, "providers && "+arg(refs)+"::text[]")
		}
		if len(f.IDs) > 0 {
			where = append(where, "id = ANY("+arg(f.IDs)+"::text[])")
		}
		if f.Budget != "" {
			// The two forms of a Key's Budgets, each served by a partial
			// index of migration 1000004.
			n := arg(f.Budget)
			where = append(where, "(status->'budget'->>'id' = "+n+" OR status->'budgets' @> jsonb_build_array(jsonb_build_object('id', "+n+"::text)))")
		}
		rows, err := q.Query(ctx, `SELECT `+objectColumns+` FROM objects WHERE `+strings.Join(where, " AND ")+` ORDER BY name LIMIT `+arg(nullLimit(pageLimit(p.Limit))), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var spec, status []byte
			var version int64
			if err := rows.Scan(&spec, &status, &version); err != nil {
				return err
			}
			obj, err := decode(kind, spec, status)
			if err != nil {
				return err
			}
			out = append(out, obj)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", named("Objects.List", err)
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
		next = store.EncodeCursor(kind, f, out[len(out)-1].Name())
	}
	return out, next, nil
}

// Delete implements store.Objects: the row is marked, so its name is
// free at once and its id never is.
func (o objects) Delete(ctx context.Context, kind, id string) error {
	err := o.s.write(ctx, func(q pgx.Tx) error {
		if kind == v1.KindKey {
			if err := lockKeyWrites(ctx, q); err != nil {
				return err
			}
		}
		tag, err := q.Exec(ctx, `UPDATE objects SET deleted_at = $3 WHERE kind = $1 AND id = $2 AND deleted_at IS NULL`, kind, id, o.s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s %s", store.ErrNotFound, kind, id)
		}
		return nil
	})
	return named("Objects.Delete", err)
}

// PutStatus implements store.Objects: the incoming members concatenated
// onto the observed column, which leaves every member the document does
// not carry as it was.
func (o objects) PutStatus(ctx context.Context, kind, id string, observed any) error {
	if !v1.KnownKind(kind) {
		return fmt.Errorf("postgres: PutStatus of kind %q, not one of the four", kind)
	}
	doc, err := observedJSON(kind, observed)
	if err != nil {
		return err
	}
	err = o.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `UPDATE objects SET observed = observed || $3::jsonb WHERE kind = $1 AND id = $2 AND deleted_at IS NULL`, kind, id, string(doc))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: %s %s", store.ErrNotFound, kind, id)
		}
		return nil
	})
	return named("Objects.PutStatus", err)
}

// Prune implements store.Objects: deleted rows at or before before whose
// journal rows are all acknowledged or dropped.
func (o objects) Prune(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := o.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `
			DELETE FROM objects WHERE deleted_at IS NOT NULL AND deleted_at <= $1
			  AND NOT EXISTS (SELECT 1 FROM journal j WHERE j.object_id = objects.id AND j.acked_at IS NULL)`, before)
		n = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, named("Objects.Prune", err)
	}
	return n, nil
}

var _ store.Objects = objects{}
