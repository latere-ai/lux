// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

func keyFences(t *testing.T, s store.Store) {
	ctx := t.Context()
	input := store.KeyFence{Name: "reserved", Owner: subject, Labels: map[string]string{"tenant": "one"}}
	_, err := s.KeyFences().Get(ctx, input.Name)
	wantErr(t, err, store.ErrNotFound, "no fence")
	for _, bad := range []store.KeyFence{{}, {Name: "x"}, {Owner: subject}} {
		_, err := s.KeyFences().Put(ctx, bad)
		truth(t, err != nil, "invalid fence")
	}
	first, err := s.KeyFences().Put(ctx, input)
	noErr(t, err, "absent fence")
	truth(t, !first.CreatedAt.IsZero(), "timestamp")
	input.Labels["tenant"] = "changed"
	first.Labels["tenant"] = "changed"
	stored, err := s.KeyFences().Get(ctx, "reserved")
	noErr(t, err, "get")
	equal(t, stored.Labels["tenant"], "one", "input/output isolation")
	originalTime := stored.CreatedAt
	replay, err := s.KeyFences().Put(ctx, stored)
	noErr(t, err, "replay")
	truth(t, replay.CreatedAt.Equal(originalTime), "replay timestamp")
	stored.Labels["tenant"] = "changed"
	_, err = s.KeyFences().Put(ctx, stored)
	wantErr(t, err, store.ErrFenceConflict, "labels conflict")
	stored.Labels["tenant"] = "one"
	stored.Owner = "other"
	_, err = s.KeyFences().Put(ctx, stored)
	wantErr(t, err, store.ErrFenceConflict, "owner conflict")
	for _, disabled := range []bool{false, true} {
		k := key("reserved")
		k.Spec.Disabled = disabled
		_, err := s.Objects().Put(ctx, k, 0)
		wantErr(t, err, store.ErrKeyFenced, "fenced create")
	}
	missing := key("reserved")
	missing.Spec.Disabled = true
	_, err = s.Objects().Put(ctx, missing, 1)
	wantErr(t, err, store.ErrKeyFenced, "missing fenced update")
	k := key("unfenced")
	_, err = s.Objects().Put(ctx, k, 0)
	noErr(t, err, "unfenced create")
	k.Metadata.Name = "reserved"
	k.Spec.Disabled = true
	_, err = s.Objects().Put(ctx, k, 1)
	wantErr(t, err, store.ErrKeyFenced, "rename into fence")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.KeyFences().Get(canceled, "reserved")
	wantErr(t, err, context.Canceled, "canceled get")
	_, err = s.KeyFences().Put(canceled, store.KeyFence{Name: "cancel", Owner: subject})
	wantErr(t, err, context.Canceled, "canceled put")
}

