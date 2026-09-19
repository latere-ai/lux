// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// objectRow is one object: the control plane's half as a stored clone,
// the observed half as the kind's store struct, and the columns the
// filters and the indexes read.
type objectRow struct {
	kind, id, name, owner string
	source                v1.Source
	version               int64
	obj                   v1.Object
	observed              any
	labels                map[string]string
	targets               []string
	createdAt, updatedAt  time.Time
	deletedAt             time.Time
}

func (r objectRow) live() bool { return r.deletedAt.IsZero() }

func nameKey(kind, name string) string { return kind + "/" + name }

type objects struct{ s *Store }

// Put implements store.Objects.
func (o objects) Put(ctx context.Context, obj v1.Object, ifVersion int64) (int64, error) {
	if obj == nil {
		return 0, errors.New("memory: Put of a nil object")
	}
	f, err := fieldsOf(obj)
	if err != nil {
		return 0, err
	}
	kind, name := obj.Kind(), obj.Name()
	if name == "" {
		return 0, fmt.Errorf("memory: Put of a %s with no metadata.name", kind)
	}
	if ifVersion < 0 {
		return 0, fmt.Errorf("memory: Put of %s %q at version %d, below 0", kind, name, ifVersion)
	}
	if *f.id == "" {
		return 0, fmt.Errorf("memory: Put of %s %q with no status.id; the store mints no id", kind, name)
	}
	var version int64
	err = o.s.write(ctx, func(st *state) error {
		if key, ok := obj.(*v1.Key); ok {
			if err := checkKeyFence(st, key, ifVersion); err != nil {
				return err
			}
		}
		now := o.s.now()
		row := objectRow{kind: kind, id: *f.id, name: name, owner: *f.owner, version: 1, createdAt: *f.createdAt, updatedAt: *f.updatedAt}
		if ifVersion == 0 {
			if _, used := st.objects[row.id]; used {
				return fmt.Errorf("%w: id %s is already used", store.ErrVersionConflict, row.id)
			}
			if liveID, taken := st.names[nameKey(kind, name)]; taken {
				live := st.objects[liveID]
				if kind != v1.KindModel || live.source != v1.SourceDiscovered || sourceOf(obj) != v1.SourceDeclared {
					return fmt.Errorf("%w: %s %q is %s", store.ErrNameTaken, kind, name, liveID)
				}
				// A declared Model shadows the discovered one in place.
				row.id, row.version, row.createdAt, row.observed = live.id, live.version+1, live.createdAt, live.observed
			}
			if row.createdAt.IsZero() {
				row.createdAt = now
			}
		} else {
			live, ok := st.objects[row.id]
			if !ok || !live.live() || live.kind != kind {
				return fmt.Errorf("%w: %s %s has no live row", store.ErrNotFound, kind, row.id)
			}
			if live.version != ifVersion {
				return fmt.Errorf("%w: %s %s is at version %d, the write at %d", store.ErrVersionConflict, kind, row.id, live.version, ifVersion)
			}
			if live.name != name {
				if other, taken := st.names[nameKey(kind, name)]; taken && other != row.id {
					return fmt.Errorf("%w: %s %q is %s", store.ErrNameTaken, kind, name, other)
				}
				delete(st.names, nameKey(kind, live.name))
			}
			row.version, row.createdAt, row.observed = live.version+1, live.createdAt, live.observed
		}
		if row.updatedAt.IsZero() {
			row.updatedAt = now
		}
		*f.id, *f.version, *f.createdAt, *f.updatedAt = row.id, row.version, row.createdAt, row.updatedAt
		stored, err := clone(obj)
		if err != nil {
			return err
		}
		prepare(stored)
		if m, ok := obj.(*v1.Model); ok && m.Status.Source == "" {
			m.Status.Source = v1.SourceDeclared
		}
		row.obj, row.source, row.labels, row.targets = stored, sourceOf(stored), maps.Clone(f.labels), targetsOf(stored)
		st.objects[row.id] = row
		st.names[nameKey(kind, name)] = row.id
		version = row.version
		return nil
	})
	return version, err
}

// Get implements store.Objects.
func (o objects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	var obj v1.Object
	var version int64
	err := o.s.read(ctx, func(st *state) error {
		row, ok := st.objects[id]
		if !ok || !row.live() || row.kind != kind {
			return fmt.Errorf("%w: %s %s", store.ErrNotFound, kind, id)
		}
		var err error
		obj, version, err = render(row)
		return err
	})
	return obj, version, err
}

