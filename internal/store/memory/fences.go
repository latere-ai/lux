// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

type keyFences struct{ s *Store }

func (f keyFences) Put(ctx context.Context, input store.KeyFence) (store.KeyFence, bool, error) {
	if err := input.Validate(); err != nil {
		return store.KeyFence{}, false, err
	}
	var result store.KeyFence
	var inserted bool
	err := f.s.write(ctx, func(st *state) error {
		if old, ok := st.fences[input.Name]; ok {
			if !old.Matches(input.Owner, input.Labels) {
				return store.ErrFenceConflict
			}
			result = old.Clone()
			return nil
		}
		if id, ok := st.names[nameKey(v1.KindKey, input.Name)]; ok {
			row := st.objects[id]
			if !input.Matches(row.owner, row.labels) {
				return store.ErrFenceConflict
			}
		}
		input.CreatedAt = f.s.now()
		st.fences[input.Name] = input.Clone()
		inserted = true
		result = input.Clone()
		return nil
	})
	return result, inserted && err == nil, err
}
func (f keyFences) Get(ctx context.Context, name string) (store.KeyFence, error) {
	var result store.KeyFence
	err := f.s.read(ctx, func(st *state) error {
		row, ok := st.fences[name]
		if !ok {
			return store.ErrNotFound
		}
		result = row.Clone()
		return nil
	})
	return result, err
}

func checkKeyFence(st *state, next *v1.Key, version int64) error {
	oldRow := st.objects[next.ID()]
	_, target := st.fences[next.Name()]
	_, source := st.fences[oldRow.name]
	if !target && !source {
		return nil
	}
	old, _ := oldRow.obj.(*v1.Key)
	if version > 0 && oldRow.live() && store.DisableOnly(old, next) {
		return nil
	}
	return store.ErrKeyFenced
}
