// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/serve"
	v1 "latere.ai/x/lux/manifest/v1"
)

var mintedShape = regexp.MustCompile(`^lux_[A-Za-z0-9_-]{40}$`)

// keyValue reads status.value off a create or rotate response.
func keyValue(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	v, _ := status(t, rec)["value"].(string)
	if !mintedShape.MatchString(v) {
		t.Fatalf("status.value %q is not a minted value", v)
	}
	return v
}

// TestRotate: the rotate answers a new status.value, keeps the id, the
// name, the spec, the owner, and status.usage, rewrites the prefix,
// replaces the hash so the old value opens nothing and the new one the
// Key, advances the version, and journals key.rotated with both
// prefixes.
func TestRotate(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	created := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	before := body(t, created)
	old := keyValue(t, created)
	id := before["status"].(map[string]any)["id"].(string)
	rotated := h.request(http.MethodPost, "/v1/keys/"+id+"/rotate", "")
	if rotated.Code != http.StatusOK || rotated.Header().Get("ETag") != `"2"` {
		t.Fatalf("rotate: %d %s", rotated.Code, rotated.Body.String())
	}
	after := body(t, rotated)
	fresh := keyValue(t, rotated)
	if fresh == old {
		t.Error("the value did not change")
	}
	sa, sb := before["status"].(map[string]any), after["status"].(map[string]any)
	if sb["id"] != id || sb["owner"] != sa["owner"] || !reflect.DeepEqual(after["spec"], before["spec"]) || after["metadata"].(map[string]any)["name"] != "run-42" {
		t.Errorf("rotate changed the id, owner, spec, or name:\n%v\n%v", before, after)
	}
	if sb["prefix"] == sa["prefix"] || sb["prefix"] != fresh[:12] || sb["usage"] == nil || sb["state"] != "Active" {
		t.Errorf("status after rotate %v", sb)
	}
	if _, err := h.st.Keys().ByHash(bg(), serve.HashKeyValue(old)); err == nil {
		t.Error("the old hash still opens the Key")
	}
	if got, err := h.st.Keys().ByHash(bg(), serve.HashKeyValue(fresh)); err != nil || got != id {
		t.Errorf("the new hash: %q, %v", got, err)
	}
	rows, _ := h.st.Journal().Since(bg(), 0, 100)
	var found map[string]any
	for _, r := range rows {
		if r.Type == "key.rotated" {
			_ = json.Unmarshal(r.Payload, &found)
		}
	}
	if found == nil {
		t.Fatal("no key.rotated row")
	}
	data := found["data"].(map[string]any)
	if data["prefix"] != sb["prefix"] || data["previousPrefix"] != sa["prefix"] || found["reason"] != "request" || found["subject"] != h.subject() || found["request_id"] != rotated.Header().Get("Lux-Request-Id") {
		t.Errorf("key.rotated %v", found)
	}
	// A read after the rotate carries no value; a rotate by name works
	// too and a stale If-Match is conflict.
	if read := h.request(http.MethodGet, "/v1/keys/run-42", ""); strings.Contains(read.Body.String(), fresh) || status(t, read)["value"] != nil {
		t.Error("a read carries the value")
	}
	wantCode(t, h.request(http.MethodPost, "/v1/keys/run-42/rotate", "", "If-Match", `"1"`), CodeConflict)
	wantCode(t, h.request(http.MethodPost, "/v1/keys/run-42/rotate", "", "If-None-Match", "*"), CodeInvalidField)
	if rec := h.request(http.MethodPost, "/v1/keys/run-42/rotate", "", "If-Match", `"2"`); rec.Code != http.StatusOK {
		t.Errorf("rotate at the version: %d", rec.Code)
	}
	h.stub.Deny(stub.Rule{Action: "key.update", Subject: h.subject()}, "not_owner")
	h.advance(time.Minute * 11) // past the shared client's cached allow
	wantCode(t, h.request(http.MethodPost, "/v1/keys/run-42/rotate", ""), CodeForbidden)
}

