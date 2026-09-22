// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestBootstrapIsIdempotent is criterion 11 of spec 035: a second apply
// over the same directory creates nothing and updates nothing, an edited
// Model is updated and nothing else is, and an existing Key is left as
// it is whatever its document and its variable say.
func TestBootstrapIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.catalog()
	first := h.apply()
	if first.Created != 4 || first.Updated != 0 || first.Unchanged != 0 {
		t.Fatalf("first apply: %+v", first)
	}
	versions, rows := h.versions(), len(h.records())

	second := h.apply()
	if second.Created != 0 || second.Updated != 0 || second.Unchanged != 4 {
		t.Fatalf("second apply over the same directory: %+v", second)
	}
	if got := h.versions(); !maps.Equal(got, versions) {
		t.Fatalf("versions moved on a second apply: %v, were %v", got, versions)
	}
	if got := len(h.records()); got != rows {
		t.Fatalf("a second apply journalled %d rows", got-rows)
	}

	h.write("models/gpt-4o.yaml", strings.Replace(modelDoc, `input: "2.50"`, `input: "3"`, 1))
	third := h.apply()
	if third.Created != 0 || third.Updated != 1 || third.Unchanged != 3 {
		t.Fatalf("apply after one edit: %+v", third)
	}
	for _, o := range third.Outcomes {
		if want := map[bool]Action{true: Updated, false: Unchanged}[o.Kind == v1.KindModel]; o.Action != want {
			t.Errorf("%s %s: %s, want %s", o.Kind, o.Name, o.Action, want)
		}
	}
	got := h.versions()
	for kind, v := range versions {
		want := v
		if kind == v1.KindModel {
			want = v + 1
		}
		if got[kind] != want {
			t.Errorf("%s at version %d after the Model's edit, want %d", kind, got[kind], want)
		}
	}
	recs := h.records()
	if len(recs) != rows+1 {
		t.Fatalf("the edit journalled %d rows, want 1", len(recs)-rows)
	}
	last := recs[len(recs)-1]
	if last.Type != events.ModelUpdated || last.Reason != events.ReasonBootstrap || last.Object.Name != "gpt-4o" {
		t.Fatalf("the edit's event: %+v", last)
	}
	if paths := anyStrings(last.Data.(map[string]any)["paths"]); !slices.Contains(paths, "spec.pricing.input") || slices.Contains(paths, "spec.pricing.output") {
		t.Errorf("the edit's paths: %v", paths)
	}
	m, _ := h.get(v1.KindModel, "gpt-4o")
	if price := m.(*v1.Model).Spec.Pricing.Input.String(); price != "3" {
		t.Errorf("the stored input price is %s", price)
	}

	// The Key's document and its variable both change; the Key stays.
	oldKey, keyVersion := h.get(v1.KindKey, "ci")
	h.write("keys/ci.yaml", strings.Replace(keyDoc, `models: ["gpt-4o"]`, `models: ["*"]`, 1))
	h.env["LUX_CI_KEY"] = "lux_ZZZZEfGhIjKlMnOpQrStUvWxYz0123456789_-"
	fourth := h.apply()
	if fourth.Created != 0 || fourth.Updated != 0 || fourth.Unchanged != 4 {
		t.Fatalf("apply after the Key's edit: %+v", fourth)
	}
	if o := fourth.Outcomes[3]; o.Kind != v1.KindKey || o.Action != Unchanged || !o.KeyKept {
		t.Errorf("the Key's outcome: %+v", o)
	}
	k, v := h.get(v1.KindKey, "ci")
	if v != keyVersion || !slices.Equal(k.(*v1.Key).Spec.Models, oldKey.(*v1.Key).Spec.Models) {
		t.Errorf("the Key changed: version %d, was %d; models %v", v, keyVersion, k.(*v1.Key).Spec.Models)
	}
	if id, err := h.st.Keys().ByHash(t.Context(), serve.HashKeyValue(keyValue)); err != nil || id != k.ID() {
		t.Errorf("the first value no longer opens the Key: %q, %v", id, err)
	}
	if _, err := h.st.Keys().ByHash(t.Context(), serve.HashKeyValue(h.env["LUX_CI_KEY"])); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the variable's new value was registered: %v", err)
	}
	if got := len(h.records()); got != rows+1 {
		t.Errorf("the kept Key journalled %d rows", got-rows-1)
	}
}

