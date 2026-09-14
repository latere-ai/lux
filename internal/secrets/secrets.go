// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lux/internal/store"
)

// The fixed suite. There is no in-band format version: a second suite
// would be a new column by migration, not a byte a reader switches on.
const (
	// KeySize is the size of a key encryption key and of a data key.
	KeySize = 32
	// NonceSize is the AES-GCM nonce size, for both AEADs.
	NonceSize = 12
	// TagSize is the AES-GCM authentication tag size.
	TagSize = 16
	// WrappedKeySize is a data key plus its tag.
	WrappedKeySize = KeySize + TagSize
	// MaxKeys is how many keys LUX_SECRETS_KEK may list.
	MaxKeys = 8
	// Encoded is the length of one key in standard base64 with padding.
	Encoded = 44
)

// Keyring is the parsed LUX_SECRETS_KEK: one to MaxKeys keys, of which
// the first wraps every new data key and every one is tried to open one.
// It has no String method and appears in no encoding, so the key
// material stays where it was parsed.
type Keyring struct {
	keys [][]byte
}

// Parse reads the comma separated list of standard base64 keys. A
// problem names the failing key by its position in the list, one-based,
// and never by its value; the returned error carries no variable name,
// so a caller prefixes the one it read.
func Parse(raw string) (*Keyring, error) {
	var entries []string
	for e := range strings.SplitSeq(raw, ",") {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("lists no key")
	}
	if len(entries) > MaxKeys {
		return nil, fmt.Errorf("lists %d keys, at most %d", len(entries), MaxKeys)
	}
	k := &Keyring{keys: make([][]byte, 0, len(entries))}
	for i, e := range entries {
		key, err := base64.StdEncoding.Strict().DecodeString(e)
		if err != nil {
			return nil, fmt.Errorf("key %d of %d is not standard base64 with padding (%d characters for a 32-byte key)", i+1, len(entries), Encoded)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("key %d of %d decodes to %d bytes, not %d", i+1, len(entries), len(key), KeySize)
		}
		k.keys = append(k.keys, key)
	}
	return k, nil
}

// Len is how many keys are listed.
func (k *Keyring) Len() int { return len(k.keys) }

// AAD is the additional data both AEADs bind: the UTF-8 bytes of the
// provider id with its prefix, a colon, and the version in decimal.
func AAD(providerID string, version int) []byte {
	return []byte(providerID + ":" + strconv.Itoa(version))
}

// Seal encrypts value for the credential at version of providerID: a
// fresh data key and two fresh nonces per call, so two seals of one
// value share no byte. The data key is zeroed before Seal returns.
func (k *Keyring) Seal(providerID string, version int, value []byte) (store.Sealed, error) {
	if err := checkSubject(providerID, version); err != nil {
		return store.Sealed{}, err
	}
	dataKey := make([]byte, KeySize)
	defer clear(dataKey)
	if _, err := rand.Read(dataKey); err != nil {
		return store.Sealed{}, fmt.Errorf("secrets: reading a data key: %w", err)
	}
	aad := AAD(providerID, version)
	nonce, ciphertext, err := seal(dataKey, value, aad)
	if err != nil {
		return store.Sealed{}, err
	}
	wrappedNonce, wrappedKey, err := seal(k.keys[0], dataKey, aad)
	if err != nil {
		return store.Sealed{}, err
	}
	return store.Sealed{Version: version, WrappedKey: wrappedKey, WrappedNonce: wrappedNonce, Ciphertext: ciphertext, Nonce: nonce}, nil
}

// Open decrypts the value of the row s of providerID, trying every listed
// key against the wrapped data key. The data key is zeroed before Open
// returns; the value is the caller's for the life of one request.
func (k *Keyring) Open(providerID string, s store.Sealed) ([]byte, error) {
	dataKey, _, err := k.unwrap(providerID, s)
	if err != nil {
		return nil, err
	}
	defer clear(dataKey)
	if len(s.Nonce) != NonceSize {
		return nil, fmt.Errorf("secrets: provider %s: the value nonce is %d bytes, not %d", providerID, len(s.Nonce), NonceSize)
	}
	value, err := open(dataKey, s.Nonce, s.Ciphertext, AAD(providerID, s.Version))
	if err != nil {
		return nil, fmt.Errorf("secrets: provider %s: the value ciphertext does not open under its data key at version %d", providerID, s.Version)
	}
	return value, nil
}

