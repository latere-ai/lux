// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The fixtures every shape test builds from: one object per kind with
// every field the table names set.
const (
	fixtureIssuer  = "https://login.example.com"
	fixtureSubject = fixtureIssuer + "|alice"
)

func money(t *testing.T, s string) *v1.Money {
	t.Helper()
	m, err := v1.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return &m
}

func fixtureProvider() *v1.Provider {
	return &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "team-openai", Labels: map[string]string{"team": "research"}},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.openai.example/v1"},
		Status:   v1.ProviderStatus{ID: "prv_01J9TESTPROVIDER0000000000", Owner: fixtureSubject},
	}
}

func fixtureModel() *v1.Model {
	return &v1.Model{
		Metadata: v1.ObjectMeta{Name: "gpt-5", Labels: map[string]string{"tier": "gold"}},
		Spec:     v1.ModelSpec{Targets: []v1.Target{{Provider: "team-openai", Model: "gpt-5-2026"}, {Provider: "relay", Model: "openai/gpt-5"}}},
		Status:   v1.ModelStatus{ID: "mdl_01J9TESTMODEL00000000000000", Owner: fixtureSubject, Source: v1.SourceDeclared},
	}
}

func fixtureKey() *v1.Key {
	return &v1.Key{
		Metadata: v1.ObjectMeta{Name: "run-42", Labels: map[string]string{"run": "r_42"}},
		Spec:     v1.KeySpec{Models: []string{"gpt-5", "anthropic/*"}, Budget: "team-research"},
		Status:   v1.KeyStatus{ID: "key_01J9TESTKEY0000000000000000", Owner: fixtureSubject, Prefix: "lux_abcdefgh"},
	}
}

func fixtureBudget(t *testing.T) *v1.Budget {
	return &v1.Budget{
		Metadata: v1.ObjectMeta{Name: "team-research", Labels: map[string]string{"team": "research"}},
		Spec:     v1.BudgetSpec{Amount: money(t, "50"), Currency: "USD", Window: "month"},
		Status:   v1.BudgetStatus{ID: "bud_01J9TESTBUDGET00000000000000", Owner: fixtureSubject},
	}
}

var fixtureMatched = []v1.ModelRef{{ID: "mdl_01J9TESTMODEL00000000000000", Name: "gpt-5", Owner: fixtureSubject}}

