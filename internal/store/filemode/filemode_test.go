// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package filemode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

const head = "apiVersion: lux.latere.ai/v1beta1\n"

const (
	providerYAML = head + `kind: Provider
metadata:
  name: openai
spec:
  dialect: openai
  baseURL: https://api.example.com/v1
  credential:
    valueFrom:
      env: OPENAI_KEY
`
	budgetYAML = head + `kind: Budget
metadata:
  name: team
spec:
  amount: "10"
`
	modelYAML = head + `kind: Model
metadata:
  name: gpt-5
spec:
  targets:
    - provider: openai
  pricing:
    input: "1"
    output: "2"
`
	keyYAML = head + `kind: Key
metadata:
  name: run-42
spec:
  models: [gpt-5]
  budget: team
  valueFrom:
    env: RUN_KEY
`
	keyValue = "lux_0123456789abcdefghijklmnopqrstuvwxyzABCD"
)

// full is the four kinds in a deliberately wrong path order: the Key
// sorts first and the Provider last.
var full = map[string]string{
	"1-key.yaml":      keyYAML,
	"2-model.yaml":    modelYAML,
	"3-budget.yaml":   budgetYAML,
	"4-provider.yaml": providerYAML,
}

var env = map[string]string{"OPENAI_KEY": "sk-live-canary-credential", "RUN_KEY": keyValue}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func options(dir string, env map[string]string) Options {
	return Options{Dir: dir, Getenv: func(k string) string { return env[k] }, Now: time.Now}
}

