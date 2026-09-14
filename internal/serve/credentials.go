// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"

	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
)

// A credential source answers the value the gateway injects toward one
// Provider, for the life of one outbound request, and nil for a Provider
// that holds none, which is the shape spec 004's CredentialSource takes
// from this package.

// StoreCredentials opens the sealed row of a Provider under the key
// encryption keys: the server mode, on the memory and the Postgres store.
type StoreCredentials struct {
	Credentials store.Credentials
	Keys        *secrets.Keyring
}

// Credential returns the plaintext of providerID's credential, nil when
// the Provider stores none, and the store's or the custody's failure
// otherwise.
func (s *StoreCredentials) Credential(ctx context.Context, providerID string) ([]byte, error) {
	row, err := s.Credentials.Get(ctx, providerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credential of %s: %w", providerID, err)
	}
	return s.Keys.Open(providerID, row)
}

// FileCredentials reads the value the file mode holds in memory from the
// variable the manifest named; nothing is sealed and nothing is opened.
type FileCredentials struct {
	Files *filemode.Store
}

// Credential returns the value read from the environment for providerID,
// and nil for a Provider that names none.
func (f *FileCredentials) Credential(_ context.Context, providerID string) ([]byte, error) {
	v, ok := f.Files.CredentialValue(providerID)
	if !ok {
		return nil, nil
	}
	return []byte(v), nil
}

// credentialSource is the seam the jobs take, satisfied by the two
// types above and by spec 004's interface of the same shape.
type credentialSource interface {
	Credential(ctx context.Context, providerID string) ([]byte, error)
}
