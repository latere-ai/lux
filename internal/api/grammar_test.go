// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// fixtures is one manifest per kind, and the plural of its route.
var fixtures = []struct{ plural, kind, name, body string }{
	{"providers", v1.KindProvider, "openai", providerJSON},
	{"models", v1.KindModel, "gpt-5", modelJSON},
	{"budgets", v1.KindBudget, "team", budgetJSON},
	{"keys", v1.KindKey, "run-42", keyJSON},
}

// actionsAsked lists the actions the stub was asked, in order.
func (h *harness) actionsAsked() []string {
	var out []string
	for _, r := range h.stub.Requests() {
		out = append(out, r.Action)
	}
	return out
}

// TestGrammarPerKind is the four routes of each kind, table-driven: PUT
// creates then updates, GET reads by name and by id, GET lists, DELETE
// answers 204, and every route asks exactly the action of its row on
// the authorizer, with the request's own id, address, and user agent.
func TestGrammarPerKind(t *testing.T) {
	h := newHarness(t, nil)
	for _, f := range fixtures {
		t.Run(f.kind, func(t *testing.T) {
			h.stub.ClearRequests()
			rec := h.request(http.MethodPut, "/v1/"+f.plural+"/"+f.name, f.body, "User-Agent", "lux-test/1")
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			s := status(t, rec)
			id, _ := s["id"].(string)
			prefix := kindOf(f.kind).prefix
			if !strings.HasPrefix(id, prefix) || s["owner"] != h.subject() || s["version"] != float64(1) || rec.Header().Get("ETag") != `"1"` {
				t.Errorf("status after create: %v, ETag %q", s, rec.Header().Get("ETag"))
			}
			k := kindOf(f.kind)
			asked := h.actionsAsked()
			if asked[0] != k.create {
				t.Errorf("create asked %v, want %s first", asked, k.create)
			}
			req := h.stub.Requests()[0]
			if req.Request.ID != rec.Header().Get("Lux-Request-Id") || req.Request.IP != "203.0.113.9" || req.Request.UserAgent != "lux-test/1" {
				t.Errorf("request block %+v", req.Request)
			}

			h.stub.ClearRequests()
			rec = h.request(http.MethodPut, "/v1/"+f.plural+"/"+f.name, f.body)
			if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"2"` {
				t.Fatalf("update: %d %s ETag %q", rec.Code, rec.Body.String(), rec.Header().Get("ETag"))
			}
			if asked := h.actionsAsked(); asked[0] != k.update {
				t.Errorf("update asked %v, want %s first", asked, k.update)
			}

			// The read by name asks the action; the read by id is the
			// same subject, action, and resource id, which the shared
			// client answers from its cache (spec 006).
			for i, ref := range []string{f.name, id} {
				h.stub.ClearRequests()
				rec = h.request(http.MethodGet, "/v1/"+f.plural+"/"+ref, "")
				if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"2"` {
					t.Fatalf("read %s: %d %s", ref, rec.Code, rec.Body.String())
				}
				want := []string{k.read}
				if i == 1 {
					want = nil
				}
				if asked := h.actionsAsked(); !reflect.DeepEqual(asked, want) {
					t.Errorf("read %s asked %v, want %v", ref, asked, want)
				}
			}

			h.stub.ClearRequests()
			rec = h.request(http.MethodGet, "/v1/"+f.plural, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			if items, _ := body(t, rec)["items"].([]any); len(items) != 1 {
				t.Errorf("list has %d items, want 1: %s", len(items), rec.Body.String())
			}
			if asked := h.actionsAsked(); !reflect.DeepEqual(asked, []string{k.list}) {
				t.Errorf("list asked %v, want [%s]", asked, k.list)
			}
		})
	}
	// Deletes in dependency order: the Key before the Budget, the Model
	// before the Provider.
	for _, f := range []struct{ plural, kind, name string }{{"keys", v1.KindKey, "run-42"}, {"budgets", v1.KindBudget, "team"}, {"models", v1.KindModel, "gpt-5"}, {"providers", v1.KindProvider, "openai"}} {
		h.stub.ClearRequests()
		rec := h.request(http.MethodDelete, "/v1/"+f.plural+"/"+f.name, "")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("delete %s: %d %s", f.name, rec.Code, rec.Body.String())
		}
		if asked := h.actionsAsked(); !reflect.DeepEqual(asked, []string{kindOf(f.kind).del}) {
			t.Errorf("delete asked %v", asked)
		}
		if rec := h.request(http.MethodGet, "/v1/"+f.plural+"/"+f.name, ""); rec.Code != http.StatusNotFound {
			t.Errorf("after delete: %d", rec.Code)
		}
	}
}