// shapes is spec 006's resource table as the flat JSON object each
// action's resource marshals to, keyed by action.
func shapes(t *testing.T) map[string]map[string]any {
	t.Helper()
	labels := func(k, v string) map[string]any { return map[string]any{k: v} }
	provider := map[string]any{"kind": "Provider", "id": "prv_01J9TESTPROVIDER0000000000", "name": "team-openai", "owner": fixtureSubject,
		"dialect": "openai", "baseURL": "https://api.openai.example/v1", "tunnel": false, "labels": labels("team", "research")}
	model := map[string]any{"kind": "Model", "id": "mdl_01J9TESTMODEL00000000000000", "name": "gpt-5", "owner": fixtureSubject,
		"source": "declared", "labels": labels("tier", "gold")}
	key := map[string]any{"kind": "Key", "id": "key_01J9TESTKEY0000000000000000", "name": "run-42", "owner": fixtureSubject,
		"prefix": "lux_abcdefgh", "labels": labels("run", "r_42")}
	budget := map[string]any{"kind": "Budget", "id": "bud_01J9TESTBUDGET00000000000000", "name": "team-research", "owner": fixtureSubject,
		"labels": labels("team", "research")}
	return map[string]map[string]any{
		ActionProviderCreate: {"kind": "Provider", "name": "team-openai", "dialect": "openai", "baseURL": "https://api.openai.example/v1",
			"tunnel": false, "labels": labels("team", "research")},
		ActionProviderRead:   provider,
		ActionProviderUpdate: provider,
		ActionProviderDelete: provider,
		ActionProviderTunnel: provider,
		ActionProviderList:   {"kind": "Provider"},
		ActionModelCreate: {"kind": "Model", "name": "gpt-5", "labels": labels("tier", "gold"),
			"targets": []any{map[string]any{"provider": "team-openai", "model": "gpt-5-2026"}, map[string]any{"provider": "relay", "model": "openai/gpt-5"}}},
		ActionModelRead:   model,
		ActionModelUpdate: model,
		ActionModelDelete: model,
		ActionModelList:   {"kind": "Model"},
		ActionModelUse: {"kind": "Model", "selector": "anthropic/*",
			"matched": []any{map[string]any{"id": "mdl_01J9TESTMODEL00000000000000", "name": "gpt-5", "owner": fixtureSubject, "labels": map[string]any{}}}},
		ActionKeyCreate:    {"kind": "Key", "name": "run-42", "labels": labels("run", "r_42"), "models": []any{"gpt-5", "anthropic/*"}, "budget": "team-research", "budgets": []any{}},
		ActionKeyRead:      key,
		ActionKeyUpdate:    key,
		ActionKeyDelete:    key,
		ActionKeyList:      {"kind": "Key"},
		ActionKeyFence:     {"kind": "KeyFence", "id": "run-42", "name": "run-42", "owner": fixtureSubject, "labels": labels("run", "r_42")},
		ActionKeyFenceRead: {"kind": "KeyFence", "id": "run-42", "name": "run-42"},
		ActionBudgetCreate: {"kind": "Budget", "name": "team-research", "amount": "50", "currency": "USD", "window": "month",
			"labels": labels("team", "research")},
		ActionBudgetRead:   budget,
		ActionBudgetUpdate: budget,
		ActionBudgetDelete: budget,
		ActionBudgetList:   {"kind": "Budget"},
		ActionBudgetDraw:   budget,
		ActionUsageRead:    {"kind": "Usage", "keys": []any{"key_01J9TESTKEY0000000000000000"}, "owners": []any{fixtureSubject}},
		ActionUsageRedact:  {"kind": "Usage", "owner": fixtureSubject},
		ActionOwnerAssign:  {"kind": "Ownership", "target_kind": "Key", "name": "run-42", "owner": fixtureSubject, "proposed": map[string]any{"owner": fixtureSubject}},
	}
}

// resourceOf builds the resource of one action from the fixtures, the
// way the API and the Lookup will.
func resourceOf(t *testing.T, action string) authz.Resource {
	t.Helper()
	switch action {
	case ActionKeyFence:
		return KeyFenceInstall("run-42", fixtureSubject, map[string]string{"run": "r_42"})
	case ActionKeyFenceRead:
		return KeyFenceRead("run-42")
	case ActionOwnerAssign:
		return OwnerAssignment("Key", "run-42", fixtureSubject, map[string]any{"owner": fixtureSubject})
	case ActionModelUse:
		return ModelUse("anthropic/*", fixtureMatched)
	case ActionUsageRead:
		return UsageRead([]string{"key_01J9TESTKEY0000000000000000"}, []string{fixtureSubject})
	case ActionUsageRedact:
		return UsageRedact(fixtureSubject)
	}
	var obj v1.Object
	switch Kind(action) {
	case v1.KindProvider:
		obj = fixtureProvider()
	case v1.KindModel:
		obj = fixtureModel()
	case v1.KindKey:
		obj = fixtureKey()
	case v1.KindBudget:
		obj = fixtureBudget(t)
	}
	res, ok := ResourceFor(action, obj)
	if !ok {
		t.Fatalf("ResourceFor(%s, %T) built nothing", action, obj)
	}
	return res
}

// flat is the resource as the wire carries it, decoded back to a generic
// object, so a comparison reads what an authorizer reads.
func flat(t *testing.T, res authz.Resource) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestResourceShapes: every action's resource is exactly the table's
// flat object, kind and id beside the fields, and a create carries no id.
func TestResourceShapes(t *testing.T) {
	want := shapes(t)
	for _, action := range Actions() {
		t.Run(action, func(t *testing.T) {
			res := resourceOf(t, action)
			if got := flat(t, res); !reflect.DeepEqual(got, want[action]) {
				t.Errorf("resource of %s:\n got %v\nwant %v", action, got, want[action])
			}
			if res.Kind != Kind(action) {
				t.Errorf("resource kind %q, want %q", res.Kind, Kind(action))
			}
			if res.ID != "" && strings.HasSuffix(action, ".create") {
				t.Errorf("a create carries id %q", res.ID)
			}
		})
	}
	if len(want) != len(Actions()) {
		t.Fatalf("the table has %d rows and the vocabulary %d actions", len(want), len(Actions()))
	}
}