// TestBootstrapWritesInKindOrder: files whose path order is the reverse
// of kind order resolve and are written Providers, Budgets, Models,
// Keys, so the Model finds its Provider and the Key its Budget and Model.
func TestBootstrapWritesInKindOrder(t *testing.T) {
	h := newHarness(t)
	h.write("a-key.yaml", keyDoc)
	h.write("b-model.yaml", modelDoc)
	h.write("c-budget.yaml", budgetDoc)
	h.write("d-provider.yml", providerDoc)
	h.write("README.md", "not a manifest")
	res := h.apply()
	want := []string{v1.KindProvider, v1.KindBudget, v1.KindModel, v1.KindKey}
	var kinds, types []string
	for _, o := range res.Outcomes {
		kinds = append(kinds, o.Kind)
	}
	for _, r := range h.records() {
		types = append(types, r.Object.Kind)
	}
	if !slices.Equal(kinds, want) || !slices.Equal(types, want) {
		t.Fatalf("outcomes %v, journal %v; want %v", kinds, types, want)
	}
	if res.Files != 4 {
		t.Errorf("%d files read; a .yml file is read and a .md file is not", res.Files)
	}
	k, _ := h.get(v1.KindKey, "ci")
	b, _ := h.get(v1.KindBudget, "team")
	if ref := k.(*v1.Key).Status.Budget; ref == nil || ref.ID != b.ID() || ref.Name != "team" {
		t.Errorf("the Key's status.budget: %+v, the Budget is %s", ref, b.ID())
	}
	if sel := k.(*v1.Key).Status.Selectors; len(sel) != 1 || !slices.Equal(sel[0].Matched, []string{"gpt-4o"}) {
		t.Errorf("the Key's selectors: %+v", sel)
	}
}