// TestRouteTableActions: every route requires a bearer but
// /v1/openapi.json and /.well-known/lux, which serve without one; a
// path outside the table is not_found; the two usage routes ask
// usage.read and the rotate asks key.update.
func TestRouteTableActions(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, r := range []struct{ method, path string }{
		{"PUT", "/v1/providers/x"}, {"GET", "/v1/providers"}, {"GET", "/v1/providers/x"}, {"DELETE", "/v1/providers/x"},
		{"PUT", "/v1/models/x"}, {"GET", "/v1/models"}, {"GET", "/v1/models/x"}, {"DELETE", "/v1/models/x"},
		{"PUT", "/v1/keys/x"}, {"GET", "/v1/keys"}, {"GET", "/v1/keys/x"}, {"DELETE", "/v1/keys/x"}, {"POST", "/v1/keys/x/rotate"},
		{"PUT", "/v1/budgets/x"}, {"GET", "/v1/budgets"}, {"GET", "/v1/budgets/x"}, {"DELETE", "/v1/budgets/x"},
		{"GET", "/v1/self"}, {"GET", "/v1/usage"}, {"GET", "/v1/requests"},
	} {
		rec := h.request(r.method, r.path, "", "Authorization", "")
		wantCode(t, rec, CodeUnauthenticated)
		rec = h.request(r.method, r.path, "", "Authorization", "Bearer not-a-token")
		if d := wantCode(t, rec, CodeUnauthenticated); !strings.Contains(d["detail"].(string), "not a JWS") {
			t.Errorf("%s %s: detail %v", r.method, r.path, d["detail"])
		}
	}
	for _, path := range []string{"/v1/openapi.json", "/.well-known/lux"} {
		if rec := h.request(http.MethodGet, path, "", "Authorization", ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s without a bearer: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/v1/providers/openai/tunnel", "/v1/providers/openai/tunnel/carry", "/v1/nothing", "/v1/keys/run-42/other"} {
		if rec := h.request(http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want not_found while unmounted", path, rec.Code)
		}
	}
	for _, path := range []string{"/v1/usage", "/v1/requests"} {
		h.stub.ClearRequests()
		if rec := h.request(http.MethodGet, path, ""); rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if asked := h.actionsAsked(); !reflect.DeepEqual(asked, []string{"usage.read"}) {
			t.Errorf("GET %s asked %v", path, asked)
		}
	}
	h.stub.ClearRequests()
	if rec := h.request(http.MethodPost, "/v1/keys/run-42/rotate", ""); rec.Code != http.StatusNotFound {
		t.Errorf("rotate of a missing Key: %d", rec.Code)
	}
	rec := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	h.stub.ClearRequests()
	if rec := h.request(http.MethodPost, "/v1/keys/run-42/rotate", ""); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	if asked := h.actionsAsked(); !reflect.DeepEqual(asked, []string{"key.update"}) {
		t.Errorf("rotate asked %v", asked)
	}
}

// TestNoPatch: PATCH and OPTIONS on every route are not_found in the
// envelope, never the mux's own 405.
func TestNoPatch(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, path := range []string{"/v1/providers", "/v1/providers/openai", "/v1/models", "/v1/models/gpt-5", "/v1/keys", "/v1/keys/run-42", "/v1/keys/run-42/rotate", "/v1/budgets", "/v1/budgets/team", "/v1/usage", "/v1/usage/redact", "/v1/requests", "/v1/self", "/v1/openapi.json", "/.well-known/lux"} {
		for _, method := range []string{"PATCH", "OPTIONS"} {
			rec := h.request(method, path, `{"spec": {}}`)
			if d := wantCode(t, rec, CodeNotFound); !strings.Contains(d["detail"].(string), method+" "+path) {
				t.Errorf("%s %s: detail %v", method, path, d["detail"])
			}
			if rec.Header().Get("Allow") != "" {
				t.Errorf("%s %s carries Allow", method, path)
			}
		}
	}
}

// TestApplyIsCreateThenUpdate: PUT twice with one manifest is 201 then
// 200, and the second response equals the first but for status.version
// and updatedAt; the Provider's credential is sealed once and its
// status counts one value.
func TestApplyIsCreateThenUpdate(t *testing.T) {
	h := newHarness(t, nil)
	first := h.request(http.MethodPut, "/v1/providers/openai", providerJSON)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	h.advance(time.Minute)
	second := h.request(http.MethodPut, "/v1/providers/openai", strings.Replace(providerJSON, `"credential": {"value": "`+canary+`"}, `, "", 1))
	if second.Code != http.StatusOK {
		t.Fatalf("second: %d %s", second.Code, second.Body.String())
	}
	a, b := body(t, first), body(t, second)
	sa, sb := a["status"].(map[string]any), b["status"].(map[string]any)
	if sa["version"] != float64(1) || sb["version"] != float64(2) || sa["updatedAt"] == sb["updatedAt"] || sa["createdAt"] != sb["createdAt"] {
		t.Errorf("versions and timestamps: %v / %v", sa, sb)
	}
	for _, s := range []map[string]any{sa, sb} {
		delete(s, "version")
		delete(s, "updatedAt")
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the second response differs beyond version and updatedAt:\n%v\n%v", a, b)
	}
	if cred, _ := sb["credential"].(map[string]any); cred["set"] != true || cred["version"] != float64(1) {
		t.Errorf("credential status %v", cred)
	}
	if strings.Contains(first.Body.String(), canary) || strings.Contains(second.Body.String(), canary) {
		t.Error("a response carries the credential")
	}
	third := h.request(http.MethodPut, "/v1/providers/openai", strings.Replace(providerJSON, canary, canary+"-2", 1))
	if cred, _ := status(t, third)["credential"].(map[string]any); cred["version"] != float64(2) {
		t.Errorf("a second value did not count: %v", cred)
	}
	// A body without a value, with or without the credential block,
	// keeps the stored credential: only a delete removes it.
	for _, b := range []string{
		strings.Replace(providerJSON, `"credential": {"value": "`+canary+`"}, `, "", 1),
		strings.Replace(providerJSON, `"credential": {"value": "`+canary+`"}`, `"credential": {"header": "Authorization"}`, 1),
	} {
		rec := h.request(http.MethodPut, "/v1/providers/openai", b)
		if cred, _ := status(t, rec)["credential"].(map[string]any); rec.Code != http.StatusOK || cred["set"] != true || cred["version"] != float64(2) {
			t.Errorf("a body without a value: %d %v", rec.Code, cred)
		}
	}
	if _, err := h.st.Credentials().Get(bg(), status(t, third)["id"].(string)); err != nil {
		t.Errorf("the credential row is gone: %v", err)
	}
	// A Provider that never had a value reads set false.
	rec := h.request(http.MethodPut, "/v1/providers/bare", strings.Replace(providerJSON, `"credential": {"value": "`+canary+`"}, `, "", 1))
	if cred, _ := status(t, rec)["credential"].(map[string]any); rec.Code != http.StatusCreated || cred["set"] != false {
		t.Errorf("a Provider without a value: %d %v", rec.Code, cred)
	}
}

// TestPreconditions: a stale If-Match is conflict; If-None-Match: * on a
// taken name is already_exists; If-Match: * on a free name is not_found;
// a weak validator, a list, an unquoted number, If-None-Match with a
// version, or both headers is invalid_field at the header; DELETE holds
// If-Match the same way.
func TestPreconditions(t *testing.T) {
	h := newHarness(t, nil)
	if rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", "*"); wantCode(t, rec, CodeNotFound) == nil {
		t.Fatal()
	}
	if rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", `"3"`); wantCode(t, rec, CodeNotFound) == nil {
		t.Fatal()
	}
	rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-None-Match", "*")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create-once: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-None-Match", "*")
	wantCode(t, rec, CodeAlreadyExists)
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", `"7"`)
	if d := wantCode(t, rec, CodeConflict); !strings.Contains(d["detail"].(string), "version 1") {
		t.Errorf("conflict detail %v", d["detail"])
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", `"1"`)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("exact version: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", "*")
	if rec.Code != http.StatusOK {
		t.Fatalf("must exist: %d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct{ header, value, path string }{
		{"If-Match", `W/"7"`, "If-Match"}, {"If-Match", `"1", "2"`, "If-Match"}, {"If-Match", `7`, "If-Match"},
		{"If-Match", `"0"`, "If-Match"}, {"If-Match", `""`, "If-Match"}, {"If-None-Match", `"7"`, "If-None-Match"},
	} {
		rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, tc.header, tc.value)
		if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{tc.path}) {
			t.Errorf("%s %s: paths %v", tc.header, tc.value, paths(d))
		}
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", `"3"`, "If-None-Match", "*")
	if d := wantCode(t, rec, CodeInvalidField); len(paths(d)) != 2 {
		t.Errorf("both headers: paths %v", paths(d))
	}
	r := httptest.NewRequest(http.MethodPut, "/v1/budgets/team", strings.NewReader(budgetJSON))
	r.Header.Set("Authorization", "Bearer "+h.alice)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("If-Match", `"3"`)
	r.Header.Add("If-Match", `"4"`)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	wantCode(t, w, CodeInvalidField)

	rec = h.request(http.MethodDelete, "/v1/budgets/team", "", "If-Match", `"1"`)
	wantCode(t, rec, CodeConflict)
	rec = h.request(http.MethodDelete, "/v1/budgets/team", "", "If-None-Match", "*")
	wantCode(t, rec, CodeInvalidField)
	rec = h.request(http.MethodDelete, "/v1/budgets/team", "", "If-Match", `"3"`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete at the version: %d %s", rec.Code, rec.Body.String())
	}
	// A name deleted and created again starts at 1 again.
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON)
	if rec.Code != http.StatusCreated || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("re-create: %d ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
}