func fencedKeyCleanup(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("occupied")
	k.Metadata.Labels = map[string]string{"tenant": "one"}
	_, err := s.Objects().Put(ctx, k, 0)
	noErr(t, err, "create")
	noErr(t, s.Keys().Put(ctx, k.ID(), hash(0)), "initial hash")
	for _, bad := range []store.KeyFence{{Name: k.Name(), Owner: "wrong", Labels: k.Metadata.Labels}, {Name: k.Name(), Owner: subject}, {Name: k.Name(), Owner: subject, Labels: map[string]string{"tenant": "one", "extra": "x"}}} {
		_, err := s.KeyFences().Put(ctx, bad)
		wantErr(t, err, store.ErrFenceConflict, "occupant mismatch")
	}
	_, err = s.KeyFences().Put(ctx, store.KeyFence{Name: k.Name(), Owner: subject, Labels: k.Metadata.Labels})
	noErr(t, err, "occupied fence")
	for _, h := range []string{hash(0), hash(1)} {
		wantErr(t, s.Keys().Put(ctx, k.ID(), h), store.ErrKeyFenced, "hash write")
	}
	_, err = s.Objects().Put(ctx, k, k.Status.Version)
	wantErr(t, err, store.ErrKeyFenced, "enabled write")
	changes := map[string]func(*v1.Key){
		"rename":          func(k *v1.Key) { k.Metadata.Name = "escape" },
		"owner":           func(k *v1.Key) { k.Status.Owner = "other" },
		"labels":          func(k *v1.Key) { k.Metadata.Labels = map[string]string{} },
		"annotations":     func(k *v1.Key) { k.Metadata.Annotations = map[string]string{"extra": "x"} },
		"prefix":          func(k *v1.Key) { k.Status.Prefix = "different" },
		"models":          func(k *v1.Key) { k.Spec.Models = []string{"*"} },
		"ttl":             func(k *v1.Key) { k.Spec.TTL = "1h" },
		"expires":         func(k *v1.Key) { k.Status.ExpiresAt = time.Now().Add(time.Hour) },
		"spec expires":    func(k *v1.Key) { k.Spec.ExpiresAt = time.Now().Add(time.Hour) },
		"budget":          func(k *v1.Key) { k.Spec.Budget = "other" },
		"budget identity": func(k *v1.Key) { k.Status.Budget = &v1.BudgetRef{Name: "other", ID: "bud_other"} },
		"unpriced":        func(k *v1.Key) { k.Spec.AllowUnpriced = true },
		"passthrough":     func(k *v1.Key) { k.Spec.Passthrough = true },
		"limits":          func(k *v1.Key) { n := 100; k.Spec.Limits.RequestsPerMinute = &n },
	}
	for name, change := range changes {
		obj, version, err := s.Objects().Get(ctx, v1.KindKey, k.ID())
		noErr(t, err, "read")
		next := as[*v1.Key](t, obj)
		next.Spec.Disabled = true
		change(next)
		_, err = s.Objects().Put(ctx, next, version)
		wantErr(t, err, store.ErrKeyFenced, name)
	}
	obj, version, err := s.Objects().Get(ctx, v1.KindKey, k.ID())
	noErr(t, err, "read")
	next := as[*v1.Key](t, obj)
	next.Spec.Disabled = true
	_, err = s.Objects().Put(ctx, next, version)
	noErr(t, err, "disable only")
	_, err = s.Objects().Put(ctx, next, next.Status.Version)
	noErr(t, err, "disabled replay")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindKey, k.ID(), store.KeyObserved{LastUsedAt: time.Now()}), "observed write")
	noErr(t, s.Keys().Delete(ctx, k.ID()), "delete hash")
	noErr(t, s.Objects().Delete(ctx, v1.KindKey, k.ID()), "delete")
	_, err = s.Objects().Prune(ctx, time.Now().Add(time.Hour))
	noErr(t, err, "prune")
	_, err = s.KeyFences().Get(ctx, k.Name())
	noErr(t, err, "fence survives prune")
	_, err = s.Objects().Put(ctx, key(k.Name()), 0)
	wantErr(t, err, store.ErrKeyFenced, "recreation")
}

func fenceRollback(t *testing.T, s store.Store) {
	ctx := t.Context()
	abort := errors.New("abort")
	input := store.KeyFence{Name: "rollback", Owner: subject}
	err := s.Transact(ctx, func(tx store.Store) error {
		_, err := tx.KeyFences().Put(ctx, input)
		if err != nil {
			return err
		}
		return abort
	})
	wantErr(t, err, abort, "rollback")
	_, err = s.KeyFences().Get(ctx, input.Name)
	wantErr(t, err, store.ErrNotFound, "rolled back fence")
	k := key(input.Name)
	err = s.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, k, 0); err != nil {
			return err
		}
		if _, err := tx.KeyFences().Put(ctx, input); err != nil {
			return err
		}
		if err := tx.Keys().Put(ctx, k.ID(), hash(0)); !errors.Is(err, store.ErrKeyFenced) {
			return err
		}
		_, err := tx.KeyFences().Get(ctx, input.Name)
		return err
	})
	noErr(t, err, "savepoint refusal preserves transaction")
	got, err := s.KeyFences().Get(ctx, input.Name)
	noErr(t, err, "committed fence")
	input.CreatedAt = got.CreatedAt
	truth(t, got.Name == input.Name && got.Owner == input.Owner && len(got.Labels) == 0, "committed assertion")
}