// TestBootstrapReadsCredentialsFromTheEnvironment: a Provider's
// credential is sealed under the keys and a Key's value is hashed as the
// API stores a supplied value, neither object keeps valueFrom, both stay
// writable through /v1, and a rotated credential variable reseals the
// Provider once.
func TestBootstrapReadsCredentialsFromTheEnvironment(t *testing.T) {
	h := newHarness(t)
	h.catalog()
	h.apply()
	ctx := t.Context()

	p, _ := h.get(v1.KindProvider, "openai")
	prv := p.(*v1.Provider)
	if c := prv.Status.Credential; c == nil || !c.Set || c.Version != 1 {
		t.Fatalf("status.credential: %+v", c)
	}
	if prv.Spec.Credential == nil || prv.Spec.Credential.ValueFrom != nil {
		t.Fatalf("the stored credential block: %+v", prv.Spec.Credential)
	}
	if got := h.open(prv.ID()); got != credential {
		t.Fatalf("the sealed credential opens to %q", got)
	}
	k, _ := h.get(v1.KindKey, "ci")
	key := k.(*v1.Key)
	if key.Spec.ValueFrom != nil || key.Status.Prefix != serve.KeyPrefix(keyValue, true) || key.Status.Owner != owner {
		t.Fatalf("the stored Key: valueFrom %+v, prefix %q, owner %q", key.Spec.ValueFrom, key.Status.Prefix, key.Status.Owner)
	}
	if id, err := h.st.Keys().ByHash(ctx, serve.HashKeyValue(keyValue)); err != nil || id != key.ID() {
		t.Fatalf("the value's hash names %q, %v", id, err)
	}
	body, err := json.Marshal(prv)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{credential, keyValue} {
		for _, r := range h.records() {
			if raw, _ := json.Marshal(r); strings.Contains(string(raw), canary) {
				t.Errorf("an event carries a secret: %s", raw)
			}
		}
	}

	// What GET /v1/providers/openai answers re-applies through /v1.
	in, err := manifest.Decode(body, manifest.MediaJSON, manifest.Hint{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Resolve(ctx, in, manifest.Options{Lookup: newCatalog(h.st.Objects()), Existing: prv, PublicURL: h.options().PublicURL}); err != nil {
		t.Fatalf("the stored Provider does not re-apply through /v1: %v", err)
	}

	h.env["OPENAI_API_KEY"] = "sk-bootstrap-credential-rotated"
	res := h.apply()
	if res.Updated != 1 || res.Outcomes[0].Action != Updated {
		t.Fatalf("apply after the variable's rotation: %+v", res)
	}
	p, _ = h.get(v1.KindProvider, "openai")
	if c := p.(*v1.Provider).Status.Credential; !c.Set || c.Version != 2 || h.open(p.ID()) != "sk-bootstrap-credential-rotated" {
		t.Fatalf("after the rotation: %+v", c)
	}
	recs := h.records()
	if last := recs[len(recs)-1]; last.Type != events.ProviderUpdated || len(anyStrings(last.Data.(map[string]any)["paths"])) != 0 {
		t.Errorf("the rotation's event: %+v", last)
	}
	if res := h.apply(); res.Unchanged != 4 {
		t.Errorf("a restart after the rotation: %+v", res)
	}

	// A row that went missing under a set status is written again.
	if err := h.st.Credentials().Delete(ctx, p.ID()); err != nil {
		t.Fatal(err)
	}
	if res := h.apply(); res.Updated != 1 || h.open(p.ID()) != "sk-bootstrap-credential-rotated" {
		t.Errorf("a missing credential row: %+v", res)
	}
}

func (h *harness) open(providerID string) string {
	h.t.Helper()
	row, err := h.st.Credentials().Get(h.t.Context(), providerID)
	if err != nil {
		h.t.Fatal(err)
	}
	plain, err := h.keys.Open(providerID, row)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(plain)
}

// TestBootstrapRefusalNamesTheFile: a document that fails to decode, to
// resolve, or whose variable is unset ends the run with the file, the
// object, and the code, and writes nothing of the directory.
func TestBootstrapRefusalNamesTheFile(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		body  string
		env   map[string]string
		kind  string
		obj   string
		code  string
		paths []string
	}{
		{"an unknown field", "models/gpt-4o.yaml", strings.Replace(modelDoc, "spec:\n", "spec:\n  bogus: 1\n", 1), nil, v1.KindModel, "gpt-4o", "unknown_field", []string{"spec.bogus"}},
		{"a body that is not YAML", "models/gpt-4o.yaml", "kind: [unclosed\n", nil, "", "", "malformed_body", nil},
		{"two documents in one file", "models/gpt-4o.yaml", modelDoc + "---\n" + modelDoc, nil, v1.KindModel, "gpt-4o", "multi_document", nil},
		{"a Model naming an absent Provider", "models/gpt-4o.yaml", strings.Replace(modelDoc, "provider: openai", "provider: nowhere", 1), nil, v1.KindModel, "gpt-4o", "not_found", []string{"spec.targets[0].provider"}},
		{"an unset credential variable", "providers/openai.yaml", providerDoc, map[string]string{"OPENAI_API_KEY": ""}, v1.KindProvider, "openai", "missing_field", []string{"spec.credential.valueFrom.env"}},
		{"an unset Key variable", "keys/ci.yaml", keyDoc, map[string]string{"LUX_CI_KEY": ""}, v1.KindKey, "ci", "missing_field", []string{"spec.valueFrom.env"}},
		{"a variable that is not a POSIX name", "providers/openai.yaml", strings.Replace(providerDoc, "env: OPENAI_API_KEY", "env: openai-key", 1), nil, v1.KindProvider, "openai", "invalid_field", []string{"spec.credential.valueFrom.env"}},
		{"a credential and a variable", "providers/openai.yaml", strings.Replace(providerDoc, "    valueFrom:", "    value: inline\n    valueFrom:", 1), nil, v1.KindProvider, "openai", "exclusive_fields", []string{"spec.credential.value", "spec.credential.valueFrom.env"}},
		{"a Key with no value", "keys/ci.yaml", strings.Replace(keyDoc, "  valueFrom:\n    env: LUX_CI_KEY\n", "", 1), nil, v1.KindKey, "ci", "missing_field", []string{"spec.valueFrom.env"}},
		{"a Key value below the supplied length", "keys/ci.yaml", keyDoc, map[string]string{"LUX_CI_KEY": "short"}, v1.KindKey, "ci", "invalid_field", []string{"spec.valueFrom.env"}},
		{"an inline Key value below the supplied length", "keys/ci.yaml", strings.Replace(keyDoc, "  valueFrom:\n    env: LUX_CI_KEY\n", "  value: short\n", 1), nil, v1.KindKey, "ci", "invalid_field", []string{"spec.value"}},
		{"a document with no name", "budgets/team.yaml", strings.Replace(budgetDoc, "  name: team\n", "  labels: {tier: a}\n", 1), nil, v1.KindBudget, "", "missing_field", []string{"metadata.name"}},
		{"a name another file declares", "budgets/zz-copy.yaml", budgetDoc, nil, v1.KindBudget, "team", "already_exists", []string{"metadata.name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.catalog()
			h.write(tc.file, tc.body)
			maps.Copy(h.env, tc.env)
			e := h.refused(h.options())
			wantFile := h.dir + "/" + tc.file
			if e.File != wantFile || e.Kind != tc.kind || e.Name != tc.obj || e.Code != tc.code || !slices.Equal(e.Paths, tc.paths) {
				t.Fatalf("refusal %+v; want %s, %s %s, %s at %v", e, wantFile, tc.kind, tc.obj, tc.code, tc.paths)
			}
			prefix := wantFile + ": "
			if tc.kind != "" {
				prefix += strings.TrimSpace(tc.kind+" "+tc.obj) + ": "
			}
			if msg := e.Error(); !strings.HasPrefix(msg, prefix+tc.code) {
				t.Errorf("message %q, want the prefix %q", msg, prefix+tc.code)
			}
			h.storeIsEmpty()
		})
	}
}

// TestBootstrapUpdateFollowsTheUpdateRules: an edit /v1 would refuse as
// immutable is refused here too, naming the file and the path.
func TestBootstrapUpdateFollowsTheUpdateRules(t *testing.T) {
	h := newHarness(t)
	h.catalog()
	h.apply()
	h.write("providers/openai.yaml", strings.Replace(providerDoc, "dialect: openai", "dialect: anthropic", 1))
	e := h.refused(h.options())
	if e.Code != "immutable_field" || !slices.Equal(e.Paths, []string{"spec.dialect"}) || e.Kind != v1.KindProvider {
		t.Fatalf("refusal %+v", e)
	}
}

// TestBootstrapCheckWritesNothing: the dry run answers the counts an
// apply would produce and writes no object, hash, credential, or event,
// before and after an apply.
func TestBootstrapCheckWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.catalog()
	res, err := Check(t.Context(), h.options())
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Created != 4 || res.Updated != 0 || res.Unchanged != 0 {
		t.Fatalf("dry run over an empty store: %+v", res)
	}
	h.storeIsEmpty()
	if _, err := h.st.Keys().ByHash(t.Context(), serve.HashKeyValue(keyValue)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the dry run registered the Key's hash: %v", err)
	}

	h.apply()
	versions, rows := h.versions(), len(h.records())
	h.write("models/gpt-4o.yaml", strings.Replace(modelDoc, `output: "10"`, `output: "12"`, 1))
	h.env["OPENAI_API_KEY"] = "sk-bootstrap-credential-next"
	res, err = Check(t.Context(), h.options())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Updated != 2 || res.Unchanged != 2 {
		t.Fatalf("dry run after two edits: %+v", res)
	}
	if got := h.versions(); !maps.Equal(got, versions) || len(h.records()) != rows || h.open(h.id(v1.KindProvider, "openai")) != credential {
		t.Fatalf("the dry run wrote: versions %v, were %v", got, versions)
	}

	h.write("models/gpt-4o.yaml", strings.Replace(modelDoc, "provider: openai", "provider: nowhere", 1))
	if _, err := Check(t.Context(), h.options()); !strings.Contains(err.Error(), "gpt-4o.yaml: Model gpt-4o: not_found") {
		t.Fatalf("dry run refusal: %v", err)
	}
}