// TestBudgetInUse: deleting a Budget a Key names is budget_in_use, 409;
// after the Key is deleted the Budget deletes; the Key's hash goes with
// it, and key.deleted carries the prefix.
func TestBudgetInUse(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	created := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	value := keyValue(t, created)
	if b := status(t, created)["budget"].(map[string]any); b["name"] != "team" || !strings.HasPrefix(b["id"].(string), "bud_") {
		t.Errorf("status.budget %v", b)
	}
	rec := h.request(http.MethodDelete, "/v1/budgets/team", "")
	if d := wantCode(t, rec, CodeBudgetInUse); !strings.Contains(d["detail"].(string), "1 live Key") {
		t.Errorf("detail %v", d["detail"])
	}
	if read := h.request(http.MethodGet, "/v1/budgets/team", ""); status(t, read)["keys"] != float64(1) {
		t.Errorf("status.keys %v", status(t, read)["keys"])
	}
	if rec := h.request(http.MethodDelete, "/v1/keys/run-42", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete the Key: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.st.Keys().ByHash(bg(), serve.HashKeyValue(value)); err == nil {
		t.Error("the hash outlived the Key")
	}
	if rec := h.request(http.MethodDelete, "/v1/budgets/team", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete the Budget: %d %s", rec.Code, rec.Body.String())
	}
	rows, _ := h.st.Journal().Since(bg(), 0, 100)
	types := map[string]map[string]any{}
	for _, r := range rows {
		var rec map[string]any
		_ = json.Unmarshal(r.Payload, &rec)
		types[r.Type] = rec
	}
	if d := types["key.deleted"]["data"].(map[string]any); d["prefix"] != value[:12] {
		t.Errorf("key.deleted %v", types["key.deleted"])
	}
	if _, ok := types["budget.deleted"]; !ok {
		t.Error("no budget.deleted row")
	}
}

// TestProviderInUse: deleting a Provider a declared Model targets is
// provider_in_use; once the Model is gone the delete takes the
// Provider's discovered Models and its credential row with it and tells
// the client source.
func TestProviderInUse(t *testing.T) {
	var revoked []string
	h := newHarness(t, func(o *Options) { o.Clients = revoker{&revoked} })
	h.seed()
	rec := h.request(http.MethodDelete, "/v1/providers/openai", "")
	if d := wantCode(t, rec, CodeProviderInUse); !strings.Contains(d["detail"].(string), "1 declared Model") {
		t.Errorf("detail %v", d["detail"])
	}
	pid := h.objectOf(v1.KindProvider, "openai").ID()
	discovered := &v1.Model{Metadata: v1.ObjectMeta{Name: "openai/o5"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: pid, Model: "o5"}}},
		Status: v1.ModelStatus{ID: h.newID(v1.PrefixModel), Owner: h.subject(), Source: v1.SourceDiscovered, Warnings: []string{}}}
	if _, err := h.st.Objects().Put(bg(), discovered, 0); err != nil {
		t.Fatal(err)
	}
	if rec := h.request(http.MethodDelete, "/v1/models/gpt-5", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete the Model: %d", rec.Code)
	}
	if rec := h.request(http.MethodDelete, "/v1/providers/openai", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete the Provider: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := h.st.Objects().Get(bg(), v1.KindModel, discovered.Status.ID); err == nil {
		t.Error("the discovered Model survived its Provider")
	}
	if _, err := h.st.Credentials().Get(bg(), pid); err == nil {
		t.Error("the credential row survived its Provider")
	}
	if !reflect.DeepEqual(revoked, []string{pid}) {
		t.Errorf("revoked %v", revoked)
	}
}

type revoker struct{ ids *[]string }

func (r revoker) Revoke(id string) { *r.ids = append(*r.ids, id) }

// TestSecretsNeverInResponses: a Key's value appears in the create and
// rotate responses and in no read, list, event, or the OpenAPI
// document; a Provider's credential appears in no response, event, or
// row of the journal.
func TestSecretsNeverInResponses(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	created := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	value := keyValue(t, created)
	rotated := h.request(http.MethodPost, "/v1/keys/run-42/rotate", "")
	fresh := keyValue(t, rotated)
	for _, path := range []string{"/v1/keys/run-42", "/v1/keys", "/v1/openapi.json", "/v1/providers/openai", "/v1/providers", "/v1/self", "/.well-known/lux"} {
		rec := h.request(http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		for _, secret := range []string{value, fresh, canary} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("GET %s carries a secret", path)
			}
		}
	}
	rows, _ := h.st.Journal().Since(bg(), 0, 100)
	for _, r := range rows {
		for _, secret := range []string{value, fresh, canary} {
			if strings.Contains(string(r.Payload), secret) {
				t.Errorf("event %s carries a secret", r.Type)
			}
		}
	}
	if strings.Contains(h.log.String(), value) || strings.Contains(h.log.String(), fresh) || strings.Contains(h.log.String(), canary) {
		t.Error("a log line carries a secret")
	}
	// The update response after a create carries no value either.
	updated := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	if updated.Code != http.StatusOK || status(t, updated)["value"] != nil {
		t.Errorf("update: %d value %v", updated.Code, status(t, updated)["value"])
	}
}