func load(t *testing.T, files map[string]string, env map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	s, err := Load(t.Context(), options(dir, env))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func loadErr(t *testing.T, files map[string]string, env map[string]string) error {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	s, err := Load(t.Context(), options(dir, env))
	if err == nil {
		t.Fatalf("Load accepted the directory: %+v", s.Summary())
	}
	if s != nil {
		t.Fatal("a failed Load returned a store")
	}
	return err
}

func byName(t *testing.T, s store.Store, kind, name string) (v1.Object, int64) {
	t.Helper()
	obj, v, err := s.Objects().ByName(t.Context(), kind, name)
	if err != nil {
		t.Fatalf("ByName %s %q: %v", kind, name, err)
	}
	return obj, v
}

func TestFileModeResolvesInKindOrder(t *testing.T) {
	s := load(t, full, env)
	p, v := byName(t, s, v1.KindProvider, "openai")
	prv := p.(*v1.Provider)
	if v != 1 || prv.Status.Owner != Subject || !strings.HasPrefix(prv.Status.ID, v1.PrefixProvider) {
		t.Fatalf("Provider status = %+v, version %d", prv.Status, v)
	}
	if c := prv.Status.Credential; c == nil || !c.Set || c.Version != 1 {
		t.Fatalf("status.credential = %+v, want set at version 1", c)
	}
	if _, set := prv.Spec.Credential.Value(); set {
		t.Fatal("the credential value reached the store")
	}
	k, _ := byName(t, s, v1.KindKey, "run-42")
	key := k.(*v1.Key)
	if key.Status.Prefix != keyValue[:12] || key.Status.Owner != Subject {
		t.Fatalf("Key status = %+v", key.Status)
	}
	b, _ := byName(t, s, v1.KindBudget, "team")
	if key.Status.Budget == nil || key.Status.Budget.Name != "team" || key.Status.Budget.ID != b.ID() {
		t.Fatalf("status.budget = %+v, want the Budget's name and id", key.Status.Budget)
	}
	if len(key.Status.Selectors) != 1 || len(key.Status.Selectors[0].Matched) != 1 || key.Status.Selectors[0].Matched[0] != "gpt-5" {
		t.Fatalf("status.selectors = %+v, want gpt-5 matched", key.Status.Selectors)
	}
	m, _ := byName(t, s, v1.KindModel, "gpt-5")
	if m.(*v1.Model).Status.Source != v1.SourceDeclared || m.(*v1.Model).Spec.Pricing.CachedInput == nil {
		t.Fatalf("Model = %+v, want declared and defaulted", m)
	}
	sum := s.Summary()
	if sum.Files != 4 || sum.Kinds[v1.KindProvider] != 1 || sum.Kinds[v1.KindKey] != 1 || sum.Dir != s.Dir() {
		t.Fatalf("Summary = %+v", sum)
	}
	for _, want := range []string{"4 files read", "1 providers, 1 budgets, 1 models, 1 keys", "address objects by name", "per replica"} {
		if !strings.Contains(s.Notice(), want) {
			t.Errorf("Notice lacks %q: %s", want, s.Notice())
		}
	}
	if err := s.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileModeIsOneObjectPerFile(t *testing.T) {
	err := loadErr(t, map[string]string{"providers/openai.yaml": providerYAML + "---\n" + budgetYAML}, env)
	if !strings.Contains(err.Error(), "providers/openai.yaml: multi_document") {
		t.Fatalf("a two-document file: %v", err)
	}
	// A .yml file is not read, a .json file and a nested directory are,
	// and any other extension is passed over.
	s := load(t, map[string]string{
		"nested/deeper/provider.json": `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider","metadata":{"name":"openai"},"spec":{"dialect":"openai","baseURL":"https://api.example.com/v1"}}`,
		"budget.yml":                  budgetYAML,
		"README.md":                   "not a manifest",
		"model.yaml":                  modelYAML,
	}, env)
	if _, _, err := s.Objects().ByName(t.Context(), v1.KindBudget, "team"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a .yml file was read: %v", err)
	}
	p, _ := byName(t, s, v1.KindProvider, "openai")
	if c := p.(*v1.Provider).Status.Credential; c == nil || c.Set {
		t.Fatalf("a Provider without a credential has status.credential.set false: %+v", c)
	}
	if _, ok := s.CredentialValue(p.ID()); ok {
		t.Fatal("a Provider without a credential has a value")
	}
	if s.Summary().Files != 2 {
		t.Fatalf("Files = %d, want the .json and the .yaml", s.Summary().Files)
	}
	err = loadErr(t, map[string]string{"a.yaml": budgetYAML, "b.yaml": budgetYAML}, env)
	if !strings.Contains(err.Error(), `b.yaml: Budget "team" is also declared in a.yaml`) {
		t.Fatalf("a duplicate name: %v", err)
	}
}

func TestFileModeValuesFromEnvironment(t *testing.T) {
	s := load(t, full, env)
	p, _ := byName(t, s, v1.KindProvider, "openai")
	if v, ok := s.CredentialValue(p.ID()); !ok || v != env["OPENAI_KEY"] {
		t.Fatalf("CredentialValue = %q, %v", v, ok)
	}
	sum := sha256.Sum256([]byte(keyValue))
	k, _ := byName(t, s, v1.KindKey, "run-42")
	if id, err := s.Keys().ByHash(t.Context(), hex.EncodeToString(sum[:])); err != nil || id != k.ID() {
		t.Fatalf("ByHash = %q, %v, want the Key %s", id, err, k.ID())
	}

	for name, tc := range map[string]struct {
		env  map[string]string
		want []string
	}{
		"credential unset": {map[string]string{"RUN_KEY": keyValue}, []string{`4-provider.yaml: Provider "openai": credential variable OPENAI_KEY is unset or empty`}},
		"credential empty": {map[string]string{"RUN_KEY": keyValue, "OPENAI_KEY": ""}, []string{"OPENAI_KEY is unset or empty"}},
		"key unset":        {map[string]string{"OPENAI_KEY": "sk"}, []string{`1-key.yaml: Key "run-42": variable RUN_KEY is unset or empty`}},
		"key shape":        {map[string]string{"OPENAI_KEY": "sk", "RUN_KEY": "not-a-lux-key-value-of-the-right-shape-000000"}, []string{`Key "run-42": the value of RUN_KEY is not of the shape lux_`}},
	} {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, full, tc.env)
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%v lacks %q", err, want)
				}
			}
			for _, secret := range []string{"not-a-lux-key-value", keyValue, "sk-live"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the failure carries a value: %v", err)
				}
			}
		})
	}
	err := loadErr(t, map[string]string{"p.yaml": providerYAML, "m.yaml": modelYAML, "k.yaml": head + "kind: Key\nmetadata:\n  name: run-42\nspec:\n  models: [gpt-5]\n"}, env)
	if !strings.Contains(err.Error(), `Key "run-42": spec.valueFrom.env is required in file mode`) {
		t.Fatalf("a Key without a variable: %v", err)
	}
	// An inline credential value is admitted, as the resolver admits it,
	// and held like a variable's value.
	inline := head + "kind: Provider\nmetadata:\n  name: inline\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n  credential:\n    value: sk-inline\n"
	s = load(t, map[string]string{"p.yaml": inline}, nil)
	p, _ = byName(t, s, v1.KindProvider, "inline")
	if v, ok := s.CredentialValue(p.ID()); !ok || v != "sk-inline" {
		t.Fatalf("an inline value: %q, %v", v, ok)
	}
	// Getenv nil reads the process environment.
	t.Setenv("OPENAI_KEY", "sk-from-process")
	t.Setenv("RUN_KEY", keyValue)
	dir := t.TempDir()
	writeFiles(t, dir, full)
	s, err = Load(t.Context(), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	p, _ = byName(t, s, v1.KindProvider, "openai")
	if v, _ := s.CredentialValue(p.ID()); v != "sk-from-process" {
		t.Fatalf("CredentialValue from the process environment = %q", v)
	}
}