// TestBootstrapEventsFollowTheTable: every row a bootstrap writes is a
// row of events.Table with its members, the reason bootstrap, and no
// subject or request id, because no caller made the change.
func TestBootstrapEventsFollowTheTable(t *testing.T) {
	h := newHarness(t)
	h.catalog()
	h.apply()
	h.write("budgets/team.yaml", strings.Replace(budgetDoc, `amount: "10"`, `amount: "20"`, 1))
	h.apply()
	recs := h.records()
	types := make([]string, 0, len(recs))
	for _, r := range recs {
		types = append(types, r.Type)
		row, ok := events.Table[r.Type]
		if !ok {
			t.Errorf("%s is not in the table", r.Type)
			continue
		}
		if r.Reason != events.ReasonBootstrap || r.Subject != "" || r.RequestID != "" || r.Object.Owner != owner || !strings.HasPrefix(r.ID, v1.PrefixEvent) {
			t.Errorf("%s: %+v", r.Type, r)
		}
		data := r.Data.(map[string]any)
		for _, m := range row.Members {
			if _, ok := data[m]; !ok {
				t.Errorf("%s lacks %s: %v", r.Type, m, data)
			}
		}
		for m := range data {
			if !slices.Contains(row.Members, m) && !slices.Contains(row.Optional, m) {
				t.Errorf("%s carries %s, which its row does not name", r.Type, m)
			}
		}
	}
	want := []string{events.ProviderCreated, events.BudgetCreated, events.ModelCreated, events.KeyCreated, events.BudgetUpdated}
	if !slices.Equal(types, want) {
		t.Fatalf("types %v, want %v", types, want)
	}
}