// conflicting is a store whose first n transactions are a version
// conflict, the write another caller landed between the read and the
// write, so the retry is exercised deterministically.
type conflicting struct {
	store.Store
	mu sync.Mutex
	n  int
}

func (s *conflicting) Transact(ctx context.Context, fn func(tx store.Store) error) error {
	s.mu.Lock()
	if s.n > 0 {
		s.n--
		s.mu.Unlock()
		return fmt.Errorf("%w: another write landed", store.ErrVersionConflict)
	}
	s.mu.Unlock()
	return s.Store.Transact(ctx, fn)
}

// TestConcurrentApplyRetries: without a precondition an apply whose
// write met a version conflict is retried, up to three attempts, and
// the caller sees 200; a fourth conflict is conflict; with If-Match the
// first conflict is answered, because the caller asked for that version
// and no other; two applies racing in fact both land.
func TestConcurrentApplyRetries(t *testing.T) {
	var cs *conflicting
	h := newHarness(t, func(o *Options) {
		cs = &conflicting{Store: o.Store}
		o.Store = cs
	})
	if rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	cs.n = applyRetries - 1
	if rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON); rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("retried apply: %d %s", rec.Code, rec.Body.String())
	}
	cs.n = applyRetries
	rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON)
	if d := wantCode(t, rec, CodeConflict); !strings.Contains(d["detail"].(string), "another write landed") {
		t.Errorf("detail %v", d["detail"])
	}
	cs.n = 1
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "If-Match", `"2"`)
	wantCode(t, rec, CodeConflict)
	cs.n = 0
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Go(func() {
			codes[i] = h.request(http.MethodPut, "/v1/budgets/team", strings.Replace(budgetJSON, `"50"`, `"`+intToString(60+i)+`"`, 1)).Code
		})
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Errorf("racing applies: %v", codes)
	}
	if rec := h.request(http.MethodGet, "/v1/budgets/team", ""); rec.Header().Get("ETag") != `"4"` {
		t.Errorf("final ETag %q, want \"4\"", rec.Header().Get("ETag"))
	}
}