func TestFileModeRefusesServerOnlyFields(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"Key.spec.value": {head + "kind: Key\nmetadata:\n  name: run-42\nspec:\n  models: [gpt-5]\n  value: " + keyValue + "\n", "k.yaml: invalid_field at spec.value"},
		"tunnel":         {head + "kind: Provider\nmetadata:\n  name: laptop\nspec:\n  dialect: openai\n  tunnel: true\n", "k.yaml: invalid_field at spec.tunnel"},
		"no name":        {head + "kind: Budget\nspec:\n  amount: \"10\"\n", "k.yaml: missing_field at metadata.name"},
	} {
		t.Run(name, func(t *testing.T) {
			err := loadErr(t, map[string]string{"p.yaml": providerYAML, "m.yaml": modelYAML, "k.yaml": tc.body}, env)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), keyValue) {
				t.Fatalf("the failure carries the value: %v", err)
			}
		})
	}
}

func TestFileModeStartupNamesTheFailingFile(t *testing.T) {
	err := loadErr(t, map[string]string{"providers/openai.yaml": providerYAML, "models/gpt-5.yaml": head + "kind: Model\nmetadata:\n  name: gpt-5\nspec:\n  targets:\n    - provider: nowhere\n"}, env)
	for _, want := range []string{"manifest dir ", "models/gpt-5.yaml: not_found at spec.targets[0].provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v lacks %q", err, want)
		}
	}
	if _, err := Load(t.Context(), Options{}); err == nil {
		t.Fatal("Load without a directory")
	}
	if _, err := Load(t.Context(), options(filepath.Join(t.TempDir(), "missing"), nil)); err == nil {
		t.Fatal("Load of a missing directory")
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "broken.yaml")); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := Load(t.Context(), options(dir, nil)); err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("an unreadable file: %v", err)
	}
}

