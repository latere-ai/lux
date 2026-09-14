// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
)

// do sends one request to a running serve and returns the response with
// its body read.
func do(t *testing.T, method, url, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

// errorCode reads the code of an error envelope.
func errorCode(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, body)
	}
	return env.Error.Code
}

// TestServeMountsTheDoorsAndTheControlPlane is spec 011's mounting in
// server mode, through run: /.well-known/lux and /v1 on the public
// listener, the four doors beside them, /metrics on the internal
// listener with the store's and the doors' families in one registry,
// and the owner policy deciding when no authorizer is configured.
func TestServeMountsTheDoorsAndTheControlPlane(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	srv := startServe(t, map[string]string{"LUX_OIDC_ISSUERS": iss.URL()})
	defer srv.stop()
	if !strings.Contains(srv.out.String(), "luxd: control plane at "+publicURL+"/v1 on the public listener") {
		t.Fatalf("no control plane line in:\n%s", srv.out.String())
	}
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}

	resp, body := do(t, http.MethodGet, srv.publicURL+"/.well-known/lux", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"mode":"server"`) || !strings.Contains(body, `"api":"`+publicURL+`/v1"`) || !strings.Contains(body, iss.URL()) {
		t.Errorf("well-known: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/v1/self", "")
	if resp.StatusCode != 401 || errorCode(t, body) != "unauthenticated" || !strings.HasPrefix(resp.Header.Get("Lux-Request-Id"), "req_") {
		t.Errorf("self without a bearer: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/v1/self", "", bearer...)
	if resp.StatusCode != 200 || !strings.Contains(body, `"policy":"owner"`) || !strings.Contains(body, iss.URL()+"|alice") {
		t.Errorf("self: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPut, srv.publicURL+"/v1/budgets/team", `{"spec": {"amount": "50"}}`, bearer...)
	if resp.StatusCode != 201 || resp.Header.Get("ETag") != `"1"` || !strings.Contains(body, `"name":"team"`) {
		t.Errorf("apply: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/v1/budgets/team", "", bearer...)
	if resp.StatusCode != 200 || !strings.Contains(body, `"state":"Open"`) || resp.Header.Get("RateLimit-Limit") != "600" {
		t.Errorf("read: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = do(t, http.MethodOptions, srv.publicURL+"/v1/keys", "")
	if resp.StatusCode != 404 || errorCode(t, body) != "not_found" || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("OPTIONS: %d %s", resp.StatusCode, body)
	}
	for _, door := range []string{"openai", "anthropic", "gemini", "lux"} {
		resp, body := do(t, http.MethodGet, srv.publicURL+"/"+door+"/v1/models", "")
		if resp.StatusCode != 401 || resp.Header.Get("Lux-Error") != "unauthenticated" {
			t.Errorf("the %s door: %d %s", door, resp.StatusCode, body)
		}
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/openai/v1/models", "", "Authorization", "Bearer lux_nosuchkey")
	if resp.StatusCode != 401 || !strings.Contains(body, `"type":"unauthenticated"`) {
		t.Errorf("an unknown Key on the openai door: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.internalURL+"/metrics", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("/metrics: %d %v", resp.StatusCode, resp.Header)
	}
	for _, family := range []string{"lux_store_operations_total", "lux_requests_total", "lux_refusals_total", "lux_key_cache_hits_total"} {
		if !strings.Contains(body, "# TYPE "+family) {
			t.Errorf("/metrics lacks %s", family)
		}
	}
	if resp, _ := do(t, http.MethodGet, srv.internalURL+"/v1/self", ""); resp.StatusCode != 404 {
		t.Errorf("/v1 on the internal listener in server mode: %d", resp.StatusCode)
	}
}

// TestFileModeMountsOnTheInternalListener is spec 011's file mode
// through run: the read routes serve on the internal listener without a
// bearer, every write is read_only with Allow: GET, /v1/* on the public
// listener is not_found, and /.well-known/lux says mode file.
func TestFileModeMountsOnTheInternalListener(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "provider.yaml", providerYAML)
	srv := startServe(t, map[string]string{"LUX_MANIFEST_DIR": dir, "OPENAI_KEY": "sk-live", "LUX_OIDC_ISSUERS": "", "LUX_SECRETS_KEK": ""})
	defer srv.stop()
	if !strings.Contains(srv.out.String(), "luxd: control plane read-only on the internal listener") {
		t.Fatalf("no control plane line in:\n%s", srv.out.String())
	}
	resp, body := do(t, http.MethodGet, srv.internalURL+"/v1/providers", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"name":"openai"`) || strings.Contains(body, "sk-live") {
		t.Errorf("internal list: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.internalURL+"/v1/self", "")
	if resp.StatusCode != 200 || body != "{\"policy\":\"file\"}\n" {
		t.Errorf("internal self: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPut, srv.internalURL+"/v1/providers/openai", `{"spec": {}}`)
	if resp.StatusCode != 405 || errorCode(t, body) != "read_only" || resp.Header.Get("Allow") != "GET" || !strings.Contains(body, dir) {
		t.Errorf("internal write: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/v1/providers", "")
	if resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Errorf("public /v1: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/.well-known/lux", "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"mode":"file"`) || !strings.Contains(body, `"issuers":[]`) {
		t.Errorf("public well-known: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do(t, http.MethodGet, srv.internalURL+"/metrics", ""); resp.StatusCode != 200 {
		t.Errorf("/metrics in file mode: %d", resp.StatusCode)
	}
}

// TestUnauthenticatedBucketSpansBothPlanes: LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE
// bounds one client address's refused credentials across /v1 and the
// doors together; the refusal takes the plane's shape.
func TestUnauthenticatedBucketSpansBothPlanes(t *testing.T) {
	srv := startServe(t, map[string]string{"LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "3"})
	defer srv.stop()
	for i := range 2 {
		if resp, _ := do(t, http.MethodGet, srv.publicURL+"/v1/self", "", "Authorization", "Bearer bad"); resp.StatusCode != 401 {
			t.Fatalf("bad bearer %d: %d", i+1, resp.StatusCode)
		}
	}
	if resp, _ := do(t, http.MethodGet, srv.publicURL+"/openai/v1/models", "", "Authorization", "Bearer lux_bad"); resp.StatusCode != 401 {
		t.Fatalf("bad Key: %d", resp.StatusCode)
	}
	resp, body := do(t, http.MethodGet, srv.publicURL+"/openai/v1/models", "", "Authorization", "Bearer lux_bad")
	if resp.StatusCode != 429 || !strings.Contains(body, `"type":"rate_limited"`) || resp.Header.Get("Retry-After") == "" {
		t.Errorf("the fourth refused credential on a door: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/v1/self", "", "Authorization", "Bearer bad")
	if resp.StatusCode != 429 || errorCode(t, body) != "rate_limited" {
		t.Errorf("the fifth on /v1: %d %s", resp.StatusCode, body)
	}
	// The probes are served whatever the bucket says.
	if code, _ := get(t, srv.publicURL+"/livez"); code != 200 {
		t.Errorf("/livez behind an empty bucket: %d", code)
	}
}