// ByName implements store.Objects.
func (o objects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	var obj v1.Object
	var version int64
	err := o.s.read(ctx, func(st *state) error {
		id, ok := st.names[nameKey(kind, name)]
		if !ok {
			return fmt.Errorf("%w: %s %q", store.ErrNotFound, kind, name)
		}
		var err error
		obj, version, err = render(st.objects[id])
		return err
	})
	return obj, version, err
}

// render is the object a read returns: a clone of the stored control
// half with the observed half laid over it and the row's version.
func render(row objectRow) (v1.Object, int64, error) {
	obj, err := clone(row.obj)
	if err != nil {
		return nil, 0, err
	}
	apply(obj, row.observed)
	return obj, row.version, nil
}

// List implements store.Objects.
func (o objects) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]v1.Object, string, error) {
	var out []v1.Object
	var next string
	err := o.s.read(ctx, func(st *state) error {
		after := ""
		if p.Cursor != "" {
			var err error
			if after, err = store.DecodeCursor(p.Cursor, kind, f); err != nil {
				return err
			}
		}
		providerName := ""
		if f.Provider != "" {
			if prv, ok := st.objects[f.Provider]; ok && prv.live() && prv.kind == v1.KindProvider {
				providerName = prv.name
			}
		}
		var rows []objectRow
		for _, row := range st.objects {
			if row.kind == kind && row.live() && row.name > after && matches(row, f, providerName) {
				rows = append(rows, row)
			}
		}
		slices.SortFunc(rows, func(a, b objectRow) int { return strings.Compare(a.name, b.name) })
		if p.Limit > 0 && len(rows) > p.Limit {
			rows = rows[:p.Limit]
			next = store.EncodeCursor(kind, f, rows[len(rows)-1].name)
		}
		out = make([]v1.Object, 0, len(rows))
		for _, row := range rows {
			obj, _, err := render(row)
			if err != nil {
				return err
			}
			out = append(out, obj)
		}
		return nil
	})
	return out, next, err
}

// matches applies every member of the filter to one live row.
func matches(row objectRow, f store.Filter, providerName string) bool {
	if f.Owner != "" && row.owner != f.Owner {
		return false
	}
	for k, v := range f.Labels {
		if row.labels[k] != v {
			return false
		}
	}
	if f.Source != "" && string(row.source) != f.Source {
		return false
	}
	if f.Provider != "" {
		hit := false
		for _, t := range row.targets {
			if t == f.Provider || (providerName != "" && t == providerName) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if len(f.IDs) > 0 && !slices.Contains(f.IDs, row.id) {
		return false
	}
	return true
}

// Delete implements store.Objects.
func (o objects) Delete(ctx context.Context, kind, id string) error {
	return o.s.write(ctx, func(st *state) error {
		row, ok := st.objects[id]
		if !ok || !row.live() || row.kind != kind {
			return fmt.Errorf("%w: %s %s", store.ErrNotFound, kind, id)
		}
		row.deletedAt = o.s.now()
		st.objects[id] = row
		delete(st.names, nameKey(kind, row.name))
		return nil
	})
}

// PutStatus implements store.Objects.
func (o objects) PutStatus(ctx context.Context, kind, id string, observed any) error {
	if !v1.KnownKind(kind) {
		return fmt.Errorf("memory: PutStatus of kind %q, not one of the four", kind)
	}
	return o.s.write(ctx, func(st *state) error {
		row, ok := st.objects[id]
		if !ok || !row.live() || row.kind != kind {
			return fmt.Errorf("%w: %s %s", store.ErrNotFound, kind, id)
		}
		merged, err := merge(kind, row.observed, observed)
		if err != nil {
			return err
		}
		row.observed = merged
		st.objects[id] = row
		return nil
	})
}

// Prune implements store.Objects.
func (o objects) Prune(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := o.s.write(ctx, func(st *state) error {
		pending := map[string]bool{}
		for _, e := range st.journal {
			if e.AckedAt.IsZero() {
				pending[e.ObjectID] = true
			}
		}
		for id, row := range st.objects {
			if !row.live() && !row.deletedAt.After(before) && !pending[id] {
				delete(st.objects, id)
				n++
			}
		}
		return nil
	})
	return n, err
}