// TestApplyIsByNameOnly and TestAddressByIdOrName: a PUT whose path
// carries an id prefix is invalid_field at metadata.name; a GET with
// another kind's id is not_found; a Key read by id and by name are one
// response; the probe id, which has no kind prefix, is a name that
// names nothing.
func TestApplyIsByNameOnly(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodPut, "/v1/keys/key_01J9ZK2P7Q8R9S0T1U2V3W4X60", keyJSON)
	if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{"metadata.name"}) {
		t.Errorf("paths %v", paths(d))
	}
	for _, prefix := range []string{"prv_", "mdl_", "bud_"} {
		rec := h.request(http.MethodPut, "/v1/keys/"+prefix+"01J9ZK2P7Q8R9S0T1U2V3W4X60", keyJSON)
		wantCode(t, rec, CodeInvalidField)
	}
}

func TestAddressByIdOrName(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	rec := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	id := status(t, rec)["id"].(string)
	byName := h.request(http.MethodGet, "/v1/keys/run-42", "")
	byID := h.request(http.MethodGet, "/v1/keys/"+id, "")
	if byName.Code != 200 || byID.Code != 200 || byName.Body.String() != byID.Body.String() {
		t.Errorf("by name and by id differ:\n%s\n%s", byName.Body.String(), byID.Body.String())
	}
	rec = h.request(http.MethodGet, "/v1/keys/prv_01J9ZK2P7Q8R9S0T1U2V3W4X60", "")
	if d := wantCode(t, rec, CodeNotFound); !strings.Contains(d["detail"].(string), "another kind") {
		t.Errorf("detail %v", d["detail"])
	}
	rec = h.request(http.MethodGet, "/v1/keys/key_01J9ZK2P7Q8R9S0T1U2V3W4X60", "")
	wantCode(t, rec, CodeNotFound)
	rec = h.request(http.MethodGet, "/v1/keys/"+authz.ProbeID, "")
	wantCode(t, rec, CodeNotFound)
	rec = h.request(http.MethodDelete, "/v1/keys/"+authz.ProbeID, "")
	wantCode(t, rec, CodeNotFound)
}

