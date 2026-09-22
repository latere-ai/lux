// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store/filemode"
	"latere.ai/x/lux/manifest"
)

// TestWellKnown: GET /.well-known/lux needs no bearer and names the
// build, the API, the four doors built from LUX_PUBLIC_URL, the
// dialects, the issuers, the audience, and the mode.
func TestWellKnown(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodGet, "/.well-known/lux", "", "Authorization", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	doc := body(t, rec)
	want := map[string]any{
		"name": "lux", "version": "0.1.0-test", "apiVersion": "lux.latere.ai/v1beta1",
		"api": publicURL + "/v1", "openapi": publicURL + "/v1/openapi.json",
		"doors":    map[string]any{"openai": publicURL + "/openai", "anthropic": publicURL + "/anthropic", "gemini": publicURL + "/gemini", "lux": publicURL + "/lux"},
		"dialects": []any{"openai", "anthropic", "gemini", "lux"},
		"issuers":  []any{h.iss.URL()}, "audience": "lux", "mode": "server",
	}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("document\n got %v\nwant %v", doc, want)
	}
	if h.stub.Requests() != nil {
		t.Error("the well-known document asked the authorizer")
	}
}

// TestWellKnownReportsThePrimaryAudience is spec 034: with an audience
// list the document's one audience member is the primary, the name a
// client asks a token for, and GET /v1/self reports the caller and no
// audience.
func TestWellKnownReportsThePrimaryAudience(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		a, err := auth.New(t.Context(), auth.Options{Issuers: o.Auth.Verifier.Issuers(), Audiences: []string{audience, "api.example.com"}, HTTP: &http.Client{}})
		if err != nil {
			t.Fatal(err)
		}
		o.Auth, o.Authorizer = a, a.Authorizer(&serve.ObjectOwners{Objects: o.Store.Objects()})
	})
	doc := body(t, h.request(http.MethodGet, "/.well-known/lux", "", "Authorization", ""))
	if doc["audience"] != audience {
		t.Fatalf("audience %v, want the primary %q", doc["audience"], audience)
	}
	self := body(t, h.request(http.MethodGet, "/v1/self", ""))
	if self["subject"] != h.subject() {
		t.Fatalf("self %v", self)
	}
	if _, has := self["audience"]; has {
		t.Fatalf("/v1/self reports an audience: %v", self)
	}
}

// TestSelf: GET /v1/self returns the caller's subject, issuer, sub, and
// claims, the policy, and, once a decision has been made for the
// subject on this replica, the cached limits and filter; it asks the
// authorizer nothing.
func TestSelf(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodGet, "/v1/self", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	doc := body(t, rec)
	if doc["subject"] != h.subject() || doc["issuer"] != h.iss.URL() || doc["sub"] != "alice" || doc["policy"] != "authorizer" || doc["limits"] != nil || doc["filter"] != nil {
		t.Errorf("self before a decision %v", doc)
	}
	if claims, _ := doc["claims"].(map[string]any); claims["email"] != "alice@example.com" || claims["iss"] != h.iss.URL() {
		t.Errorf("claims %v", claims)
	}
	if h.stub.Requests() != nil {
		t.Error("/v1/self asked the authorizer")
	}
	h.stub.Allow(stub.Rule{Action: "budget.list", Limits: map[string]any{"requests_per_minute": 1200, "max_key_spend": "50", "max_key_ttl": "168h", "max_keys": 10}, Filter: &authz.Filter{Owners: []string{h.subject()}}})
	h.request(http.MethodGet, "/v1/budgets", "")
	doc = body(t, h.request(http.MethodGet, "/v1/self", ""))
	if !reflect.DeepEqual(doc["limits"], map[string]any{"requests_per_minute": float64(1200), "max_key_spend": "50", "max_key_ttl": "168h", "max_keys": float64(10)}) {
		t.Errorf("limits %v", doc["limits"])
	}
	if !reflect.DeepEqual(doc["filter"], map[string]any{"owners": []any{h.subject()}}) {
		t.Errorf("filter %v", doc["filter"])
	}
	// The memo lapses with the allow's ttl.
	h.advance(2 * time.Minute)
	if doc := body(t, h.request(http.MethodGet, "/v1/self", "")); doc["limits"] != nil {
		t.Errorf("limits survived the ttl: %v", doc)
	}
	// Under the owner policy the policy is owner.
	owner := newOwnerHarness(t)
	if doc := body(t, owner.request(http.MethodGet, "/v1/self", "")); doc["policy"] != "owner" {
		t.Errorf("owner policy %v", doc)
	}
}