// TestSuppliedValueIsNotEchoed: a create with spec.value answers without
// spec.value and without status.value, with a sup_ prefix of the hash;
// the value opens the Key by its hash; a second create with the same
// value is invalid_field at spec.value naming no Key; an update carrying
// spec.value is immutable_field; a rotate mints a lux_ value and the
// supplied one stops working.
func TestSuppliedValueIsNotEchoed(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	supplied := "platform-token-" + canary
	bodyJSON := `{"spec": {"models": ["gpt-5"], "value": "` + supplied + `"}}`
	rec := h.request(http.MethodPut, "/v1/keys/run-42", bodyJSON)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	s := status(t, rec)
	want := "sup_" + serve.HashKeyValue(supplied)[:8]
	if s["prefix"] != want || s["value"] != nil || strings.Contains(rec.Body.String(), supplied) {
		t.Errorf("status %v, body %s", s, rec.Body.String())
	}
	id := s["id"].(string)
	if got, err := h.st.Keys().ByHash(bg(), serve.HashKeyValue(supplied)); err != nil || got != id {
		t.Errorf("the supplied value opens %q, %v", got, err)
	}
	rec = h.request(http.MethodPut, "/v1/keys/run-43", bodyJSON)
	d := wantCode(t, rec, CodeInvalidField)
	if !reflect.DeepEqual(paths(d), []string{"spec.value"}) || d["detail"] != "a Key with this value already exists" || strings.Contains(rec.Body.String(), id) {
		t.Errorf("a taken value: %v", d)
	}
	if _, _, err := h.st.Objects().ByName(bg(), v1.KindKey, "run-43"); err == nil {
		t.Error("the refused Key was stored")
	}
	rec = h.request(http.MethodPut, "/v1/keys/run-42", bodyJSON)
	if d := wantCode(t, rec, CodeImmutableField); !reflect.DeepEqual(paths(d), []string{"spec.value"}) {
		t.Errorf("an update with the value: %v", d)
	}
	rotated := h.request(http.MethodPost, "/v1/keys/run-42/rotate", "")
	fresh := keyValue(t, rotated)
	if _, err := h.st.Keys().ByHash(bg(), serve.HashKeyValue(supplied)); err == nil {
		t.Error("the supplied value still opens the Key after a rotate")
	}
	if got, _ := h.st.Keys().ByHash(bg(), serve.HashKeyValue(fresh)); got != id {
		t.Error("the minted value does not open the Key")
	}
}

