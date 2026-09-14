// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"fmt"

	"latere.ai/x/lux/internal/store"
)

type keys struct{ s *Store }

// hashLength is SHA-256 as lower-case hex.
const hashLength = 64

// checkHash holds a hash to 64 lower-case hex characters.
func checkHash(hash string) error {
	if len(hash) != hashLength {
		return fmt.Errorf("memory: a key hash is %d lower-case hex characters, this one is %d", hashLength, len(hash))
	}
	for i := range len(hash) {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("memory: a key hash is lower-case hex; byte %d is %q", i, c)
		}
	}
	return nil
}

// Put implements store.Keys.
func (k keys) Put(ctx context.Context, keyID, hash string) error {
	if keyID == "" {
		return fmt.Errorf("memory: Keys.Put with no key id")
	}
	if err := checkHash(hash); err != nil {
		return err
	}
	return k.s.write(ctx, func(st *state) error {
		if other, ok := st.hashes[hash]; ok && other != keyID {
			return fmt.Errorf("%w: the hash is registered to another key", store.ErrHashTaken)
		}
		if old, ok := st.keyHash[keyID]; ok {
			delete(st.hashes, old)
		}
		st.hashes[hash] = keyID
		st.keyHash[keyID] = hash
		return nil
	})
}

// ByHash implements store.Keys.
func (k keys) ByHash(ctx context.Context, hash string) (string, error) {
	var id string
	err := k.s.read(ctx, func(st *state) error {
		var ok bool
		if id, ok = st.hashes[hash]; !ok {
			return fmt.Errorf("%w: no key has this hash", store.ErrNotFound)
		}
		return nil
	})
	return id, err
}

// Delete implements store.Keys.
func (k keys) Delete(ctx context.Context, keyID string) error {
	return k.s.write(ctx, func(st *state) error {
		old, ok := st.keyHash[keyID]
		if !ok {
			return fmt.Errorf("%w: key %s has no hash", store.ErrNotFound, keyID)
		}
		delete(st.hashes, old)
		delete(st.keyHash, keyID)
		return nil
	})
}
