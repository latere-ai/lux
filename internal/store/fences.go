// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// KeyFence permanently closes a Key name to credential writes. Owner and Labels
// assert the occupant's identity; the caller must separately authorize fencing.
type KeyFence struct {
	Name      string            `json:"name"`
	Owner     string            `json:"owner"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"createdAt"`
}

// KeyFences serializes installation with Key object and credential writes.
// Put waits for earlier writes to commit and blocks subsequent ones. Identical
// assertions are idempotent; an occupied name must match owner and exact labels.
// The inserted result is true only for a new committed row, so a caller can
// append an audit event in the same transaction without duplicating replays.
// No delete or reopen operation exists. Get returns ErrNotFound when unfenced.
type KeyFences interface {
	Put(context.Context, KeyFence) (fence KeyFence, inserted bool, err error)
	Get(context.Context, string) (KeyFence, error)
}

// Validate rejects incomplete assertions before acquiring a store lock.
func (f KeyFence) Validate() error {
	if f.Name == "" || f.Owner == "" {
		return errors.New("key fence requires a name and owner")
	}
	return nil
}

// Matches checks an exact identity assertion, treating empty label maps alike.
func (f KeyFence) Matches(owner string, labels map[string]string) bool {
	return f.Owner == owner && maps.Equal(f.Labels, labels)
}

// Clone prevents callers from modifying the durable assertion by map aliasing.
func (f KeyFence) Clone() KeyFence { f.Labels = maps.Clone(f.Labels); return f }

// DisableOnly permits a fenced Key's cleanup without changing its authority.
// Observations, version and update timestamp are store-owned and excluded.
func DisableOnly(old, next *v1.Key) bool {
	if old == nil || next == nil || !next.Spec.Disabled {
		return false
	}
	before := old.Spec
	before.Disabled = true
	return old.ID() == next.ID() && old.Status.Owner == next.Status.Owner &&
		old.Status.Prefix == next.Status.Prefix && old.Status.ExpiresAt.Equal(next.Status.ExpiresAt) &&
		reflect.DeepEqual(old.Status.Budget, next.Status.Budget) && reflect.DeepEqual(old.Status.Budgets, next.Status.Budgets) &&
		reflect.DeepEqual(old.Metadata, next.Metadata) && reflect.DeepEqual(before, next.Spec)
}