// TestModelNamesWithSlashes: a Model named openai/gpt-5 is applied,
// read, listed, and deleted by its percent-encoded name, by its
// two-segment path, and by its id, all naming one object; a
// three-segment name is one object too; a rejoined name that fails the
// name rule is not_found.
func TestModelNamesWithSlashes(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	rec := h.request(http.MethodPut, "/v1/models/openai%2Fgpt-5", modelJSON)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := status(t, rec)["id"].(string)
	if body(t, rec)["metadata"].(map[string]any)["name"] != "openai/gpt-5" {
		t.Errorf("name %v", body(t, rec)["metadata"])
	}
	rec = h.request(http.MethodPut, "/v1/models/openai/gpt-5", modelJSON)
	if rec.Code != http.StatusOK || status(t, rec)["id"] != id {
		t.Fatalf("two-segment apply: %d %s", rec.Code, rec.Body.String())
	}
	for _, ref := range []string{"openai%2Fgpt-5", "openai/gpt-5", id} {
		rec := h.request(http.MethodGet, "/v1/models/"+ref, "")
		if rec.Code != http.StatusOK || status(t, rec)["id"] != id {
			t.Errorf("read %s: %d %s", ref, rec.Code, rec.Body.String())
		}
	}
	rec = h.request(http.MethodPut, "/v1/models/relay/anthropic/claude-sonnet-4", modelJSON)
	if rec.Code != http.StatusCreated || body(t, rec)["metadata"].(map[string]any)["name"] != "relay/anthropic/claude-sonnet-4" {
		t.Fatalf("three segments: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodGet, "/v1/models", "")
	items, _ := body(t, rec)["items"].([]any)
	if len(items) != 3 {
		t.Errorf("list has %d Models, want 3", len(items))
	}
	for _, bad := range []string{"/v1/models/", "/v1/models/Open/AI", "/v1/models/a//b", "/v1/models/-x"} {
		rec := h.request(http.MethodGet, bad, "")
		wantCode(t, rec, CodeNotFound)
	}
	if rec := h.request(http.MethodDelete, "/v1/models/openai%2Fgpt-5", ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete by encoded name: %d", rec.Code)
	}
	if rec := h.request(http.MethodGet, "/v1/models/openai/gpt-5", ""); rec.Code != http.StatusNotFound {
		t.Errorf("after delete: %d", rec.Code)
	}
	if rec := h.request(http.MethodDelete, "/v1/models/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete by id after delete: %d", rec.Code)
	}
}

// TestNameOnApply: a body whose metadata.name differs from the path is
// invalid_field at metadata.name; a body without one takes the path's.
func TestNameOnApply(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodPut, "/v1/budgets/team", `{"metadata": {"name": "other"}, `+budgetJSON[1:])
	if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{"metadata.name"}) {
		t.Errorf("paths %v", paths(d))
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON)
	if rec.Code != http.StatusCreated || body(t, rec)["metadata"].(map[string]any)["name"] != "team" {
		t.Fatalf("no name: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", `{"metadata": {"name": "team"}, `+budgetJSON[1:])
	if rec.Code != http.StatusOK {
		t.Fatalf("the same name: %d %s", rec.Code, rec.Body.String())
	}
}