// TestBootstrapKeyValueForms: a Key applies from a value in the file or
// from its hash as /v1 applies either, and a value another Key holds, a
// value two documents share, and a fenced name are refused before
// anything of the run is written.
func TestBootstrapKeyValueForms(t *testing.T) {
	inline := strings.Replace(keyDoc, "  valueFrom:\n    env: LUX_CI_KEY\n", "  value: "+keyValue+"\n", 1)
	hash := serve.HashKeyValue("an-operator-held-value-of-enough-length")
	hashed := strings.Replace(keyDoc, "  valueFrom:\n    env: LUX_CI_KEY\n", "  valueSHA256: "+hash+"\n", 1)

	t.Run("inline", func(t *testing.T) {
		h := newHarness(t)
		h.catalog()
		h.write("keys/ci.yaml", inline)
		h.apply()
		k, _ := h.get(v1.KindKey, "ci")
		if id, err := h.st.Keys().ByHash(t.Context(), serve.HashKeyValue(keyValue)); err != nil || id != k.ID() {
			t.Fatalf("%q, %v", id, err)
		}
	})
	t.Run("hashed", func(t *testing.T) {
		h := newHarness(t)
		h.catalog()
		h.write("keys/ci.yaml", hashed)
		h.apply()
		k, _ := h.get(v1.KindKey, "ci")
		if k.(*v1.Key).Status.Prefix != serve.SuppliedKeyPrefix(hash) {
			t.Errorf("prefix %q", k.(*v1.Key).Status.Prefix)
		}
		if id, err := h.st.Keys().ByHash(t.Context(), hash); err != nil || id != k.ID() {
			t.Fatalf("%q, %v", id, err)
		}
	})
	t.Run("a value another Key holds", func(t *testing.T) {
		h := newHarness(t)
		h.catalog()
		h.apply()
		h.write("keys/other.yaml", strings.Replace(keyDoc, "name: ci", "name: other", 1))
		e := h.refused(h.options())
		if e.Code != "invalid_field" || e.Name != "other" || !slices.Equal(e.Paths, []string{"spec.valueFrom.env"}) || e.Detail != hashTakenDetail {
			t.Fatalf("refusal %+v", e)
		}
	})
	t.Run("a value two documents share", func(t *testing.T) {
		h := newHarness(t)
		h.catalog()
		h.write("keys/other.yaml", strings.Replace(inline, "name: ci", "name: other", 1))
		e := h.refused(h.options())
		if e.Code != "invalid_field" || e.Name != "other" || !slices.Equal(e.Paths, []string{"spec.value"}) {
			t.Fatalf("refusal %+v", e)
		}
		h.storeIsEmpty()
	})
	t.Run("a fenced name", func(t *testing.T) {
		h := newHarness(t)
		h.catalog()
		if _, _, err := h.st.KeyFences().Put(t.Context(), store.KeyFence{Name: "ci", Owner: owner}); err != nil {
			t.Fatal(err)
		}
		e := h.refused(h.options())
		if e.Code != "key_fenced" || e.Name != "ci" {
			t.Fatalf("refusal %+v", e)
		}
	})
}

