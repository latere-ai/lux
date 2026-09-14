// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/manifest"
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
			"matched": []any{map[string]any{"id": "mdl_01J9TESTMODEL00000000000000", "name": "gpt-5", "owner": fixtureSubject}}},
		ActionKeyCreate: {"kind": "Key", "name": "run-42", "labels": labels("run", "r_42"), "models": []any{"gpt-5", "anthropic/*"}, "budget": "team-research"},
		ActionKeyRead:   key,
		ActionKeyUpdate: key,
		ActionKeyDelete: key,
		ActionKeyList:   {"kind": "Key"},
		ActionBudgetCreate: {"kind": "Budget", "name": "team-research", "amount": "50", "currency": "USD", "window": "month",
			"labels": labels("team", "research")},
		ActionBudgetRead:   budget,
		ActionBudgetUpdate: budget,
		ActionBudgetDelete: budget,
		ActionBudgetList:   {"kind": "Budget"},
		ActionBudgetDraw:   budget,
		ActionUsageRead:    {"kind": "Usage", "keys": []any{"key_01J9TESTKEY0000000000000000"}, "owners": []any{fixtureSubject}},
	}
}

// resourceOf builds the resource of one action from the fixtures, the
// way the API and the Lookup will.
func resourceOf(t *testing.T, action string) authz.Resource {
	t.Helper()
	switch action {
	case ActionModelUse:
		return ModelUse("anthropic/*", fixtureMatched)
	case ActionUsageRead:
		return UsageRead([]string{"key_01J9TESTKEY0000000000000000"}, []string{fixtureSubject})
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

// TestResourceForRefusesTheWrongKind: an action on an object of another
// kind, an action whose resource is not an object, and a string outside
// the vocabulary build nothing.
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
	} {
		if _, ok := ResourceFor(tc.action, tc.obj); ok {
			t.Errorf("ResourceFor(%s, %T) built a resource", tc.action, tc.obj)
		}
	}
}

// TestActionsAndKinds: the vocabulary is the table's twenty-four
// actions, each maps to its kind, and anything else maps to none.
func TestActionsAndKinds(t *testing.T) {
	all := Actions()
	if len(all) != 24 || len(slices.Compact(slices.Sorted(slices.Values(all)))) != 24 {
		t.Fatalf("Actions() = %v", all)
	}
	for _, a := range all {
		prefix, _, _ := strings.Cut(a, ".")
		want := map[string]string{"provider": "Provider", "model": "Model", "key": "Key", "budget": "Budget", "usage": KindUsage}[prefix]
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

// TestCodesHaveOneSentence: every code has one fixed user sentence, the
// unavailability one is manifest's, and the developer's line carries the
// code and the detail apart from it.
func TestCodesHaveOneSentence(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Codes() {
		msg := c.Message()
		if msg == "" || !strings.HasSuffix(msg, ".") || strings.Count(msg, ". ") != 0 || seen[msg] {
			t.Errorf("code %s: sentence %q", c, msg)
		}
		seen[msg] = true
	}
	if CodeAuthorizerUnavailable.Message() != manifest.CodeAuthorizerUnavailable.Message() {
		t.Error("authorizer_unavailable reads differently here and in manifest")
	}
	if Code("nope").Message() != "" {
		t.Error("an unknown code has a sentence")
	}
	e := refuse(CodeForbidden, "authz deny: not yours")
	if e.Message != "You do not have permission to do this." || e.Error() != "forbidden: authz deny: not yours" {
		t.Errorf("refuse() = %+v", e)
	}
	if (&Error{Code: CodeUnauthenticated}).Error() != "unauthenticated" {
		t.Error("an Error without detail renders more than its code")
	}
}
