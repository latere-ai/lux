// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"latere.ai/x/lux/internal/store"
)

type credentials struct{ s *Store }

// copySealed is a Sealed whose slices are the store's own or the
// caller's own, never shared.
func copySealed(s store.Sealed) store.Sealed {
	return store.Sealed{
		Version:      s.Version,
		WrappedKey:   slices.Clone(s.WrappedKey),
		WrappedNonce: slices.Clone(s.WrappedNonce),
		Ciphertext:   slices.Clone(s.Ciphertext),
		Nonce:        slices.Clone(s.Nonce),
	}
}

// Put implements store.Credentials.
func (c credentials) Put(ctx context.Context, providerID string, s store.Sealed) error {
	if providerID == "" {
		return errors.New("memory: Credentials.Put with no provider id")
	}
	return c.s.write(ctx, func(st *state) error {
		st.creds[providerID] = copySealed(s)
		return nil
	})
}

// Rewrap implements store.Credentials.
func (c credentials) Rewrap(ctx context.Context, providerID string, ifVersion int, wrappedKey, wrappedNonce []byte) error {
	return c.s.write(ctx, func(st *state) error {
		row, ok := st.creds[providerID]
		if !ok {
			return fmt.Errorf("%w: provider %s has no credential", store.ErrNotFound, providerID)
		}
		if row.Version != ifVersion {
			return fmt.Errorf("%w: credential of %s is at version %d, the re-wrap at %d", store.ErrVersionConflict, providerID, row.Version, ifVersion)
		}
		row.WrappedKey, row.WrappedNonce = slices.Clone(wrappedKey), slices.Clone(wrappedNonce)
		st.creds[providerID] = row
		return nil
	})
}

// Get implements store.Credentials.
func (c credentials) Get(ctx context.Context, providerID string) (store.Sealed, error) {
	var out store.Sealed
	err := c.s.read(ctx, func(st *state) error {
		row, ok := st.creds[providerID]
		if !ok {
			return fmt.Errorf("%w: provider %s has no credential", store.ErrNotFound, providerID)
		}
		out = copySealed(row)
		return nil
	})
	return out, err
}

// Delete implements store.Credentials.
func (c credentials) Delete(ctx context.Context, providerID string) error {
	return c.s.write(ctx, func(st *state) error {
		if _, ok := st.creds[providerID]; !ok {
			return fmt.Errorf("%w: provider %s has no credential", store.ErrNotFound, providerID)
		}
		delete(st.creds, providerID)
		return nil
	})
}

// List implements store.Credentials.
func (c credentials) List(ctx context.Context) ([]string, error) {
	var ids []string
	err := c.s.read(ctx, func(st *state) error {
		ids = slices.Sorted(maps.Keys(st.creds))
		return nil
	})
	return ids, err
}