// TestApplyWithoutTheEnvelope: a body of spec alone creates the Key and
// answers the full envelope; the same body with another kind is
// unsupported_kind; another apiVersion is unsupported_version; the full
// envelope is accepted too.
func TestApplyWithoutTheEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	rec := h.request(http.MethodPut, "/v1/keys/run-42", `{"spec": {"models": ["gpt-5"]}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	b := body(t, rec)
	if b["apiVersion"] != v1.APIVersion || b["kind"] != "Key" || b["metadata"].(map[string]any)["name"] != "run-42" {
		t.Errorf("envelope %v", b)
	}
	rec = h.request(http.MethodPut, "/v1/keys/run-43", `{"kind": "Model", "spec": {"models": ["gpt-5"]}}`)
	if d := wantCode(t, rec, CodeUnsupportedKind); !reflect.DeepEqual(paths(d), []string{"kind"}) {
		t.Errorf("paths %v", paths(d))
	}
	rec = h.request(http.MethodPut, "/v1/keys/run-43", `{"apiVersion": "lux.latere.ai/v2", "spec": {"models": ["gpt-5"]}}`)
	wantCode(t, rec, CodeUnsupportedVersion)
	rec = h.request(http.MethodPut, "/v1/keys/run-43", `{"apiVersion": "`+v1.APIVersion+`", "kind": "Key", "metadata": {"name": "run-43"}, "spec": {"models": ["gpt-5"]}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("full envelope: %d %s", rec.Code, rec.Body.String())
	}
	// The body a GET returned re-applies unchanged: status is ignored.
	got := h.request(http.MethodGet, "/v1/keys/run-43", "")
	rec = h.request(http.MethodPut, "/v1/keys/run-43", got.Body.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("re-apply of a read: %d %s", rec.Code, rec.Body.String())
	}
	var before, after map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &before)
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if !reflect.DeepEqual(before["spec"], after["spec"]) {
		t.Errorf("spec changed on re-apply:\n%v\n%v", before["spec"], after["spec"])
	}
}