func TestFileModeRefusesWrites(t *testing.T) {
	ctx := t.Context()
	s := load(t, full, env)
	p, _ := byName(t, s, v1.KindProvider, "openai")
	k, _ := byName(t, s, v1.KindKey, "run-42")
	fresh := &v1.Budget{Metadata: v1.ObjectMeta{Name: "new"}, Status: v1.BudgetStatus{ID: "bud_new", Owner: Subject}}
	writes := map[string]func(s store.Store) error{
		"Objects.Put create": func(s store.Store) error { _, err := s.Objects().Put(ctx, fresh, 0); return err },
		"Objects.Put update": func(s store.Store) error { _, err := s.Objects().Put(ctx, p, 1); return err },
		"Objects.Delete":     func(s store.Store) error { return s.Objects().Delete(ctx, v1.KindProvider, p.ID()) },
		"Keys.Put":           func(s store.Store) error { return s.Keys().Put(ctx, k.ID(), strings.Repeat("a", 64)) },
		"Keys.Delete":        func(s store.Store) error { return s.Keys().Delete(ctx, k.ID()) },
		"Credentials.Put":    func(s store.Store) error { return s.Credentials().Put(ctx, p.ID(), store.Sealed{}) },
		"Credentials.Rewrap": func(s store.Store) error { return s.Credentials().Rewrap(ctx, p.ID(), 1, nil, nil) },
		"Credentials.Get":    func(s store.Store) error { _, err := s.Credentials().Get(ctx, p.ID()); return err },
		"Credentials.Delete": func(s store.Store) error { return s.Credentials().Delete(ctx, p.ID()) },
		"Credentials.List":   func(s store.Store) error { _, err := s.Credentials().List(ctx); return err },
		"Objects.Put nil":    func(s store.Store) error { _, err := s.Objects().Put(ctx, nil, 0); return err },
		"Put over a declared": func(s store.Store) error {
			m, _, err := s.Objects().ByName(ctx, v1.KindModel, "gpt-5")
			if err != nil {
				return err
			}
			return putDiscovered(ctx, s, m.Name(), m.ID(), 1)
		},
	}
	for name, write := range writes {
		for _, via := range []string{"direct", "transact"} {
			err := write(s)
			if via == "transact" {
				err = s.Transact(ctx, func(tx store.Store) error { return write(tx) })
			}
			if !errors.Is(err, store.ErrReadOnly) {
				t.Errorf("%s via %s = %v, want ErrReadOnly", name, via, err)
			}
			if !strings.Contains(err.Error(), s.Dir()) {
				t.Errorf("%s via %s: the detail does not name the directory: %v", name, via, err)
			}
		}
	}
	// Nothing changed, and a read-only Transact still refuses nesting.
	if _, v := byName(t, s, v1.KindProvider, "openai"); v != 1 {
		t.Fatalf("Provider version = %d after refused writes", v)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindBudget, "new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused Budget exists: %v", err)
	}
	err := s.Transact(ctx, func(tx store.Store) error { return tx.Transact(ctx, func(store.Store) error { return nil }) })
	if !errors.Is(err, store.ErrNested) {
		t.Fatalf("nested Transact = %v", err)
	}
	if err := s.Objects().Delete(ctx, v1.KindProvider, "prv_none"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Delete of an unknown id = %v, want ErrNotFound before the read-only rule", err)
	}
	if err := putDiscovered(ctx, s, "openai/o3", "mdl_none", 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a discovered update of an unknown id = %v, want ErrNotFound", err)
	}
}

// putDiscovered writes a discovered Model the way the discovery job of
// spec 005 does, with an owner of its own the mode overrides.
func putDiscovered(ctx context.Context, s store.Store, name, id string, version int64) error {
	w := 100
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ModelSpec{Targets: []v1.Target{{Provider: "openai", Model: strings.TrimPrefix(name, "openai/"), Weight: &w}}, Fallback: v1.FallbackNever},
		Status:   v1.ModelStatus{ID: id, Owner: "https://login.example.com|discovery", Source: v1.SourceDiscovered, Warnings: []string{}},
	}
	_, err := s.Objects().Put(ctx, m, version)
	return err
}

