// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The fixtures every test of this package builds from: one object per
// kind with every field spec 006's resource table names set. The
// vocabulary they are rendered through is latere.ai/x/lux/authorizer's.
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
		authorizer.ActionProviderCreate: {"kind": "Provider", "name": "team-openai", "dialect": "openai", "baseURL": "https://api.openai.example/v1",
			"tunnel": false, "labels": labels("team", "research")},
		authorizer.ActionProviderRead:   provider,
		authorizer.ActionProviderUpdate: provider,
		authorizer.ActionProviderDelete: provider,
		authorizer.ActionProviderTunnel: provider,
		authorizer.ActionProviderList:   {"kind": "Provider"},
		authorizer.ActionModelCreate: {"kind": "Model", "name": "gpt-5", "labels": labels("tier", "gold"),
			"targets": []any{map[string]any{"provider": "team-openai", "model": "gpt-5-2026"}, map[string]any{"provider": "relay", "model": "openai/gpt-5"}}},
		authorizer.ActionModelRead:   model,
		authorizer.ActionModelUpdate: model,
		authorizer.ActionModelDelete: model,
		authorizer.ActionModelList:   {"kind": "Model"},
		authorizer.ActionModelUse: {"kind": "Model", "selector": "anthropic/*",
			"matched": []any{map[string]any{"id": "mdl_01J9TESTMODEL00000000000000", "name": "gpt-5", "owner": fixtureSubject, "labels": map[string]any{}}}},
		authorizer.ActionKeyCreate:    {"kind": "Key", "name": "run-42", "labels": labels("run", "r_42"), "models": []any{"gpt-5", "anthropic/*"}, "budget": "team-research"},
		authorizer.ActionKeyRead:      key,
		authorizer.ActionKeyUpdate:    key,
		authorizer.ActionKeyDelete:    key,
		authorizer.ActionKeyList:      {"kind": "Key"},
		authorizer.ActionKeyFence:     {"kind": "KeyFence", "id": "run-42", "name": "run-42", "owner": fixtureSubject, "labels": labels("run", "r_42")},
		authorizer.ActionKeyFenceRead: {"kind": "KeyFence", "id": "run-42", "name": "run-42"},
		authorizer.ActionBudgetCreate: {"kind": "Budget", "name": "team-research", "amount": "50", "currency": "USD", "window": "month",
			"labels": labels("team", "research")},
		authorizer.ActionBudgetRead:   budget,
		authorizer.ActionBudgetUpdate: budget,
		authorizer.ActionBudgetDelete: budget,
		authorizer.ActionBudgetList:   {"kind": "Budget"},
		authorizer.ActionBudgetDraw:   budget,
		authorizer.ActionUsageRead:    {"kind": "Usage", "keys": []any{"key_01J9TESTKEY0000000000000000"}, "owners": []any{fixtureSubject}},
		authorizer.ActionOwnerAssign:  {"kind": "Ownership", "target_kind": "Key", "name": "run-42", "owner": fixtureSubject, "proposed": map[string]any{"owner": fixtureSubject}},
	}
}

// resourceOf builds the resource of one action from the fixtures, the
// way the API and the Lookup will.
func resourceOf(t *testing.T, action string) authz.Resource {
	t.Helper()
	switch action {
	case authorizer.ActionKeyFence:
		return authorizer.KeyFenceInstall("run-42", fixtureSubject, map[string]string{"run": "r_42"})
	case authorizer.ActionKeyFenceRead:
		return authorizer.KeyFenceRead("run-42")
	case authorizer.ActionOwnerAssign:
		return authorizer.OwnerAssignment("Key", "run-42", fixtureSubject, map[string]any{"owner": fixtureSubject})
	case authorizer.ActionModelUse:
		return authorizer.ModelUse("anthropic/*", fixtureMatched)
	case authorizer.ActionUsageRead:
		return authorizer.UsageRead([]string{"key_01J9TESTKEY0000000000000000"}, []string{fixtureSubject})
	}
	var obj v1.Object
	switch authorizer.Kind(action) {
	case v1.KindProvider:
		obj = fixtureProvider()
	case v1.KindModel:
		obj = fixtureModel()
	case v1.KindKey:
		obj = fixtureKey()
	case v1.KindBudget:
		obj = fixtureBudget(t)
	}
	res, ok := authorizer.ResourceFor(action, obj)
	if !ok {
		t.Fatalf("authorizer.ResourceFor(%s, %T) built nothing", action, obj)
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