// TestMaxKeysCeiling: max_keys from the authorizer caps the subject's
// live Keys at key.create, inside the transaction, and the create over
// it is ceiling_exceeded; another subject's Keys do not count; the
// authorizer's Key ceilings reach Resolve as ceiling_exceeded at the
// field.
func TestMaxKeysCeiling(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	h.stub.Allow(stub.Rule{Action: "key.create", Limits: map[string]any{"max_keys": 1, "max_key_requests_per_minute": 100}})
	if rec := h.request(http.MethodPut, "/v1/keys/run-1", keyJSON); rec.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	rec := h.request(http.MethodPut, "/v1/keys/run-2", keyJSON)
	if d := wantCode(t, rec, CodeCeilingExceeded); !strings.Contains(d["detail"].(string), "max_keys is 1") {
		t.Errorf("detail %v", d["detail"])
	}
	if rec := h.request(http.MethodPut, "/v1/keys/run-2", keyJSON, as(h.bob)...); rec.Code != http.StatusCreated {
		t.Errorf("another subject: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.request(http.MethodPut, "/v1/keys/run-1", keyJSON); rec.Code != http.StatusOK {
		t.Errorf("an update under the cap: %d", rec.Code)
	}
	rec = h.request(http.MethodPut, "/v1/keys/run-3", `{"spec": {"models": ["gpt-5"], "limits": {"requestsPerMinute": 500}}}`, as(h.bob)...)
	if d := wantCode(t, rec, CodeCeilingExceeded); !reflect.DeepEqual(paths(d), []string{"spec.limits.requestsPerMinute"}) {
		t.Errorf("a Key ceiling: %v", d)
	}
}

// TestDenyReasonStaysInDetail: a deny on the request's own action is
// forbidden with the authorizer's reason in details.detail and never in
// message; a deny reached through the Lookup is not_found at the field,
// its detail naming no object; an unavailable authorizer is
// authorizer_unavailable on the request and at the field.
func TestDenyReasonStaysInDetail(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	h.stub.Deny(stub.Rule{Action: "budget.create"}, "plan_exhausted: the team plan allows no budget")
	rec := h.request(http.MethodPut, "/v1/budgets/other", budgetJSON)
	d := wantCode(t, rec, CodeForbidden)
	detail, _ := d["detail"].(string)
	if !strings.Contains(detail, "plan_exhausted") || strings.Contains(body(t, rec)["error"].(map[string]any)["message"].(string), "plan") {
		t.Errorf("deny: %s", rec.Body.String())
	}
	h.stub.Deny(stub.Rule{Action: "budget.draw"}, "not_owner")
	rec = h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	d = wantCode(t, rec, CodeNotFound)
	if !reflect.DeepEqual(paths(d), []string{"spec.budget"}) || strings.Contains(d["detail"].(string), "bud_") {
		t.Errorf("a refused reference: %v", d)
	}
	h.stub.Deny(stub.Rule{Action: "model.use"}, "not_allowed")
	rec = h.request(http.MethodPut, "/v1/keys/run-42", `{"spec": {"models": ["gpt-5"]}}`)
	if d := wantCode(t, rec, CodeNotFound); !reflect.DeepEqual(paths(d), []string{"spec.models[0]"}) {
		t.Errorf("a refused selector: %v", d)
	}
	h.stub.Fail(http.StatusInternalServerError)
	rec = h.request(http.MethodPut, "/v1/budgets/third", budgetJSON)
	wantCode(t, rec, CodeAuthorizerUnavailable)
	rec = h.request(http.MethodGet, "/v1/budgets", "")
	wantCode(t, rec, CodeAuthorizerUnavailable)
	h.stub.Resume()
	h.stub.SetRules()
	h.stub.Hang()
	h.h.o.Authorizer = nil // not reached: the hang is the client's timeout
	h.stub.Resume()
}

// TestEventsAreJournalled: an apply raises <kind>.created with the row's
// data, an update <kind>.updated with the changed paths, each with
// reason request, the subject, and the request id, in the transaction
// that wrote the object.
func TestEventsAreJournalled(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	rec := h.request(http.MethodPut, "/v1/keys/run-42", `{"metadata": {"labels": {"run": "r_42"}}, "spec": {"models": ["gpt-5"], "budget": "team", "ttl": "24h"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	h.request(http.MethodPut, "/v1/keys/run-42", `{"metadata": {"labels": {"run": "r_43"}}, "spec": {"models": ["gpt-5", "openai/*"], "budget": "team", "ttl": "24h", "disabled": true}}`)
	h.request(http.MethodPut, "/v1/models/gpt-5", strings.Replace(modelJSON, `"input": "1"`, `"input": "3"`, 1))
	rows, _ := h.st.Journal().Since(bg(), 0, 100)
	byType := map[string]map[string]any{}
	for _, r := range rows {
		var rec map[string]any
		_ = json.Unmarshal(r.Payload, &rec)
		byType[r.Type] = rec
	}
	for _, typ := range []string{"provider.created", "model.created", "budget.created", "key.created", "key.updated", "model.updated"} {
		e, ok := byType[typ]
		if !ok {
			t.Errorf("no %s row", typ)
			continue
		}
		if e["reason"] != "request" || e["subject"] != h.subject() || !strings.HasPrefix(e["request_id"].(string), "req_") || e["object"].(map[string]any)["labels"] == nil {
			t.Errorf("%s envelope %v", typ, e)
		}
	}
	created := byType["key.created"]["data"].(map[string]any)
	if created["prefix"] == nil || !reflect.DeepEqual(created["models"], []any{"gpt-5"}) || created["budget"] != "team" || created["expiresAt"] == nil {
		t.Errorf("key.created data %v", created)
	}
	if p := byType["provider.created"]["data"].(map[string]any); p["dialect"] != "openai" || p["baseURL"] != "https://api.example.com/v1" {
		t.Errorf("provider.created data %v", p)
	}
	if m := byType["model.created"]["data"].(map[string]any); m["priced"] != true || len(m["targets"].([]any)) != 1 {
		t.Errorf("model.created data %v", m)
	}
	if b := byType["budget.created"]["data"].(map[string]any); b["amount"] != "50" || b["currency"] != "USD" || b["window"] != "month" || b["hard"] != true {
		t.Errorf("budget.created data %v", b)
	}
	if p := byType["key.updated"]["data"].(map[string]any)["paths"]; !reflect.DeepEqual(p, []any{"metadata.labels.run", "spec.disabled", "spec.models[1]"}) {
		t.Errorf("key.updated paths %v", p)
	}
	// Resolve derives the two cache prices from input, so raising input
	// changes three paths.
	if p := byType["model.updated"]["data"].(map[string]any)["paths"]; !reflect.DeepEqual(p, []any{"spec.pricing.cacheWrite", "spec.pricing.cachedInput", "spec.pricing.input"}) {
		t.Errorf("model.updated paths %v", p)
	}
}