// TestResourceListsAreNeverNull: an object with no labels, targets,
// models, matches, keys, or owners still carries each as an empty list
// or object, so an authorizer in any language reads a container.
func TestResourceListsAreNeverNull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		res    authz.Resource
		fields []string
	}{
		{"provider.create", ProviderCreate(&v1.Provider{}), []string{"labels"}},
		{"model.create", ModelCreate(&v1.Model{}), []string{"labels", "targets"}},
		{"model.use", ModelUse("*", nil), []string{"matched"}},
		{"key.create", KeyCreate(&v1.Key{}), []string{"labels", "models"}},
		{"budget.create", BudgetCreate(&v1.Budget{}), []string{"labels"}},
		{"usage.read", UsageRead(nil, nil), []string{"keys", "owners"}},
	} {
		got := flat(t, tc.res)
		for _, f := range tc.fields {
			if got[f] == nil {
				t.Errorf("%s: %s is null", tc.name, f)
			}
		}
	}
	if got := flat(t, BudgetCreate(&v1.Budget{})); got["amount"] != "" {
		t.Errorf("an absent amount reads %v, want the empty string", got["amount"])
	}
}

// impostor claims a kind without being that kind's type.
type impostor struct{ v1.Object }

func (impostor) Kind() string { return v1.KindKey }

// TestResourceForRefusesTheWrongKind: an action on an object of another
// kind, an action whose resource is not an object, a string outside the
// vocabulary, and a value that claims a kind it is not build nothing.
func TestResourceForRefusesTheWrongKind(t *testing.T) {
	for _, tc := range []struct {
		action string
		obj    v1.Object
	}{
		{ActionKeyRead, fixtureProvider()},
		{ActionModelUse, fixtureModel()},
		{ActionUsageRead, fixtureKey()},
		{"key.rotate", fixtureKey()},
		{ActionKeyRead, nil},
		{ActionKeyRead, impostor{}},
	} {
		if _, ok := ResourceFor(tc.action, tc.obj); ok {
			t.Errorf("ResourceFor(%s, %T) built a resource", tc.action, tc.obj)
		}
	}
}

// TestActionsAndKinds: the vocabulary is the table's twenty-eight
// actions, each maps to its kind, and anything else maps to none.
func TestActionsAndKinds(t *testing.T) {
	all := Actions()
	if len(all) != 28 || len(slices.Compact(slices.Sorted(slices.Values(all)))) != 28 {
		t.Fatalf("Actions() = %v", all)
	}
	for _, a := range all {
		prefix, _, _ := strings.Cut(a, ".")
		want := map[string]string{"provider": "Provider", "model": "Model", "key": "Key", "budget": "Budget", "usage": KindUsage, "owner": KindOwnership}[prefix]
		if a == ActionKeyFence || a == ActionKeyFenceRead {
			want = KindKeyFence
		}
		if Kind(a) != want || !Known(a) {
			t.Errorf("Kind(%s) = %q, Known %v; want %q", a, Kind(a), Known(a), want)
		}
	}
	for _, a := range []string{"", "key", "key.rotate", "usage.write", "Provider.read"} {
		if Kind(a) != "" || Known(a) {
			t.Errorf("Kind(%q) = %q, Known %v; want none", a, Kind(a), Known(a))
		}
	}
	all[0] = "changed"
	if Actions()[0] == "changed" {
		t.Error("Actions() hands out its own backing array")
	}
}