// TestBootstrapDeclaresOverADiscoveredModel: a discovered Model of the
// document's name counts as absent, as /v1 counts it, and the declared
// one replaces it in place under its id.
func TestBootstrapDeclaresOverADiscoveredModel(t *testing.T) {
	h := newHarness(t)
	h.write("providers/openai.yaml", providerDoc)
	h.apply()
	p, _ := h.get(v1.KindProvider, "openai")
	discovered := &v1.Model{Metadata: v1.ObjectMeta{Name: "gpt-4o"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: p.ID(), Model: "gpt-4o"}}}}
	discovered.Status.ID, discovered.Status.Owner, discovered.Status.Source = "mdl_01J9ZK2P7Q8R9S0T1U2V3W4X5Y", owner, v1.SourceDiscovered
	if _, err := h.st.Objects().Put(t.Context(), discovered, 0); err != nil {
		t.Fatal(err)
	}
	h.write("models/gpt-4o.yaml", modelDoc)
	res := h.apply()
	if res.Created != 1 || res.Unchanged != 1 {
		t.Fatalf("%+v", res)
	}
	m, _ := h.get(v1.KindModel, "gpt-4o")
	if m.ID() != discovered.Status.ID || m.(*v1.Model).Status.Source != v1.SourceDeclared {
		t.Fatalf("the declared Model: id %s, source %s", m.ID(), m.(*v1.Model).Status.Source)
	}
	recs := h.records()
	if last := recs[len(recs)-1]; last.Type != events.ModelCreated || last.Object.ID != discovered.Status.ID {
		t.Errorf("the event names %+v", last.Object)
	}
}

// TestBootstrapCredentialCustody: a credential with no keys to seal it
// under, and a stored credential the keys do not open, are refused
// rather than written or replaced.
func TestBootstrapCredentialCustody(t *testing.T) {
	h := newHarness(t)
	h.write("providers/openai.yaml", providerDoc)
	o := h.options()
	o.Keys = nil
	if e := h.refused(o); e.Code != codeInternal || e.Kind != v1.KindProvider {
		t.Fatalf("no keys: %+v", e)
	}
	h.apply()
	o = h.options()
	o.Keys = h.keyring(otherKEK)
	if e := h.refused(o); e.Code != codeInternal || !strings.Contains(e.Detail, "does not open") {
		t.Fatalf("another key: %+v", e)
	}
	// A Provider without a credential keeps what is stored.
	h.write("providers/openai.yaml", strings.Replace(providerDoc, "    valueFrom:\n      env: OPENAI_API_KEY\n", "    header: Authorization\n", 1))
	o = h.options()
	o.Keys = nil
	res, err := Apply(t.Context(), o)
	if err != nil || res.Unchanged != 1 {
		t.Fatalf("no value in the document: %+v, %v", res, err)
	}
	if h.open(h.id(v1.KindProvider, "openai")) != credential {
		t.Error("the stored credential changed")
	}
}

