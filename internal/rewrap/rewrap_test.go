// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package rewrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
)

const canary = "sk-canary-value-never-in-any-output"

func keyring(t *testing.T, fills ...byte) *secrets.Keyring {
	t.Helper()
	parts := make([]string, 0, len(fills))
	for _, f := range fills {
		parts = append(parts, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{f}, secrets.KeySize)))
	}
	k, err := secrets.Parse(strings.Join(parts, ","))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// seal stores one credential row for id under k at version.
func seal(t *testing.T, s store.Store, k *secrets.Keyring, id string, version int) store.Sealed {
	t.Helper()
	row, err := k.Seal(id, version, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Credentials().Put(t.Context(), id, row); err != nil {
		t.Fatal(err)
	}
	return row
}

// TestRewrapUnderANewKEK: under new,old every data key is re-wrapped,
// every value ciphertext byte is unchanged, every row opens under new
// alone afterwards, and a second run is a no-op.
func TestRewrapUnderANewKEK(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	old, fresh := keyring(t, 1), keyring(t, 2)
	before := map[string]store.Sealed{}
	for _, id := range []string{"prv_a", "prv_b", "prv_c"} {
		before[id] = seal(t, s, old, id, 1)
	}
	var report bytes.Buffer
	sum, err := Run(ctx, s.Credentials(), keyring(t, 2, 1), &report)
	if err != nil || sum.Rewrapped != 3 || sum.Current != 0 || sum.Failed() {
		t.Fatalf("Run() = %+v, %v", sum, err)
	}
	if sum.String() != "rewrap: 3 re-wrapped, 0 already current, 0 unopenable" || report.Len() != 0 {
		t.Fatalf("summary %q, report %q", sum.String(), report.String())
	}
	for id, was := range before {
		row, err := s.Credentials().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(row.Ciphertext, was.Ciphertext) || !bytes.Equal(row.Nonce, was.Nonce) || row.Version != was.Version {
			t.Fatalf("%s: the value columns changed", id)
		}
		if bytes.Equal(row.WrappedKey, was.WrappedKey) || bytes.Equal(row.WrappedNonce, was.WrappedNonce) {
			t.Fatalf("%s: the wrap columns did not change", id)
		}
		if v, err := fresh.Open(id, row); err != nil || string(v) != canary {
			t.Fatalf("%s: Open under new alone = %q, %v", id, v, err)
		}
		if _, err := old.Open(id, row); err == nil {
			t.Fatalf("%s: still opens under old alone", id)
		}
	}
	again, err := Run(ctx, s.Credentials(), keyring(t, 2, 1), &report)
	if err != nil || again.Rewrapped != 0 || again.Current != 3 || again.Failed() {
		t.Fatalf("second Run() = %+v, %v", again, err)
	}
	if strings.Contains(report.String(), canary) {
		t.Fatal("the report carries a value")
	}
}

// TestRewrapDoesNotOverwriteANewerValue: a value applied during the run
// keeps its newer wrap, because the write is at the version that was
// read.
func TestRewrapDoesNotOverwriteANewerValue(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	old, both := keyring(t, 1), keyring(t, 2, 1)
	seal(t, s, old, "prv_a", 1)
	seal(t, s, old, "prv_b", 1)
	// The apply lands between the read and the write: racing is a store
	// whose Get triggers it once.
	racing := &racingStore{Credentials: s.Credentials(), onGet: func(id string) {
		if id == "prv_b" {
			seal(t, s, both, "prv_b", 2)
		}
	}}
	var report bytes.Buffer
	sum, err := Run(ctx, racing, both, &report)
	if err != nil || sum.Rewrapped != 1 || sum.Current != 1 || sum.Failed() {
		t.Fatalf("Run() = %+v, %v", sum, err)
	}
	row, err := s.Credentials().Get(ctx, "prv_b")
	if err != nil || row.Version != 2 {
		t.Fatalf("prv_b = %+v, %v", row, err)
	}
	if v, err := keyring(t, 2).Open("prv_b", row); err != nil || string(v) != canary {
		t.Fatalf("the newer value does not open under new alone: %v", err)
	}
	// A row deleted during the run is skipped at the read and at the
	// write.
	gone := &racingStore{Credentials: s.Credentials(), onGet: func(id string) {
		if id == "prv_a" {
			_ = s.Credentials().Delete(ctx, "prv_a")
		}
	}}
	seal(t, s, old, "prv_a", 1)
	sum, err = Run(ctx, gone, both, &report)
	if err != nil || sum.Rewrapped != 0 || sum.Current != 1 {
		t.Fatalf("Run() over a deleted row = %+v, %v", sum, err)
	}
}

// racingStore runs onGet after each Get, before the caller's write.
type racingStore struct {
	store.Credentials
	onGet func(id string)
}

func (r *racingStore) Get(ctx context.Context, id string) (store.Sealed, error) {
	row, err := r.Credentials.Get(ctx, id)
	if err == nil {
		r.onGet(id)
	}
	return row, err
}

// TestRewrapReportsUnopenableRows: a row no key opens is named by
// provider id, the others are still re-wrapped, and the run has failed.
func TestRewrapReportsUnopenableRows(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	old, lost := keyring(t, 1), keyring(t, 9)
	seal(t, s, old, "prv_a", 1)
	seal(t, s, lost, "prv_lost", 1)
	seal(t, s, old, "prv_c", 1)
	var report bytes.Buffer
	sum, err := Run(ctx, s.Credentials(), keyring(t, 2, 1), &report)
	if err != nil || sum.Rewrapped != 2 || sum.Current != 0 || !sum.Failed() || strings.Join(sum.Unopenable, ",") != "prv_lost" {
		t.Fatalf("Run() = %+v, %v", sum, err)
	}
	if got := report.String(); !strings.HasPrefix(got, "rewrap: provider prv_lost: ") || !strings.Contains(got, "no listed key opens") || strings.Count(got, "\n") != 1 {
		t.Fatalf("report = %q", got)
	}
	if sum.String() != "rewrap: 2 re-wrapped, 0 already current, 1 unopenable" {
		t.Fatalf("summary = %q", sum.String())
	}
	if v, err := keyring(t, 2).Open("prv_c", mustGet(t, s, "prv_c")); err != nil || string(v) != canary {
		t.Fatal("the others were not re-wrapped")
	}
}

func mustGet(t *testing.T, s store.Store, id string) store.Sealed {
	t.Helper()
	row, err := s.Credentials().Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// TestRewrapStopsOnAStoreFailure: a store that cannot list, read, or
// write ends the run with the failure and what was done so far.
func TestRewrapStopsOnAStoreFailure(t *testing.T) {
	ctx := t.Context()
	s := memory.New()
	seal(t, s, keyring(t, 1), "prv_a", 1)
	both := keyring(t, 2, 1)
	if _, err := Run(ctx, failing{Credentials: s.Credentials(), list: true}, both, &bytes.Buffer{}); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("a failing List: %v", err)
	}
	if _, err := Run(ctx, failing{Credentials: s.Credentials(), get: true}, both, &bytes.Buffer{}); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("a failing Get: %v", err)
	}
	if _, err := Run(ctx, failing{Credentials: s.Credentials(), rewrap: true}, both, &bytes.Buffer{}); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("a failing Rewrap: %v", err)
	}
}

// failing refuses the named operations the way the file mode does.
type failing struct {
	store.Credentials
	list, get, rewrap bool
}

func (f failing) List(ctx context.Context) ([]string, error) {
	if f.list {
		return nil, store.ErrReadOnly
	}
	return f.Credentials.List(ctx)
}

func (f failing) Get(ctx context.Context, id string) (store.Sealed, error) {
	if f.get {
		return store.Sealed{}, store.ErrReadOnly
	}
	return f.Credentials.Get(ctx, id)
}

func (f failing) Rewrap(ctx context.Context, id string, v int, wk, wn []byte) error {
	if f.rewrap {
		return store.ErrReadOnly
	}
	return f.Credentials.Rewrap(ctx, id, v, wk, wn)
}