// TestVocabularyIsTheTable: Vocabulary carries the twenty-eight rows in
// the table's order, each action with the kind it acts on, and the three
// older reads are that value read three ways rather than a second table.
func TestVocabularyIsTheTable(t *testing.T) {
	v := Vocabulary()
	if v.Core != "lux" {
		t.Errorf("Core = %q, want lux", v.Core)
	}
	if len(v.Actions) != 28 {
		t.Fatalf("the vocabulary has %d rows, want the table's twenty-eight", len(v.Actions))
	}
	names := make([]string, 0, len(v.Actions))
	for _, a := range v.Actions {
		names = append(names, a.Name)
		kind, ok := v.Kind(a.Name)
		if !ok || kind != a.Kind || kind != Kind(a.Name) || !Known(a.Name) {
			t.Errorf("%s: the row says %q, Kind says %q, Known %v", a.Name, a.Kind, Kind(a.Name), Known(a.Name))
		}
	}
	if !slices.Equal(names, Actions()) {
		t.Errorf("Actions() = %v, and the vocabulary names %v", Actions(), names)
	}
	if got, want := v.Kinds(), []string{v1.KindProvider, v1.KindModel, v1.KindKey, KindKeyFence, v1.KindBudget, KindUsage, KindOwnership}; !slices.Equal(got, want) {
		t.Errorf("Kinds() = %v, want %v", got, want)
	}
	for _, a := range []string{"", "key", "key.rotate", "usage.write", "Provider.read"} {
		if kind, ok := v.Kind(a); ok || kind != "" || v.Known(a) {
			t.Errorf("the vocabulary answers %q with %q, %v; want none", a, kind, ok)
		}
	}
	v.Actions[0] = authz.Action{Name: "changed", Kind: "Changed"}
	if Vocabulary().Actions[0].Name == "changed" || Actions()[0] == "changed" {
		t.Error("Vocabulary() hands out the package's own table")
	}
}

// TestVocabularyConstructs: the table above is one NewVocabulary
// accepts, so importing the package cannot panic; must refuses one it
// does not, which is the programming error it stands for.
func TestVocabularyConstructs(t *testing.T) {
	if _, err := authz.NewVocabulary(vocabulary.Core, vocabulary.Actions...); err != nil {
		t.Fatalf("the declared table does not construct: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("must accepted a table NewVocabulary refused")
		}
	}()
	must(authz.NewVocabulary("lux",
		authz.Action{Name: ActionKeyRead, Kind: v1.KindKey},
		authz.Action{Name: ActionKeyRead, Kind: v1.KindKey},
	))
}

// TestVocabularyLabelsEveryKind: each of the five resource kinds carries
// the heading a person reads, so a grant picker groups the table by
// function without anybody hard-coding the headings (spec 006's table).
// The label travels with the copy Vocabulary() hands out, which is the
// whole point of declaring it: a consumer reads the table and the
// headings from one value.
func TestVocabularyLabelsEveryKind(t *testing.T) {
	want := map[string]string{
		v1.KindProvider: "Providers",
		v1.KindModel:    "Models",
		v1.KindKey:      "Keys",
		v1.KindBudget:   "Budgets",
		KindUsage:       "Usage",
		KindOwnership:   "Ownership",
		KindKeyFence:    "Key fences",
	}
	v := Vocabulary()
	for _, kind := range v.Kinds() {
		if got := v.Label(kind); got != want[kind] {
			t.Errorf("Label(%q) = %q, want %q", kind, got, want[kind])
		}
	}
	if len(v.Kinds()) != len(want) {
		t.Errorf("the table names %v; a label is declared for %d kinds", v.Kinds(), len(want))
	}
	// A kind the table does not name reads as itself, which is the
	// shared type's own answer and not this package's.
	if got := v.Label("Sandbox"); got != "Sandbox" {
		t.Errorf("Label of a kind outside the table = %q, want the kind", got)
	}
}

func TestModelUseCopiesLabels(t *testing.T) {
	refs := []v1.ModelRef{{ID: "model", Name: "model", Owner: fixtureSubject, Labels: map[string]string{"tenant": "a"}}}
	r := ModelUse("model", refs)
	refs[0].Labels["tenant"] = "b"
	entries, ok := r.Fields["matched"].([]map[string]any)
	if !ok {
		t.Fatal("missing models")
	}
	labels, ok := entries[0]["labels"].(map[string]string)
	if !ok || labels["tenant"] != "a" {
		t.Fatal("resource aliases model labels", entries)
	}
}