// TestBootstrapStoreFailures: a store that fails a read or the write is
// the refusal of the document it failed at, with the code /v1 maps the
// failure to.
func TestBootstrapStoreFailures(t *testing.T) {
	boom := errors.New("the database went away")
	cases := []struct {
		name  string
		setup func(h *harness)
		f     faulty
		kind  string
		code  string
		paths []string
	}{
		{"a read by name", nil, faulty{byName: boom}, v1.KindProvider, codeStoreUnavailable, nil},
		{"the Model list", nil, faulty{list: boom}, v1.KindKey, codeStoreUnavailable, nil},
		{"the fence read", nil, faulty{fences: boom}, v1.KindKey, codeStoreUnavailable, nil},
		{"the hash read", nil, faulty{byHash: boom}, v1.KindKey, codeStoreUnavailable, nil},
		{"the write", nil, faulty{transact: boom}, v1.KindProvider, codeStoreUnavailable, nil},
		{"a version conflict", nil, faulty{transact: store.ErrVersionConflict}, v1.KindProvider, codeConflict, nil},
		{"a name taken", nil, faulty{transact: store.ErrNameTaken}, v1.KindProvider, codeAlreadyExists, nil},
		{"the credential read", func(h *harness) { h.apply(); h.env["OPENAI_API_KEY"] = "sk-next" }, faulty{creds: boom}, v1.KindProvider, codeStoreUnavailable, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.catalog()
			if tc.setup != nil {
				tc.setup(h)
			}
			o := h.options()
			f := tc.f
			f.Store = h.st
			o.Store = &f
			e := h.refused(o)
			if e.Code != tc.code || e.Kind != tc.kind || !slices.Equal(e.Paths, tc.paths) {
				t.Fatalf("refusal %+v; want %s %s at %v", e, tc.kind, tc.code, tc.paths)
			}
		})
	}
	t.Run("a hash taken at the write", func(t *testing.T) {
		h := newHarness(t)
		h.write("keys/ci.yaml", strings.Replace(strings.Replace(keyDoc, `models: ["gpt-4o"]`, `models: ["*"]`, 1), "  budget: team\n", "", 1))
		o := h.options()
		f := &faulty{Store: h.st, transact: store.ErrHashTaken}
		o.Store = f
		e := h.refused(o)
		if e.Code != codeInvalidField || !slices.Equal(e.Paths, []string{"spec.valueFrom.env"}) || e.Detail != hashTakenDetail {
			t.Fatalf("refusal %+v", e)
		}
	})
}

// TestFailMapsEveryStoreError: each error the store names takes the
// code the API's error mapping gives it.
func TestFailMapsEveryStoreError(t *testing.T) {
	d := &document{path: "d/x.yaml", kind: v1.KindKey, name: "x"}
	want := map[error]string{
		store.ErrKeyFenced:       codeKeyFenced,
		store.ErrFenceConflict:   codeFenceConflict,
		store.ErrNotFound:        codeNotFound,
		store.ErrVersionConflict: codeConflict,
		store.ErrNameTaken:       codeAlreadyExists,
		store.ErrHashTaken:       codeInvalidField,
		store.ErrReadOnly:        codeReadOnly,
		store.ErrInvalidCursor:   codeStoreUnavailable,
	}
	for err, code := range want {
		e := d.fail(err, "spec.value")
		if e.Code != code || !errors.Is(e, err) {
			t.Errorf("%v: %+v, want %s", err, e, code)
		}
	}
	inner := d.refuse(codeInternal, "x")
	if d.fail(inner, "") != inner {
		t.Error("a refusal is not passed through")
	}
}