func TestFileModeAdmitsTheJobs(t *testing.T) {
	ctx := t.Context()
	s := load(t, full, env)
	p, _ := byName(t, s, v1.KindProvider, "openai")
	if err := s.Objects().PutStatus(ctx, v1.KindProvider, p.ID(), store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy}}); err != nil {
		t.Fatalf("PutStatus: %v", err)
	}
	got, _ := byName(t, s, v1.KindProvider, "openai")
	if h := got.(*v1.Provider).Status.Health; h == nil || h.State != v1.HealthHealthy {
		t.Fatalf("health = %+v", h)
	}
	if _, err := s.Counters().Add(ctx, "key:k:requests:0", 1, time.Time{}); err != nil {
		t.Fatalf("Counters.Add: %v", err)
	}
	if held, err := s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica", store.LeaseTTL); err != nil || !held {
		t.Fatalf("Leases.Acquire = %v, %v", held, err)
	}
	if _, err := s.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: p.ID(), Type: "provider.unreachable"}); err != nil {
		t.Fatalf("Journal.Append: %v", err)
	}
	if err := s.Tunnels().Register(ctx, store.Tunnel{ProviderID: p.ID(), Session: "tun_1"}, time.Minute); err != nil {
		t.Fatalf("Tunnels.Register: %v", err)
	}
	err := s.Transact(ctx, func(tx store.Store) error {
		if err := putDiscovered(ctx, tx, "openai/o3", "mdl_o3", 0); err != nil {
			return err
		}
		if _, err := tx.Counters().Add(ctx, "key:k:tokens:0", 1, time.Time{}); err != nil {
			return err
		}
		if _, err := tx.Leases().Acquire(ctx, store.LeaseJournal, "replica", store.LeaseTTL); err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, store.Event{ID: "evt_2", ObjectID: "mdl_o3", Type: "model.discovered"}); err != nil {
			return err
		}
		if _, err := tx.Tunnels().Get(ctx, p.ID()); err != nil {
			return err
		}
		if err := tx.Ready(ctx); err != nil {
			return err
		}
		return tx.Objects().PutStatus(ctx, v1.KindModel, "mdl_o3", store.ModelObserved{Available: ptr(true)})
	})
	if err != nil {
		t.Fatalf("the discovery Transact: %v", err)
	}
	if err := putDiscovered(ctx, s, "openai/o3", "mdl_o3", 1); err != nil {
		t.Fatalf("a discovered update: %v", err)
	}
	if err := s.Objects().Delete(ctx, v1.KindModel, "mdl_o3"); err != nil {
		t.Fatalf("Delete of a discovered Model: %v", err)
	}
}

func TestFileModeDiscovery(t *testing.T) {
	ctx := t.Context()
	s := load(t, full, env)
	if err := putDiscovered(ctx, s, "openai/o3", "mdl_o3", 0); err != nil {
		t.Fatal(err)
	}
	models, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Source: string(v1.SourceDiscovered)}, store.Page{})
	if err != nil || len(models) != 1 || models[0].Name() != "openai/o3" {
		t.Fatalf("discovered Models = %v, %v", models, err)
	}
	if models[0].Owner() != Subject {
		t.Fatalf("owner = %q, want the file subject", models[0].Owner())
	}
	all, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	if err != nil || len(all) != 2 {
		t.Fatalf("every Model = %v, %v", all, err)
	}
	// Read-only for a caller: a declared write over it is refused.
	declared := &v1.Model{Metadata: v1.ObjectMeta{Name: "openai/o3"}, Spec: models[0].(*v1.Model).Spec, Status: v1.ModelStatus{ID: "mdl_declared", Owner: Subject, Source: v1.SourceDeclared}}
	if _, err := s.Objects().Put(ctx, declared, 0); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("a declared Put over a discovered Model = %v", err)
	}
}

