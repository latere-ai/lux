// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

const (
	owner      = "https://login.example.com|alice"
	kek        = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	otherKEK   = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	credential = "sk-bootstrap-credential-7f3c9a"
	keyValue   = "lux_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-"
	publicURL  = "https://lux.example.com"
)

// The four documents most tests start from: one of each kind, the
// Provider's credential and the Key's value named by variables.
const (
	providerDoc = `apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openai
spec:
  dialect: openai
  baseURL: https://api.example.com/v1
  credential:
    valueFrom:
      env: OPENAI_API_KEY
  discovery:
    mode: none
`
	budgetDoc = `apiVersion: lux.latere.ai/v1beta1
kind: Budget
metadata:
  name: team
spec:
  amount: "10"
  currency: USD
  window: month
`
	modelDoc = `apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: gpt-4o
spec:
  targets:
    - provider: openai
      model: gpt-4o
  pricing:
    input: "2.50"
    output: "10"
`
	keyDoc = `apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: ci
spec:
  models: ["gpt-4o"]
  budget: team
  valueFrom:
    env: LUX_CI_KEY
`
)

// harness is one bootstrap directory over a memory store, with the
// environment, the clock, and the ids the tests read.
type harness struct {
	t    *testing.T
	dir  string
	st   *memory.Store
	keys *secrets.Keyring
	env  map[string]string
	now  time.Time
	ids  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:   t,
		dir: t.TempDir(),
		env: map[string]string{"OPENAI_API_KEY": credential, "LUX_CI_KEY": keyValue},
		now: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
	}
	h.st = memory.New(memory.WithClock(func() time.Time { return h.now }))
	h.keys = h.keyring(kek)
	return h
}