// newOwnerHarness is a surface under the owner policy, alice its admin.
func newOwnerHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil)
	a, err := auth.New(t.Context(), auth.Options{Issuers: []string{h.iss.URL()}, Audiences: []string{audience}, AdminSubjects: []string{h.subject()}, HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.auth = a
	o := h.h.o
	o.Auth, o.Authorizer = a, a.Authorizer(&serve.ObjectOwners{Objects: h.st.Objects()})
	h.h = New(o)
	return h
}

// TestNoCORS: no response on the plane carries an Access-Control-*
// header, a preflight OPTIONS is not_found, and an Origin header changes
// nothing.
func TestNoCORS(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, tc := range []struct{ method, path string }{
		{"OPTIONS", "/v1/keys"}, {"OPTIONS", "/v1/self"}, {"OPTIONS", "/.well-known/lux"}, {"GET", "/v1/budgets/team"}, {"GET", "/.well-known/lux"}, {"PUT", "/v1/budgets/team"},
	} {
		body := ""
		if tc.method == "PUT" {
			body = budgetJSON
		}
		rec := h.request(tc.method, tc.path, body, "Origin", "https://console.example.com", "Access-Control-Request-Method", "PUT")
		for name := range rec.Header() {
			if strings.HasPrefix(name, "Access-Control-") {
				t.Errorf("%s %s: %s", tc.method, tc.path, name)
			}
		}
		if tc.method == "OPTIONS" {
			wantCode(t, rec, CodeNotFound)
		}
	}
}

// fileModeHarness is the surface over a directory of manifests: no
// bearer, nothing authorized, every write read_only.
func fileModeHarness(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{
		"provider.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: openai\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n  credential:\n    valueFrom:\n      env: OPENAI_KEY\n  discovery:\n    mode: none\n  health:\n    mode: none\n",
		"model.yaml":    "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: gpt-5\nspec:\n  targets:\n    - provider: openai\n",
		"budget.yaml":   "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: team\nspec:\n  amount: \"50\"\n",
		"key.yaml":      "apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: run-42\nspec:\n  models: [gpt-5]\n  budget: team\n  valueFrom:\n    env: RUN_KEY\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	getenv := func(k string) string {
		return map[string]string{"OPENAI_KEY": canary, "RUN_KEY": "lux_" + strings.Repeat("A", 40)}[k]
	}
	files, err := filemode.Load(t.Context(), filemode.Options{Dir: dir, Getenv: getenv, Defaults: manifest.Defaults{Timeout: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(t.Context(), auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(publicURL)
	return New(Options{Store: files, Auth: a, PublicURL: base, Version: "0.1.0-test", ReadOnlyDir: dir}), dir
}

// TestFileModeIsReadOnly: in the file mode every PUT, POST, and DELETE
// under /v1/{kind}s, the tunnel routes included, is read_only, 405,
// with Allow: GET and the directory in the detail, before the body is
// read; PATCH is not_found; the read routes serve without a bearer;
// GET /v1/self answers policy file with no subject; /.well-known/lux
// says mode file with no issuers; a Key's value from the environment
// appears in no response.
func TestFileModeIsReadOnly(t *testing.T) {
	h, dir := fileModeHarness(t)
	send := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	for _, tc := range []struct{ method, path string }{
		{"PUT", "/v1/providers/openai"}, {"DELETE", "/v1/providers/openai"}, {"POST", "/v1/providers/openai/tunnel"}, {"POST", "/v1/providers/openai/tunnel/carry"},
		{"PUT", "/v1/models/gpt-5"}, {"DELETE", "/v1/models/openai/gpt-5"}, {"PUT", "/v1/keys/run-42"}, {"POST", "/v1/keys/run-42/rotate"}, {"DELETE", "/v1/keys/run-42"},
		{"PUT", "/v1/budgets/team"}, {"DELETE", "/v1/budgets/team"}, {"POST", "/v1/budgets"},
	} {
		body := &countingReader{Reader: strings.NewReader(strings.Repeat("{", 1<<20))}
		rec := send(tc.method, tc.path, body)
		d := wantCode(t, rec, CodeReadOnly)
		if rec.Header().Get("Allow") != "GET" || !strings.Contains(d["detail"].(string), dir) {
			t.Errorf("%s %s: Allow %q detail %v", tc.method, tc.path, rec.Header().Get("Allow"), d["detail"])
		}
		if body.n > 0 {
			t.Errorf("%s %s: %d bytes of the body were read", tc.method, tc.path, body.n)
		}
	}
	for _, path := range []string{"/v1/keys/run-42", "/v1/budgets/team", "/v1/self"} {
		rec := send("PATCH", path, nil)
		wantCode(t, rec, CodeNotFound)
	}
	for _, path := range []string{"/v1/providers", "/v1/providers/openai", "/v1/models", "/v1/models/gpt-5", "/v1/keys", "/v1/keys/run-42", "/v1/budgets", "/v1/budgets/team", "/v1/usage", "/v1/requests", "/v1/openapi.json"} {
		rec := send("GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), canary) || strings.Contains(rec.Body.String(), strings.Repeat("A", 40)) {
			t.Errorf("GET %s carries a secret", path)
		}
	}
	rec := send("GET", "/v1/keys/run-42", nil)
	if s := status(t, rec); s["state"] != "Active" || s["prefix"] != "lux_AAAAAAAA" || s["usage"] == nil {
		t.Errorf("a file-mode Key renders %v", s)
	}
	rec = send("GET", "/v1/self", nil)
	if doc := body(t, rec); rec.Code != http.StatusOK || !reflect.DeepEqual(doc, map[string]any{"policy": "file"}) {
		t.Errorf("self %d %v", rec.Code, doc)
	}
	rec = send("GET", "/.well-known/lux", nil)
	if doc := body(t, rec); doc["mode"] != "file" || !reflect.DeepEqual(doc["issuers"], []any{}) || doc["audience"] != "" {
		t.Errorf("well-known %v", doc)
	}
	// The public listener's /v1 in this mode: not_found everywhere, with
	// a request id.
	un := h.Unmounted()
	for _, tc := range []struct{ method, path string }{{"GET", "/v1/keys"}, {"PUT", "/v1/keys/run-42"}, {"GET", "/v1/self"}} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-Request-Id", "trace-1")
		rec := httptest.NewRecorder()
		un.ServeHTTP(rec, r)
		wantCode(t, rec, CodeNotFound)
		if rec.Header().Get("X-Request-Id") != "trace-1" {
			t.Errorf("%s %s: no echo", tc.method, tc.path)
		}
	}
}

// countingReader counts the bytes read from it.
type countingReader struct {
	io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += n
	return n, err
}

// TestNewRefusesAMissingSeam: a nil Store, Auth, or PublicURL, and a nil
// Authorizer outside the file mode, is a panic naming the option.
func TestNewRefusesAMissingSeam(t *testing.T) {
	h := newHarness(t, nil)
	base, _ := url.Parse(publicURL)
	for name, o := range map[string]Options{
		"Store":      {Auth: h.auth, PublicURL: base},
		"Auth":       {Store: h.st, PublicURL: base},
		"PublicURL":  {Store: h.st, Auth: h.auth},
		"Authorizer": {Store: h.st, Auth: h.auth, PublicURL: base},
	} {
		func() {
			defer func() {
				if p := recover(); p == nil || !strings.Contains(p.(string), name) {
					t.Errorf("New without %s: %v", name, p)
				}
			}()
			New(o)
		}()
	}
}