func TestFileModeReReads(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	writeFiles(t, dir, full)
	env2 := map[string]string{"OPENAI_KEY": "sk-live", "RUN_KEY": keyValue}
	s, err := Load(ctx, options(dir, env2))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := byName(t, s, v1.KindProvider, "openai")
	k, _ := byName(t, s, v1.KindKey, "run-42")
	if err := putDiscovered(ctx, s, "openai/o3", "mdl_o3", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Objects().PutStatus(ctx, v1.KindProvider, p.ID(), store.ProviderObserved{Discovered: &v1.DiscoveredStatus{Count: 1}}); err != nil {
		t.Fatal(err)
	}

	// Added, changed with the Provider's address kept, and a second
	// Provider whose baseURL will change.
	azure := strings.ReplaceAll(strings.ReplaceAll(providerYAML, "name: openai", "name: azure"), "env: OPENAI_KEY", "env: AZURE_KEY")
	env2["AZURE_KEY"] = "az"
	writeFiles(t, dir, map[string]string{"5-azure.yaml": azure})
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("Reload with the Provider added: %v", err)
	}
	az, _ := byName(t, s, v1.KindProvider, "azure")
	env2["OPENAI_KEY"] = "sk-rotated"
	if err := putDiscovered(ctx, s, "azure/o3", "mdl_az", 0); err != nil {
		t.Fatal(err)
	}
	// The azure discovered Model targets openai by name in putDiscovered;
	// point it at azure so the drop is attributable.
	m, _ := byName(t, s, v1.KindModel, "azure/o3")
	m.(*v1.Model).Spec.Targets[0].Provider = "azure"
	if _, err := s.mem.Objects().Put(ctx, m, 1); err != nil {
		t.Fatal(err)
	}

	writeFiles(t, dir, map[string]string{
		"2-model.yaml": strings.ReplaceAll(modelYAML, `input: "1"`, `input: "3"`),
		"5-azure.yaml": strings.ReplaceAll(azure, "api.example.com/v1", "azure.example.com/v1"),
		"6-key2.yaml":  strings.ReplaceAll(strings.ReplaceAll(keyYAML, "name: run-42", "name: run-43"), "env: RUN_KEY", "env: RUN_KEY2"),
	})
	if err := os.Remove(filepath.Join(dir, "3-budget.yaml")); err != nil {
		t.Fatal(err)
	}
	env2["RUN_KEY2"] = "lux_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"
	// The Key names the removed Budget, so the read fails and the old
	// snapshot keeps serving.
	err = s.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "1-key.yaml: not_found at spec.budget") {
		t.Fatalf("a directory that stops resolving: %v", err)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindBudget, "team"); err != nil {
		t.Fatalf("the previous snapshot is not serving: %v", err)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindKey, "run-43"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the failed read left an object behind: %v", err)
	}
	if got, _ := byName(t, s, v1.KindModel, "gpt-5"); got.(*v1.Model).Spec.Pricing.Input.String() != "1" {
		t.Fatal("the failed read changed a Model")
	}

	// Put the Budget back and the read succeeds: the change, the addition,
	// the kept ids, the kept discovered Models of the unchanged Provider,
	// the dropped ones of the changed Provider, the rotated credential.
	writeFiles(t, dir, map[string]string{"3-budget.yaml": budgetYAML})
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	got, v := byName(t, s, v1.KindModel, "gpt-5")
	if got.(*v1.Model).Spec.Pricing.Input.String() != "3" || v != 2 {
		t.Fatalf("the changed Model = %s at version %d", got.(*v1.Model).Spec.Pricing.Input, v)
	}
	p2, pv := byName(t, s, v1.KindProvider, "openai")
	if p2.ID() != p.ID() || p2.(*v1.Provider).Status.Discovered == nil || pv != 1 {
		t.Fatalf("the unchanged Provider lost its id, its observed half, or moved its version: %+v at %d", p2, pv)
	}
	if v, _ := s.CredentialValue(p.ID()); v != "sk-rotated" {
		t.Fatalf("the credential was not re-read: %q", v)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindModel, "openai/o3"); err != nil {
		t.Fatalf("the unchanged Provider's discovered Model was dropped: %v", err)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindModel, "azure/o3"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the changed Provider's discovered Model was kept: %v", err)
	}
	if az2, _ := byName(t, s, v1.KindProvider, "azure"); az2.ID() != az.ID() {
		t.Fatal("the changed Provider lost its id")
	}
	k2, _ := byName(t, s, v1.KindKey, "run-43")
	sum := sha256.Sum256([]byte(env2["RUN_KEY2"]))
	if id, err := s.Keys().ByHash(ctx, hex.EncodeToString(sum[:])); err != nil || id != k2.ID() {
		t.Fatalf("the added Key's hash = %q, %v", id, err)
	}
	if s.Summary().Files != 6 || s.Summary().Kinds[v1.KindKey] != 2 {
		t.Fatalf("Summary = %+v", s.Summary())
	}

	// A removed Provider takes its discovered Models, its credential, and
	// a removed Key its hash.
	for _, name := range []string{"5-azure.yaml", "1-key.yaml"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("Reload with removals: %v", err)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindProvider, "azure"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the removed Provider: %v", err)
	}
	if _, ok := s.CredentialValue(az.ID()); ok {
		t.Fatal("the removed Provider's credential value stayed")
	}
	sum = sha256.Sum256([]byte(keyValue))
	if _, err := s.Keys().ByHash(ctx, hex.EncodeToString(sum[:])); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the removed Key's hash = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Objects().Get(ctx, v1.KindKey, k.ID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the removed Key = %v", err)
	}

	// A directory that is gone is a failed read, and the snapshot stays.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err == nil {
		t.Fatal("Reload of a removed directory")
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindProvider, "openai"); err != nil {
		t.Fatalf("the snapshot did not survive: %v", err)
	}
}

