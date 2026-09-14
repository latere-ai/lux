// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// The value of a Key as spec 007 shapes it: lux_ and forty characters
// of a 64-letter alphabet from a cryptographically secure source, 240
// bits, so the value is the secret and nothing about the Key is
// derivable from it. The first twelve characters are status.prefix, the
// Key's handle for a person; the remaining 32 are never shown again.
const (
	// MintedPrefix opens every value the gateway mints.
	MintedPrefix = "lux_"
	// SuppliedPrefix opens the status.prefix of a Key whose value the
	// caller supplied, which has no lux_ prefix of its own to show.
	SuppliedPrefix = "sup_"
	// KeyPrefixLength is the length of status.prefix, minted or supplied.
	KeyPrefixLength = 12
	// keyValueRandom is the random characters after MintedPrefix.
	keyValueRandom = 40
)

// keyAlphabet is [A-Za-z0-9_-], 64 characters, so one random byte's low
// six bits pick one letter without bias.
const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// MintKeyValue returns a fresh value from crypto/rand: lux_ and forty
// characters of [A-Za-z0-9_-].
func MintKeyValue() (string, error) {
	return mintKeyValue(rand.Reader)
}

// mintKeyValue is MintKeyValue over a given source, for the tests.
func mintKeyValue(random io.Reader) (string, error) {
	var b [keyValueRandom]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("minting a Key value: %w", err)
	}
	out := make([]byte, 0, len(MintedPrefix)+keyValueRandom)
	out = append(out, MintedPrefix...)
	for _, c := range b {
		out = append(out, keyAlphabet[c&63])
	}
	return string(out), nil
}

// HashKeyValue is the hash index's key for a value, minted or supplied:
// SHA-256 over the exact bytes, 64 lower-case hex characters, the one
// function the API stores under and the door looks up with. Nothing is
// trimmed, decoded, or parsed first.
func HashKeyValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// KeyPrefix is status.prefix for a value: the first twelve characters
// of a minted value, and sup_ with the first eight hex characters of
// the hash of a supplied one, because a supplied value's first bytes
// may be the same for every value of its kind and must not be shown.
// Both are twelve characters, name one Key, and disclose nothing of the
// value.
func KeyPrefix(value string, supplied bool) string {
	if supplied {
		return SuppliedPrefix + HashKeyValue(value)[:KeyPrefixLength-len(SuppliedPrefix)]
	}
	return value[:KeyPrefixLength]
}