// TestOptionsAreChecked: a run without a directory, without a store, or
// with an owner that is not a rendered subject is a plain error, and so
// is a directory that cannot be read.
func TestOptionsAreChecked(t *testing.T) {
	h := newHarness(t)
	edits := map[string]func(*Options){
		"no directory":   func(o *Options) { o.Dir = "" },
		"no store":       func(o *Options) { o.Store = nil },
		"no owner":       func(o *Options) { o.Owner = "" },
		"a bare subject": func(o *Options) { o.Owner = "alice" },
		"an empty half":  func(o *Options) { o.Owner = "|alice" },
		"a padded owner": func(o *Options) { o.Owner = " " + owner },
		"a missing dir":  func(o *Options) { o.Dir = h.dir + "/absent" },
	}
	for name, edit := range edits {
		o := h.options()
		edit(&o)
		_, err := Apply(t.Context(), o)
		if _, refused := errors.AsType[*Error](err); err == nil || refused {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestDefaultsOfTheZeroOptions: nil Getenv, Now, and NewID read the
// process environment, the wall clock, and v1.NewID.
func TestDefaultsOfTheZeroOptions(t *testing.T) {
	h := newHarness(t)
	h.catalog()
	t.Setenv("OPENAI_API_KEY", credential)
	t.Setenv("LUX_CI_KEY", keyValue)
	o := h.options()
	o.Getenv, o.Now, o.NewID = nil, nil, nil
	res, err := Apply(t.Context(), o)
	if err != nil || res.Created != 4 {
		t.Fatalf("%+v, %v", res, err)
	}
	for _, r := range h.records() {
		if len(r.ID) != len(v1.PrefixEvent)+26 {
			t.Errorf("event id %q", r.ID)
		}
	}
}

// TestResultLines: one line per outcome in the file mode's shape, the
// kept Key named as such, and a summary that says what a dry run did not
// write.
func TestResultLines(t *testing.T) {
	r := Result{Dir: "deploy/catalog", Files: 3}
	r.add(Outcome{File: "providers/openai.yaml", Kind: v1.KindProvider, Name: "openai", Action: Created})
	r.add(Outcome{File: "models/gpt-4o.yaml", Kind: v1.KindModel, Name: "gpt-4o", Action: Updated})
	r.add(Outcome{File: "keys/ci.yaml", Kind: v1.KindKey, Name: "ci", Action: Unchanged, KeyKept: true})
	want := []string{
		"bootstrap dir deploy/catalog: providers/openai.yaml: Provider openai created",
		"bootstrap dir deploy/catalog: models/gpt-4o.yaml: Model gpt-4o updated",
		"bootstrap dir deploy/catalog: keys/ci.yaml: Key ci unchanged; a Key that exists is left as it is, because its value is write-once",
	}
	if got := r.Lines(); !slices.Equal(got, want) {
		t.Errorf("lines:\n%s", strings.Join(got, "\n"))
	}
	if got := r.Summary(); got != "bootstrap dir deploy/catalog: 3 files read, 1 created, 1 updated, 1 unchanged" {
		t.Errorf("summary %q", got)
	}
	r.DryRun = true
	if got := r.Lines()[0]; got != "bootstrap dir deploy/catalog: providers/openai.yaml: Provider openai to be created" {
		t.Errorf("dry run line %q", got)
	}
	if got := r.Summary(); got != "bootstrap dir deploy/catalog: 3 files read, 1 to create, 1 to update, 1 unchanged; nothing written" {
		t.Errorf("dry run summary %q", got)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// TestBootstrapResolvesAgainstTheStore: a reference the directory does
// not declare resolves against the store, by name and by id, as /v1
// resolves it, so a directory may name a Provider or a Budget an earlier
// apply or a caller created.
func TestBootstrapResolvesAgainstTheStore(t *testing.T) {
	h := newHarness(t)
	h.write("providers/openai.yaml", providerDoc)
	h.write("budgets/team.yaml", budgetDoc)
	h.apply()
	pid := h.id(v1.KindProvider, "openai")
	bid := h.id(v1.KindBudget, "team")

	h.dir = t.TempDir()
	h.write("models/gpt-4o.yaml", strings.Replace(modelDoc, "provider: openai", "provider: "+pid, 1))
	h.write("keys/ci.yaml", strings.Replace(keyDoc, "budget: team", "budget: "+bid, 1))
	res := h.apply()
	if res.Created != 2 {
		t.Fatalf("%+v", res)
	}
	k, _ := h.get(v1.KindKey, "ci")
	if ref := k.(*v1.Key).Status.Budget; ref == nil || ref.ID != bid {
		t.Errorf("status.budget %+v", ref)
	}

	h.dir = t.TempDir()
	h.write("keys/other.yaml", strings.Replace(strings.Replace(keyDoc, "name: ci", "name: other", 1), "LUX_CI_KEY", "LUX_OTHER_KEY", 1))
	h.env["LUX_OTHER_KEY"] = "lux_OtherGhIjKlMnOpQrStUvWxYz0123456789_-"
	if res := h.apply(); res.Created != 1 {
		t.Fatalf("a Key naming the stored Budget by name: %+v", res)
	}
	h.write("keys/other.yaml", strings.Replace(strings.Replace(keyDoc, "name: ci", "name: third", 1), "budget: team", "budget: absent", 1))
	if e := h.refused(h.options()); e.Code != codeNotFound || !slices.Equal(e.Paths, []string{"spec.budget"}) {
		t.Fatalf("a Budget in neither: %+v", e)
	}
}
