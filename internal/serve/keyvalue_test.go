// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
)

var keyShape = regexp.MustCompile(`^lux_[A-Za-z0-9_-]{40}$`)

// TestKeyValueShape: a minted value matches ^lux_[A-Za-z0-9_-]{40}$,
// status.prefix is its first twelve characters, every letter of the
// alphabet is reachable from one random byte, and a source that fails
// is an error and never a short value.
func TestKeyValueShape(t *testing.T) {
	v, err := MintKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	if !keyShape.MatchString(v) {
		t.Fatalf("minted %q", v)
	}
	if p := KeyPrefix(v, false); p != v[:12] || len(p) != KeyPrefixLength || !strings.HasPrefix(p, MintedPrefix) {
		t.Fatalf("prefix %q of %q", p, v)
	}
	// Every byte value maps into the alphabet, and 0..63 covers it whole.
	var all [40]byte
	for i := range all {
		all[i] = byte(i)
	}
	v, err = mintKeyValue(strings.NewReader(string(all[:])))
	if err != nil || v != MintedPrefix+"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn" {
		t.Fatalf("mint over 0..39 = %q, %v", v, err)
	}
	for i := range all {
		all[i] = byte(64 + 24 + i) // 88.. wraps to the same letters through the low six bits
	}
	v, _ = mintKeyValue(strings.NewReader(string(all[:])))
	if v != MintedPrefix+"YZabcdefghijklmnopqrstuvwxyz0123456789_-" {
		t.Fatalf("mint over the high bytes = %q", v)
	}
	if _, err := mintKeyValue(iotest.ErrReader(errors.New("no entropy"))); err == nil || !strings.Contains(err.Error(), "minting a Key value") {
		t.Fatalf("a failing source: %v", err)
	}
	if _, err := mintKeyValue(strings.NewReader("short")); err == nil {
		t.Fatal("a short source minted a value")
	}
}

// TestKeyValuesAreDistinct: ten thousand mints are distinct.
func TestKeyValuesAreDistinct(t *testing.T) {
	seen := make(map[string]bool, 10_000)
	for range 10_000 {
		v, err := MintKeyValue()
		if err != nil {
			t.Fatal(err)
		}
		if seen[v] {
			t.Fatalf("a value was minted twice: %s", v[:12])
		}
		seen[v] = true
	}
}

// TestSuppliedValuePrefix: the hash is SHA-256 of the exact bytes as
// lower-case hex, and a supplied value's prefix is sup_ and the first
// eight hex characters of that hash, twelve characters like a minted one.
func TestSuppliedValuePrefix(t *testing.T) {
	value := "  eyJhbGciOiJSUzI1NiJ9.token-with-leading-spaces  "
	sum := sha256.Sum256([]byte(value))
	want := hex.EncodeToString(sum[:])
	if got := HashKeyValue(value); got != want || got == HashKeyValue(strings.TrimSpace(value)) {
		t.Fatalf("HashKeyValue = %s", got)
	}
	p := KeyPrefix(value, true)
	if p != SuppliedPrefix+want[:8] || len(p) != KeyPrefixLength {
		t.Fatalf("supplied prefix = %q", p)
	}
	if strings.Contains(p, "eyJ") {
		t.Fatal("the prefix shows the value")
	}
}