func TestFileModeIdsSurviveReRead(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	writeFiles(t, dir, full)
	s, err := Load(ctx, options(dir, env))
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]v1.Object{}
	for _, kn := range [][2]string{{v1.KindProvider, "openai"}, {v1.KindBudget, "team"}, {v1.KindModel, "gpt-5"}, {v1.KindKey, "run-42"}} {
		before[kn[1]], _ = byName(t, s, kn[0], kn[1])
	}
	writeFiles(t, dir, map[string]string{"4-provider.yaml": strings.ReplaceAll(providerYAML, "api.example.com/v1", "api.example.com/v2")})
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	for _, kn := range [][2]string{{v1.KindProvider, "openai"}, {v1.KindBudget, "team"}, {v1.KindModel, "gpt-5"}, {v1.KindKey, "run-42"}} {
		after, _ := byName(t, s, kn[0], kn[1])
		if after.ID() != before[kn[1]].ID() {
			t.Errorf("%s %s: id %s became %s", kn[0], kn[1], before[kn[1]].ID(), after.ID())
		}
	}
	p, v := byName(t, s, v1.KindProvider, "openai")
	if v != 2 || !p.(*v1.Provider).Status.CreatedAt.Equal(before["openai"].(*v1.Provider).Status.CreatedAt) {
		t.Fatalf("the changed Provider: version %d, createdAt moved", v)
	}
	// Ids are per start: another store over the same directory mints its own.
	other, err := Load(ctx, options(dir, env))
	if err != nil {
		t.Fatal(err)
	}
	if op, _ := byName(t, other, v1.KindProvider, "openai"); op.ID() == p.ID() {
		t.Fatal("two starts minted one id")
	}
}

// TestFileModeEventsArePerReplica is the store half of that row: two
// file-mode replicas share nothing, so each holds every lease and each
// journal is its own; the events themselves and the start-up line with a
// sink are spec 012's to raise once that lands.
func TestFileModeEventsArePerReplica(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	writeFiles(t, dir, full)
	a, err := Load(ctx, options(dir, env))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(ctx, options(dir, env))
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Store{"a": a, "b": b} {
		if held, err := s.Leases().Acquire(ctx, store.LeaseHealth, name, store.LeaseTTL); err != nil || !held {
			t.Fatalf("%s did not hold the lease: %v, %v", name, held, err)
		}
	}
	pa, _ := byName(t, a, v1.KindProvider, "openai")
	if _, err := a.Journal().Append(ctx, store.Event{ID: "evt_a", ObjectID: pa.ID(), Type: "provider.unreachable"}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := b.Journal().Since(ctx, 0, 0); len(rows) != 0 {
		t.Fatalf("b's journal saw a's event: %v", rows)
	}
}

func ptr[T any](v T) *T { return &v }