func (h *harness) keyring(raw string) *secrets.Keyring {
	h.t.Helper()
	k, err := secrets.Parse(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return k
}

// catalog writes the four documents under the directory's subfolders.
func (h *harness) catalog() {
	h.write("providers/openai.yaml", providerDoc)
	h.write("budgets/team.yaml", budgetDoc)
	h.write("models/gpt-4o.yaml", modelDoc)
	h.write("keys/ci.yaml", keyDoc)
}

func (h *harness) write(rel, body string) {
	h.t.Helper()
	path := filepath.Join(h.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) options() Options {
	u, err := url.Parse(publicURL)
	if err != nil {
		h.t.Fatal(err)
	}
	return Options{
		Dir:       h.dir,
		Store:     h.st,
		Owner:     owner,
		Keys:      h.keys,
		Getenv:    func(name string) string { return h.env[name] },
		Defaults:  manifest.Defaults{RequestsPerMinute: 60, TokensPerMinute: 100000, Timeout: time.Minute},
		PublicURL: u,
		Now:       func() time.Time { return h.now },
		NewID: func(prefix string) string {
			h.ids++
			return fmt.Sprintf("%s%026d", prefix, h.ids)
		},
	}
}

// apply runs Apply and fails the test on a refusal.
func (h *harness) apply() Result {
	h.t.Helper()
	res, err := Apply(h.t.Context(), h.options())
	if err != nil {
		h.t.Fatalf("apply: %v", err)
	}
	return res
}

// refused runs Apply and returns its refusal, failing the test on none.
func (h *harness) refused(o Options) *Error {
	h.t.Helper()
	_, err := Apply(h.t.Context(), o)
	e, ok := errors.AsType[*Error](err)
	if !ok {
		h.t.Fatalf("apply answered %v, want a refusal", err)
	}
	return e
}

// get reads one object by kind and name with its version.
func (h *harness) get(kind, name string) (v1.Object, int64) {
	h.t.Helper()
	obj, version, err := h.st.Objects().ByName(h.t.Context(), kind, name)
	if err != nil {
		h.t.Fatalf("%s %s: %v", kind, name, err)
	}
	return obj, version
}

// versions is the version of every object the catalog declares.
func (h *harness) versions() map[string]int64 {
	h.t.Helper()
	out := map[string]int64{}
	for _, kn := range [][2]string{{v1.KindProvider, "openai"}, {v1.KindBudget, "team"}, {v1.KindModel, "gpt-4o"}, {v1.KindKey, "ci"}} {
		_, v := h.get(kn[0], kn[1])
		out[kn[0]] = v
	}
	return out
}

// records is the journal in global order, each row decoded as the body
// a sink receives.
func (h *harness) records() []events.Record {
	h.t.Helper()
	rows, err := h.st.Journal().Since(h.t.Context(), 0, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]events.Record, 0, len(rows))
	for _, row := range rows {
		var rec events.Record
		if err := json.Unmarshal(row.Payload, &rec); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

// storeIsEmpty fails the test when any object, hash, credential, or
// journal row exists.
func (h *harness) storeIsEmpty() {
	h.t.Helper()
	ctx := h.t.Context()
	for _, kind := range kindOrder {
		objs, _, err := h.st.Objects().List(ctx, kind, store.Filter{}, store.Page{})
		if err != nil || len(objs) != 0 {
			h.t.Errorf("%s: %d objects, %v", kind, len(objs), err)
		}
	}
	if ids, err := h.st.Credentials().List(ctx); err != nil || len(ids) != 0 {
		h.t.Errorf("credentials: %v, %v", ids, err)
	}
	if rows, err := h.st.Journal().Since(ctx, 0, 0); err != nil || len(rows) != 0 {
		h.t.Errorf("journal: %d rows, %v", len(rows), err)
	}
}

// faulty is a store whose chosen reads and whose Transact fail, for the
// refusals a store's own failure produces.
type faulty struct {
	store.Store
	transact error
	byName   error
	list     error
	creds    error
	fences   error
	byHash   error
}

func (f *faulty) Objects() store.Objects {
	return faultyObjects{Objects: f.Store.Objects(), byName: f.byName, list: f.list}
}

func (f *faulty) Credentials() store.Credentials {
	return faultyCredentials{Credentials: f.Store.Credentials(), err: f.creds}
}

func (f *faulty) KeyFences() store.KeyFences {
	return faultyFences{KeyFences: f.Store.KeyFences(), err: f.fences}
}

func (f *faulty) Keys() store.Keys { return faultyKeys{Keys: f.Store.Keys(), err: f.byHash} }

func (f *faulty) Transact(ctx context.Context, fn func(tx store.Store) error) error {
	if f.transact != nil {
		return f.transact
	}
	return f.Store.Transact(ctx, fn)
}

type faultyObjects struct {
	store.Objects
	byName, list error
}

func (o faultyObjects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	if o.byName != nil {
		return nil, 0, o.byName
	}
	return o.Objects.ByName(ctx, kind, name)
}

func (o faultyObjects) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]v1.Object, string, error) {
	if o.list != nil {
		return nil, "", o.list
	}
	return o.Objects.List(ctx, kind, f, p)
}

type faultyCredentials struct {
	store.Credentials
	err error
}

func (c faultyCredentials) Get(ctx context.Context, id string) (store.Sealed, error) {
	if c.err != nil {
		return store.Sealed{}, c.err
	}
	return c.Credentials.Get(ctx, id)
}

type faultyFences struct {
	store.KeyFences
	err error
}

func (f faultyFences) Get(ctx context.Context, name string) (store.KeyFence, error) {
	if f.err != nil {
		return store.KeyFence{}, f.err
	}
	return f.KeyFences.Get(ctx, name)
}

type faultyKeys struct {
	store.Keys
	err error
}

func (k faultyKeys) ByHash(ctx context.Context, hash string) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	return k.Keys.ByHash(ctx, hash)
}

// id is the id of one object by kind and name.
func (h *harness) id(kind, name string) string {
	h.t.Helper()
	obj, _ := h.get(kind, name)
	return obj.ID()
}