// Rewrap returns the row with its data key wrapped under the first
// listed key and a fresh wrapped nonce, and true; or the row as it was
// and false when it was already under the first key. The value
// ciphertext and its nonce are returned as they were and are never
// decrypted.
func (k *Keyring) Rewrap(providerID string, s store.Sealed) (store.Sealed, bool, error) {
	dataKey, position, err := k.unwrap(providerID, s)
	if err != nil {
		return store.Sealed{}, false, err
	}
	defer clear(dataKey)
	out := store.Sealed{
		Version:      s.Version,
		WrappedKey:   slices.Clone(s.WrappedKey),
		WrappedNonce: slices.Clone(s.WrappedNonce),
		Ciphertext:   slices.Clone(s.Ciphertext),
		Nonce:        slices.Clone(s.Nonce),
	}
	if position == 1 {
		return out, false, nil
	}
	wrappedNonce, wrappedKey, err := seal(k.keys[0], dataKey, AAD(providerID, s.Version))
	if err != nil {
		return store.Sealed{}, false, err
	}
	out.WrappedKey, out.WrappedNonce = wrappedKey, wrappedNonce
	return out, true, nil
}

// Unwrap reports which listed key, by one-based position, opens the
// wrapped data key of the row s of providerID, and touches the value
// ciphertext not at all. The data key is zeroed before Unwrap returns.
// It is the start-up check and luxd check's credentials row.
func (k *Keyring) Unwrap(providerID string, s store.Sealed) (position int, err error) {
	dataKey, position, err := k.unwrap(providerID, s)
	if err != nil {
		return 0, err
	}
	clear(dataKey)
	return position, nil
}

// unwrap opens the wrapped data key under the first listed key that
// does, returning the key and the position that opened it.
func (k *Keyring) unwrap(providerID string, s store.Sealed) ([]byte, int, error) {
	if err := checkSubject(providerID, s.Version); err != nil {
		return nil, 0, err
	}
	if len(s.WrappedKey) != WrappedKeySize {
		return nil, 0, fmt.Errorf("secrets: provider %s: the wrapped data key is %d bytes, not %d", providerID, len(s.WrappedKey), WrappedKeySize)
	}
	if len(s.WrappedNonce) != NonceSize {
		return nil, 0, fmt.Errorf("secrets: provider %s: the wrapped nonce is %d bytes, not %d", providerID, len(s.WrappedNonce), NonceSize)
	}
	aad := AAD(providerID, s.Version)
	for i, key := range k.keys {
		dataKey, err := open(key, s.WrappedNonce, s.WrappedKey, aad)
		if err == nil {
			return dataKey, i + 1, nil
		}
	}
	return nil, 0, fmt.Errorf("secrets: provider %s: no listed key opens the wrapped data key at version %d (%d tried)", providerID, s.Version, len(k.keys))
}

// checkSubject refuses the two inputs the additional data cannot do
// without: an empty id or a version below 1 would bind a row to nothing.
func checkSubject(providerID string, version int) error {
	if providerID == "" {
		return errors.New("secrets: no provider id")
	}
	if version < 1 {
		return fmt.Errorf("secrets: provider %s: credential version %d, below 1", providerID, version)
	}
	return nil
}

// seal is one AES-256-GCM encryption under key with a fresh nonce.
func seal(key, plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secrets: reading a nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

// open is one AES-256-GCM decryption under key.
func open(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, aad)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return aead, nil
}

// Check opens the wrapped data key, and never the value, of every row in
// creds under k. It returns how many rows there are and, when any row
// opens under no listed key, one error naming those Providers by id in
// the store's order, which is the start-up failure of luxd serve and the
// credentials row of luxd check.
func Check(ctx context.Context, creds store.Credentials, k *Keyring) (int, error) {
	ids, err := creds.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("secrets: listing the credential rows: %w", err)
	}
	var unopenable []string
	for _, id := range ids {
		row, err := creds.Get(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("secrets: reading the credential row of %s: %w", id, err)
		}
		if _, err := k.Unwrap(id, row); err != nil {
			unopenable = append(unopenable, id)
		}
	}
	if len(unopenable) > 0 {
		return len(ids), fmt.Errorf("%d of %d credential rows open under no listed key: providers %s", len(unopenable), len(ids), strings.Join(unopenable, ", "))
	}
	return len(ids), nil
}
