// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

const (
	canary   = "sk-canary-7f3c9a-do-not-leak"
	provider = "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y"
)

// encoded is one key of the given fill byte, in the variable's syntax.
func encoded(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, KeySize))
}

func keyring(t *testing.T, fills ...byte) *Keyring {
	t.Helper()
	parts := make([]string, 0, len(fills))
	for _, f := range fills {
		parts = append(parts, encoded(f))
	}
	k, err := Parse(strings.Join(parts, ","))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestParse holds the variable's syntax: standard base64 with padding,
// 32 bytes each, at most eight, and a problem that names a position and
// never a value.
func TestParse(t *testing.T) {
	good := encoded(1)
	bad31 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 31))
	for _, tc := range []struct {
		name, raw, want string
		n               int
	}{
		{"one key", good, "", 1},
		{"two keys with spaces", " " + good + " , " + encoded(2), "", 2},
		{"a trailing comma", good + ",", "", 1},
		{"eight keys", strings.Repeat(good+",", 8), "", 8},
		{"empty", "", "lists no key", 0},
		{"blank", " , ", "lists no key", 0},
		{"nine keys", strings.Repeat(good+",", 9), "lists 9 keys, at most 8", 0},
		{"url-safe alphabet", good + "," + strings.ReplaceAll(strings.ReplaceAll(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)), "+", "-"), "/", "_"), "key 2 of 2 is not standard base64 with padding", 0},
		{"no padding", strings.TrimRight(good, "="), "key 1 of 1 is not standard base64 with padding", 0},
		{"31 bytes", good + "," + bad31 + "," + good, "key 2 of 3 decodes to 31 bytes, not 32", 0},
		{"a word", good + ",secret", "key 2 of 2 is not standard base64", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, err := Parse(tc.raw)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Parse() = %v", err)
			case tc.want == "" && k.Len() != tc.n:
				t.Fatalf("Len() = %d, want %d", k.Len(), tc.n)
			case tc.want != "" && err == nil:
				t.Fatalf("Parse() accepted %q", tc.raw)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Parse() = %v, want %q", err, tc.want)
			case tc.want != "" && (strings.Contains(err.Error(), bad31) || strings.Contains(err.Error(), "secret")):
				t.Fatalf("the problem names a value: %v", err)
			}
		})
	}
}

// TestCredentialRowsAreSealed: the row a store holds carries no
// plaintext under any key; the value ciphertext and the wrapped data key
// are separate columns of the sizes the table gives; the value opens.
func TestCredentialRowsAreSealed(t *testing.T) {
	ctx := t.Context()
	k := keyring(t, 1)
	row, err := k.Seal(provider, 1, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	s := memory.New()
	if err := s.Credentials().Put(ctx, provider, row); err != nil {
		t.Fatal(err)
	}
	got, err := s.Credentials().Get(ctx, provider)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WrappedKey) != WrappedKeySize || len(got.WrappedNonce) != NonceSize || len(got.Nonce) != NonceSize || len(got.Ciphertext) != len(canary)+TagSize {
		t.Fatalf("row sizes: wrapped %d, wrapped nonce %d, nonce %d, ciphertext %d", len(got.WrappedKey), len(got.WrappedNonce), len(got.Nonce), len(got.Ciphertext))
	}
	for name, col := range map[string][]byte{"wrapped key": got.WrappedKey, "wrapped nonce": got.WrappedNonce, "ciphertext": got.Ciphertext, "nonce": got.Nonce} {
		if bytes.Contains(col, []byte(canary)) || bytes.Contains(col, []byte(canary[:8])) {
			t.Errorf("%s carries the value", name)
		}
	}
	if enc, _ := json.Marshal(got); bytes.Contains(enc, []byte(canary)) {
		t.Fatal("the encoded row carries the value")
	}
	value, err := k.Open(provider, got)
	if err != nil || string(value) != canary {
		t.Fatalf("Open() = %q, %v", value, err)
	}
}

