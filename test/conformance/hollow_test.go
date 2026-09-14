// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// hollowValue is the Key value the hollow server answers every create
// with, lux_ and forty characters as a minted one is.
const hollowValue = "lux_" + "hollowhollowhollowhollowhollowhollowholl"

// hollowServer answers every request with a plausible success: a
// well-known document naming itself, an OpenAPI document with one route,
// a stubs document naming itself as every stub, 201 to every PUT so the
// suite mints its Key and applies its fixtures, and one body with a
// member for every reader, so the suite gets far into each case and
// finds every assertion false. A suite that is green against it proves
// nothing, which is what the test below holds it to.
func hollowServer(t testing.TB, skewed bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	requestID := "req_" + strings.Repeat("0", 26)
	body := map[string]any{
		"items": []any{}, "metadata": map[string]any{"name": "x"}, "spec": map[string]any{},
		"status": map[string]any{"id": "key_00000000000000000000000000", "value": hollowValue, "prefix": hollowValue[:12], "version": 1, "warnings": []any{}, "owner": "nobody"},
		"error":  map[string]any{"code": "nope", "message": "No", "details": map[string]any{"request_id": "req_other"}},
		"object": "list", "data": []any{}, "models": []any{}, "policy": "nobody", "input_tokens": 0, "source": "nowhere",
		"subject": "nobody", "issuer": "", "sub": "",
	}
	if skewed {
		body["items"] = "not a list"
	}
	record := map[string]any{"id": requestID, "status": "weird", "error": "x", "attempts": []any{map[string]any{}}, "tokens": map[string]any{"input": 1, "estimated": true},
		"cost": map[string]any{"amount": 1.5, "priced": true, "currency": "XX"}, "key": map[string]any{"id": "x", "prefix": "y"}, "owner": "nobody", "door": "lux",
		"model": map[string]any{"name": "m", "id": "x"}, "provider": map[string]any{"name": "p", "id": "x"}, "upstreamModel": "u", "route": "/x", "translated": true}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Lux-Request-Id", requestID)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/.well-known/lux" || strings.HasPrefix(r.URL.Path, "/stub") {
			// The control plane and the stubs answer as below.
		} else {
			w.Header().Set("Lux-Error", "nope")
		}
		switch {
		case r.URL.Path == "/.well-known/lux":
			doors := map[string]string{}
			for _, d := range dialects {
				doors[d] = srv.URL + "/" + d
			}
			doc := map[string]any{
				"name": "lux", "version": "hollow", "apiVersion": v1.APIVersion, "api": srv.URL + "/v1", "openapi": srv.URL + "/v1/openapi.json",
				"doors": doors, "dialects": dialects, "issuers": []string{"https://issuer.example.com"}, "audience": "lux", "mode": "server",
			}
			if skewed {
				doors["extra"] = "http://elsewhere.example.com"
				doc["version"], doc["apiVersion"], doc["openapi"], doc["issuers"], doc["audience"] = "", "v9", "x", []string{}, ""
				doc["doors"] = doors
			}
			_ = json.NewEncoder(w).Encode(doc)
		case strings.HasSuffix(r.URL.Path, "/_received"):
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode([]received{{Method: "GET", Path: "/elsewhere", Headers: map[string][]string{"Authorization": {"Bearer " + hollowValue}}, Body: "{}"}})
		case strings.HasSuffix(r.URL.Path, "/fail"):
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/requests":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{record}, "source": "nowhere"})
		case r.URL.Path == "/v1/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"requests": 7, "cost": 1.5, "dimensions": map[string]any{"key": "x"}}}})
		case r.URL.Path == "/v1/openapi.json":
			// The kinds' routes with a schema that admits any object, so
			// the suite's own holding lets the Key mint and the fixtures
			// apply; every other route is outside the document.
			anyObject := map[string]any{"default": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}}
			paths := map[string]any{}
			for _, kind := range kindOrder {
				ops := map[string]any{}
				for _, method := range []string{"get", "put", "delete", "post"} {
					ops[method] = map[string]any{"responses": anyObject}
				}
				paths["/v1/"+plurals[kind]] = ops
				paths["/v1/"+plurals[kind]+"/{name}"] = ops
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"openapi": "3.1.0", "paths": paths, "components": map[string]any{"schemas": map[string]any{"Error": map[string]any{"type": "object"}}}})
		case r.URL.Path == "/stubs/":
			providers := map[string]string{}
			for _, d := range dialects {
				providers[d] = srv.URL + "/stub/" + d
			}
			_ = json.NewEncoder(w).Encode(stubs{Providers: providers, Authorizer: srv.URL + "/stub/authorizer", Credential: "hollow"})
		default:
			data, _ := io.ReadAll(r.Body)
			if strings.Contains(string(data), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				for range 3 {
					_, _ = io.WriteString(w, "data: {\"object\":\"chunk\"}\n\n")
				}
				_, _ = io.WriteString(w, "data: {\"model\":\"x\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
				return
			}
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSuiteIsRedAgainstASkewedDocument: a well-known document whose
// members disagree with spec 011 and a list that is not an array fail
// their two cases, which the plausible hollow server passes.
func TestSuiteIsRedAgainstASkewedDocument(t *testing.T) {
	srv := hollowServer(t, true)
	c := newClient(t, Config{URL: srv.URL, Token: func(string) (string, bool) { return "static-token", true }})
	for _, name := range []string{"case011WellKnown", "case011ListReads"} {
		for _, tc := range cases {
			if tc.name != name {
				continue
			}
			if f := drive(t, tc.group+"/"+tc.name, func(t testing.TB) { tc.fn(t, c) }); !f.Failed() {
				t.Errorf("%s passed against the skewed document", name)
			}
		}
	}
}

// TestSuiteIsRedAgainstAHollowServer: against a server that answers
// everything with a plausible success the suite mints its Key, applies
// its fixtures, and then fails across every group. This is the other
// half of the mutation check: the suite is not green by construction.
func TestSuiteIsRedAgainstAHollowServer(t *testing.T) {
	srv := hollowServer(t, false)
	subject := "https://issuer.example.com|alice"
	c := newClient(t, Config{URL: srv.URL, StubsURL: srv.URL + "/stubs", Subject: subject, Token: func(s string) (string, bool) { return "static-token", s == subject }})
	c.patience = 300 * time.Millisecond
	if mint := drive(t, "doors/case007SuiteMintsItsKey", func(t testing.TB) { c.mintKey(t) }); mint.Failed() || c.key == nil {
		t.Fatalf("the hollow server did not mint a Key:\n%s", mint.output())
	}
	if f := drive(t, "doors/fixtures", func(t testing.TB) { c.applyFixtures(t) }); f.Failed() {
		t.Fatalf("the fixtures did not apply:\n%s", f.output())
	}
	var failed, passed, skipped []string
	for _, tc := range cases {
		if reason := c.caseSkip(tc); reason != "" {
			skipped = append(skipped, tc.name)
			continue
		}
		f := drive(t, tc.group+"/"+tc.name, func(t testing.TB) { tc.fn(t, c) })
		switch {
		case f.Failed():
			failed = append(failed, tc.name)
		case f.Skipped():
			skipped = append(skipped, tc.name)
		default:
			passed = append(passed, tc.name)
		}
	}
	// The cases that pass against a server that answers everything with a
	// success are the ones whose every assertion is a success: the
	// well-known document the hollow server copies from spec 011, a list
	// that is an array, and the credential forms, which are each a 200.
	slices.Sort(passed)
	slices.Sort(skipped)
	if want := []string{"case004CredentialForms", "case011ListReads", "case011WellKnown"}; !slices.Equal(passed, want) {
		t.Errorf("passed against the hollow server: %v, want %v", passed, want)
	}
	if len(failed) < 40 {
		t.Errorf("only %d cases failed: %v", len(failed), failed)
	}
	if want := []string{"case003PreviousReleaseManifests", "case009PreviousReleaseRecords", "case011ReadOnlyInFileMode"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped against the hollow server: %v, want %v", skipped, want)
	}
}

// TestClientRefusesAServerThatIsNotLux: the well-known document is the
// one thing Run trusts, and a server that does not serve it, serves
// another product's, or names a mode the suite does not know, fails the
// run before any case.
func TestClientRefusesAServerThatIsNotLux(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"no document", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }},
		{"not json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }},
		{"another product", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"name":"other","api":"x","doors":{"a":"b"}}`))
		}},
		{"unknown mode", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"name":"lux","api":"http://lux.example.com/v1","doors":{"openai":"x"},"mode":"weird"}`))
		}},
		{"no openapi", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/lux" {
				_, _ = w.Write([]byte(`{"name":"lux","api":"http://` + r.Host + `/v1","doors":{"openai":"x"},"mode":"server"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}},
		{"bad openapi", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/lux" {
				_, _ = w.Write([]byte(`{"name":"lux","api":"http://` + r.Host + `/v1","doors":{"openai":"x"},"mode":"server"}`))
				return
			}
			_, _ = w.Write([]byte(`{"openapi":"2.0"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			if f := drive(t, "client", func(t testing.TB) { newClient(t, Config{URL: srv.URL}) }); !f.Failed() {
				t.Error("the client accepted the server")
			}
		})
	}
	if f := drive(t, "client", func(t testing.TB) { newClient(t, Config{}) }); !f.Failed() {
		t.Error("a client without a URL was built")
	}
	if f := drive(t, "client", func(t testing.TB) { newClient(t, Config{URL: "http://127.0.0.1:1"}) }); !f.Failed() {
		t.Error("a client to a refused connection was built")
	}
}

