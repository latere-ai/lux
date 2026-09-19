// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filemode

import (
	"context"
	"fmt"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// view is a store.Store over an inner one, the memory store or the
// Store a Transact hands out, with the control plane's writes refused:
// the mode is read-only for callers, not for the gateway.
type view struct {
	inner store.Store
	dir   string
}

// readOnly is ErrReadOnly with the developer detail naming the directory
// and the write that was refused.
func (v view) readOnly(what string) error {
	return fmt.Errorf("%w: desired state is the directory %s; %s", store.ErrReadOnly, v.dir, what)
}

func (v view) Objects() store.Objects         { return roObjects{v.inner.Objects(), v} }
func (v view) KeyFences() store.KeyFences     { return roKeyFences{v.inner.KeyFences(), v} }
func (v view) Keys() store.Keys               { return roKeys{v.inner.Keys(), v} }
func (v view) Credentials() store.Credentials { return roCredentials{v} }
func (v view) Counters() store.Counters       { return v.inner.Counters() }
func (v view) Leases() store.Leases           { return v.inner.Leases() }
func (v view) Journal() store.Journal         { return v.inner.Journal() }
func (v view) Tunnels() store.Tunnels         { return v.inner.Tunnels() }
func (v view) Usage() store.Usage             { return v.inner.Usage() }

func (v view) Transact(ctx context.Context, fn func(tx store.Store) error) error {
	return v.inner.Transact(ctx, func(tx store.Store) error { return fn(view{inner: tx, dir: v.dir}) })
}

func (v view) Ready(ctx context.Context) error { return v.inner.Ready(ctx) }
func (v view) Close() error                    { return v.inner.Close() }

// roObjects admits what discovery writes, a Model whose source is
// discovered, and refuses every other Put and Delete.
type roObjects struct {
	store.Objects
	v view
}

func (o roObjects) Put(ctx context.Context, obj v1.Object, ifVersion int64) (int64, error) {
	m, ok := obj.(*v1.Model)
	if !ok || m.Status.Source != v1.SourceDiscovered {
		return 0, o.v.readOnly(fmt.Sprintf("Objects.Put of a declared %s %q", kindOf(obj), nameOf(obj)))
	}
	if ifVersion > 0 {
		existing, _, err := o.Get(ctx, v1.KindModel, m.Status.ID)
		if err != nil {
			return 0, err
		}
		if e, ok := existing.(*v1.Model); ok && e.Status.Source != v1.SourceDiscovered {
			return 0, o.v.readOnly(fmt.Sprintf("Objects.Put over the declared Model %q", e.Metadata.Name))
		}
	}
	// A discovered Model in this mode is the directory's, whoever found it.
	m.Status.Owner = Subject
	return o.Objects.Put(ctx, obj, ifVersion)
}

func (o roObjects) Delete(ctx context.Context, kind, id string) error {
	existing, _, err := o.Get(ctx, kind, id)
	if err != nil {
		return err
	}
	if m, ok := existing.(*v1.Model); ok && m.Status.Source == v1.SourceDiscovered {
		return o.Objects.Delete(ctx, kind, id)
	}
	return o.v.readOnly(fmt.Sprintf("Objects.Delete of the declared %s %q", kind, existing.Name()))
}

func kindOf(obj v1.Object) string {
	if obj == nil {
		return "object"
	}
	return obj.Kind()
}

func nameOf(obj v1.Object) string {
	if obj == nil {
		return ""
	}
	return obj.Name()
}

// roKeys answers lookups and refuses the two writes.
type roKeys struct {
	store.Keys
	v view
}

func (k roKeys) Put(context.Context, string, string) error {
	return k.v.readOnly("Keys.Put; a file-mode Key's value comes from its variable")
}

func (k roKeys) Delete(context.Context, string) error {
	return k.v.readOnly("Keys.Delete; a file-mode Key is removed from the directory")
}

// roCredentials refuses every call: nothing is sealed in this mode, and
// a Provider's value is read through the Store's CredentialValue.
type roCredentials struct{ v view }

func (c roCredentials) Put(context.Context, string, store.Sealed) error {
	return c.v.readOnly("Credentials.Put; nothing is sealed in this mode")
}

func (c roCredentials) Rewrap(context.Context, string, int, []byte, []byte) error {
	return c.v.readOnly("Credentials.Rewrap; nothing is sealed in this mode")
}

func (c roCredentials) Get(context.Context, string) (store.Sealed, error) {
	return store.Sealed{}, c.v.readOnly("Credentials.Get; a value is read through CredentialValue")
}

func (c roCredentials) Delete(context.Context, string) error {
	return c.v.readOnly("Credentials.Delete; nothing is sealed in this mode")
}

func (c roCredentials) List(context.Context) ([]string, error) {
	return nil, c.v.readOnly("Credentials.List; nothing is sealed in this mode")
}

type roKeyFences struct {
	store.KeyFences
	v view
}

func (f roKeyFences) Put(context.Context, store.KeyFence) (store.KeyFence, error) {
	return store.KeyFence{}, f.v.readOnly("KeyFences.Put")
}