// TestSealedAdditionalData: both AEADs bind the provider id and the
// version. The wrap fails when either changes; the value fails on its own
// when the data key is re-wrapped under another subject.
func TestSealedAdditionalData(t *testing.T) {
	k := keyring(t, 1)
	row, err := k.Seal(provider, 2, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := k.Open(provider, row); err != nil || string(v) != canary {
		t.Fatalf("unchanged: Open() = %q, %v", v, err)
	}
	if _, err := k.Open("prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Z", row); err == nil || !strings.Contains(err.Error(), "no listed key opens the wrapped data key") {
		t.Fatalf("another provider id: %v", err)
	}
	moved := row
	moved.Version = 3
	if _, err := k.Open(provider, moved); err == nil || !strings.Contains(err.Error(), "no listed key opens the wrapped data key at version 3") {
		t.Fatalf("another version: %v", err)
	}
	// The second AEAD on its own: the data key wrapped again under the
	// other subject's additional data opens, and the value does not.
	dataKey, _, err := k.unwrap(provider, row)
	if err != nil {
		t.Fatal(err)
	}
	other := "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Z"
	wrappedNonce, wrappedKey, err := seal(k.keys[0], dataKey, AAD(other, 2))
	if err != nil {
		t.Fatal(err)
	}
	copied := row
	copied.WrappedKey, copied.WrappedNonce = wrappedKey, wrappedNonce
	if _, err := k.Open(other, copied); err == nil || !strings.Contains(err.Error(), "the value ciphertext does not open") {
		t.Fatalf("a copied row: %v", err)
	}
}

// TestSealIsNeverDeterministic: two seals of one value share no nonce
// and no ciphertext, on either layer.
func TestSealIsNeverDeterministic(t *testing.T) {
	k := keyring(t, 1)
	a, err := k.Seal(provider, 1, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Seal(provider, 1, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) || bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.WrappedKey, b.WrappedKey) || bytes.Equal(a.WrappedNonce, b.WrappedNonce) {
		t.Fatalf("two seals share bytes:\n%+v\n%+v", a, b)
	}
}

// TestRewrapRoundTrip: a row under the old key re-wraps under the new
// one with the value bytes untouched, opens under the new key alone
// afterwards, and is already current on a second pass.
func TestRewrapRoundTrip(t *testing.T) {
	old := keyring(t, 1)
	row, err := old.Seal(provider, 1, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	both := keyring(t, 2, 1)
	if pos, err := both.Unwrap(provider, row); err != nil || pos != 2 {
		t.Fatalf("Unwrap() = %d, %v, want 2", pos, err)
	}
	re, changed, err := both.Rewrap(provider, row)
	if err != nil || !changed {
		t.Fatalf("Rewrap() = %v, %v", changed, err)
	}
	if !bytes.Equal(re.Ciphertext, row.Ciphertext) || !bytes.Equal(re.Nonce, row.Nonce) || re.Version != row.Version {
		t.Fatal("Rewrap touched the value columns")
	}
	if bytes.Equal(re.WrappedKey, row.WrappedKey) || bytes.Equal(re.WrappedNonce, row.WrappedNonce) {
		t.Fatal("Rewrap kept the wrap columns")
	}
	fresh := keyring(t, 2)
	if v, err := fresh.Open(provider, re); err != nil || string(v) != canary {
		t.Fatalf("Open under the new key alone = %q, %v", v, err)
	}
	if _, err := old.Open(provider, re); err == nil {
		t.Fatal("the re-wrapped row still opens under the old key alone")
	}
	again, changed, err := fresh.Rewrap(provider, re)
	if err != nil || changed {
		t.Fatalf("second Rewrap() = %v, %v", changed, err)
	}
	if !reflect.DeepEqual(again, re) {
		t.Fatal("a no-op Rewrap changed the row")
	}
	if _, _, err := fresh.Rewrap(provider, row); err == nil {
		t.Fatal("Rewrap of a row under no listed key succeeded")
	}
}

// TestRefusedInputs: the subjects the additional data cannot bind and
// the row shapes that are not this suite's are refused with the
// developer's detail before any key is tried.
func TestRefusedInputs(t *testing.T) {
	k := keyring(t, 1)
	if _, err := k.Seal("", 1, []byte("v")); err == nil || !strings.Contains(err.Error(), "no provider id") {
		t.Fatalf("Seal without an id: %v", err)
	}
	if _, err := k.Seal(provider, 0, []byte("v")); err == nil || !strings.Contains(err.Error(), "version 0, below 1") {
		t.Fatalf("Seal at version 0: %v", err)
	}
	row, err := k.Seal(provider, 1, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	short := row
	short.WrappedKey = row.WrappedKey[:WrappedKeySize-1]
	if _, err := k.Open(provider, short); err == nil || !strings.Contains(err.Error(), "wrapped data key is 47 bytes, not 48") {
		t.Fatalf("a short wrapped key: %v", err)
	}
	badNonce := row
	badNonce.WrappedNonce = nil
	if _, err := k.Open(provider, badNonce); err == nil || !strings.Contains(err.Error(), "wrapped nonce is 0 bytes, not 12") {
		t.Fatalf("a missing wrapped nonce: %v", err)
	}
	valueNonce := row
	valueNonce.Nonce = row.Nonce[:NonceSize-1]
	if _, err := k.Open(provider, valueNonce); err == nil || !strings.Contains(err.Error(), "value nonce is 11 bytes, not 12") {
		t.Fatalf("a short value nonce: %v", err)
	}
	if _, err := k.Unwrap("", row); err == nil {
		t.Fatal("Unwrap without an id")
	}
}

// TestStartupRequiresAWorkingKEK is the secrets half of the row: every
// stored row's wrapped data key is opened at start, and the rows no
// listed key opens are named by provider id, never by value.
func TestStartupRequiresAWorkingKEK(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	a, b := keyring(t, 1), keyring(t, 2)
	for id, k := range map[string]*Keyring{"prv_a": a, "prv_b": b, "prv_c": a} {
		row, err := k.Seal(id, 1, []byte(canary))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Credentials().Put(ctx, id, row); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Check(ctx, s.Credentials(), keyring(t, 2, 1))
	if err != nil || n != 3 {
		t.Fatalf("Check under both keys = %d, %v", n, err)
	}
	n, err = Check(ctx, s.Credentials(), a)
	if err == nil || n != 3 {
		t.Fatalf("Check under one key = %d, %v", n, err)
	}
	if got := err.Error(); !strings.Contains(got, "1 of 3 credential rows open under no listed key: providers prv_b") || strings.Contains(got, canary) {
		t.Fatalf("Check() = %v", err)
	}
	if n, err := Check(ctx, memory.New().Credentials(), a); err != nil || n != 0 {
		t.Fatalf("Check over an empty store = %d, %v", n, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Check(cancelled, s.Credentials(), a); err == nil {
		t.Fatal("Check over an ended context succeeded")
	}
}

// TestCredentialStatusCarriesNoValue is the custody half of the row:
// status.credential has the three members and nothing else, and a
// Provider carrying a canary value, once stored, encodes without it on
// a read and a list.
func TestCredentialStatusCarriesNoValue(t *testing.T) {
	fields := reflect.TypeFor[v1.CredentialStatus]()
	if fields.NumField() != 3 {
		t.Fatalf("CredentialStatus has %d fields, want set, version, updatedAt", fields.NumField())
	}
	for i, want := range []string{"set", "version", "updatedAt"} {
		if tag, _, _ := strings.Cut(fields.Field(i).Tag.Get("json"), ","); tag != want {
			t.Errorf("field %d is %q, want %q", i, tag, want)
		}
	}
	ctx := t.Context()
	s := memory.New()
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "openai"},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", Credential: &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer}},
		Status:   v1.ProviderStatus{ID: provider, Owner: "https://login.example.com|alice", Credential: &v1.CredentialStatus{Set: true, Version: 1, UpdatedAt: time.Now()}, Warnings: []string{}},
	}
	p.Spec.Credential.SetValue(canary)
	if _, err := s.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Objects().Get(ctx, v1.KindProvider, provider)
	if err != nil {
		t.Fatal(err)
	}
	list, _, err := s.Objects().List(ctx, v1.KindProvider, store.Filter{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	for name, obj := range map[string]any{"Get": got, "List": list, "the applied object": p} {
		enc, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(enc), canary) || strings.Contains(string(enc), canary[:8]) {
			t.Errorf("%s encodes the value: %s", name, enc)
		}
		if !strings.Contains(string(enc), `"credential":{"set":true,"version":1,`) {
			t.Errorf("%s lacks the credential status: %s", name, enc)
		}
	}
}

func TestErrorsAreSentinelFree(t *testing.T) {
	// The package raises plain errors with the developer's detail; a
	// caller has no sentinel to compare, so a store error passes through
	// wrapped and recognizable.
	k := keyring(t, 1)
	_, err := Check(t.Context(), refusing{}, k)
	if !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("Check over a refusing store = %v", err)
	}
}

// refusing is a Credentials whose every call is the file mode's answer.
type refusing struct{ store.Credentials }

func (refusing) List(context.Context) ([]string, error) { return nil, store.ErrReadOnly }

// TestHelpersRefuseABadKey: the two AEAD helpers answer a key of the
// wrong size with the cipher's error rather than a panic, which is what
// keeps a caller's mistake a failure and never a crash.
func TestHelpersRefuseABadKey(t *testing.T) {
	if _, _, err := seal([]byte("short"), []byte("v"), nil); err == nil {
		t.Fatal("seal under a 5-byte key")
	}
	if _, err := open([]byte("short"), make([]byte, NonceSize), nil, nil); err == nil {
		t.Fatal("open under a 5-byte key")
	}
	if _, err := gcm(nil); err == nil {
		t.Fatal("gcm over no key")
	}
}

// TestCheckPassesAStoreFailureThrough: a row that cannot be read is the
// store's failure, wrapped with the provider id and passed through.
func TestCheckPassesAStoreFailureThrough(t *testing.T) {
	_, err := Check(t.Context(), listing{}, keyring(t, 1))
	if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "prv_gone") {
		t.Fatalf("Check() = %v", err)
	}
}

// listing is a Credentials that lists one row and cannot read it.
type listing struct{ store.Credentials }

func (listing) List(context.Context) ([]string, error) { return []string{"prv_gone"}, nil }
func (listing) Get(context.Context, string) (store.Sealed, error) {
	return store.Sealed{}, store.ErrNotFound
}