// TestStubsDocumentIsRequiredWhole: a stubs URL that does not answer, is
// not the document, or names no provider for a listed dialect fails the
// run, so a partial run never passes for a full one; and the stub
// helpers fail loudly when a stub does not answer as its contract says.
func TestStubsDocumentIsRequiredWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"no answer", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"not the document", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"providers":{}}`)) }},
		{"a dialect missing", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"providers":{"openai":"http://stub.example.com"}}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			c := &client{cfg: Config{URL: "http://lux.example.com", StubsURL: srv.URL}, http: newHTTPClient(), ctx: t.Context(), well: wellKnown{Dialects: dialects}}
			if f := drive(t, "load", func(t testing.TB) { c.loadStubs(t) }); !f.Failed() {
				t.Error("the stubs document was accepted")
			}
		})
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer broken.Close()
	c := &client{cfg: Config{URL: "http://lux.example.com"}, http: newHTTPClient(), ctx: t.Context(), stubs: &stubs{Authorizer: broken.URL}}
	for name, fn := range map[string]func(testing.TB){
		"received":       func(t testing.TB) { c.received(t, broken.URL) },
		"clearReceived":  func(t testing.TB) { c.clearReceived(t, broken.URL) },
		"lastReceived":   func(t testing.TB) { c.lastReceived(t, broken.URL) },
		"authorizerFail": func(t testing.TB) { c.authorizerFail(t, 503) },
	} {
		if f := drive(t, name, fn); !f.Failed() {
			t.Errorf("%s passed against a stub that answers 418", name)
		}
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer empty.Close()
	if f := drive(t, "lastReceived", func(t testing.TB) { c.lastReceived(t, empty.URL) }); !f.Failed() {
		t.Error("lastReceived passed against a stub that recorded nothing")
	}
	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) }))
	defer notJSON.Close()
	if f := drive(t, "received", func(t testing.TB) { c.received(t, notJSON.URL) }); !f.Failed() {
		t.Error("received passed a body that is not JSON")
	}
}

// TestHTTPClientFollowsNoRedirect: the suite's client returns a 3xx as
// the answer, since a redirect is not an answer in either plane's table;
// and the small helpers around it.
func TestHTTPClientFollowsNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/a", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status %d, want the redirect itself", resp.StatusCode)
	}
	if got := excerpt(make([]byte, 5000)); len(got) != 2048+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("excerpt of 5000 bytes is %d long", len(got))
	}
	if excerpt([]byte("short")) != "short" {
		t.Error("a short body was cut")
	}
	for s, want := range map[string]bool{"One sentence.": true, "Two. Sentences.": false, "No stop": false, "": false} {
		if oneSentence(s) != want {
			t.Errorf("oneSentence(%q) = %v", s, !want)
		}
	}
	if s := sprintf("%d %s", 1, "x"); s != "1 x" {
		t.Errorf("sprintf %q", s)
	}
	if f := drive(t, "json", func(t testing.TB) { (&response{Body: []byte("[]")}).json(t) }); !f.Failed() {
		t.Error("a body that is not an object decoded")
	}
	if f := drive(t, "encode", func(t testing.TB) { encode(t, make(chan int)) }); !f.Failed() {
		t.Error("a value that does not encode was encoded")
	}
	if f := drive(t, "canonical", func(t testing.TB) { canonical(t, make(chan int)) }); !f.Failed() {
		t.Error("a value that does not encode was rendered")
	}
}
