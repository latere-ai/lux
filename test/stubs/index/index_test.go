// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// document is the index of a run with every stub named.
func document() Document {
	return Document{
		Providers:  map[string]string{"openai": "http://127.0.0.1:1", "anthropic": "http://127.0.0.1:2", "gemini": "http://127.0.0.1:3", "lux": "http://127.0.0.1:4"},
		Issuer:     "http://127.0.0.1:5",
		Authorizer: "http://127.0.0.1:6",
		Sink:       "http://127.0.0.1:7",
		Credential: "stub-credential",
	}
}

// get sends one request to the handler and reads the answer whole.
func get(t *testing.T, h http.Handler, method, path string) (int, http.Header, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// TestIndexServesTheDocument: GET / answers the document naming every
// stub and the credential, as JSON, under the wire names a reader across
// a process boundary decodes.
func TestIndexServesTheDocument(t *testing.T) {
	status, header, body := get(t, New(document()), http.MethodGet, "/")
	if status != http.StatusOK || header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET / = %d %s", status, header.Get("Content-Type"))
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("GET / is not JSON: %v\n%s", err, body)
	}
	providers, ok := doc["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers is %T in %s", doc["providers"], body)
	}
	for _, d := range []string{"openai", "anthropic", "gemini", "lux"} {
		if providers[d] != document().Providers[d] {
			t.Errorf("providers.%s = %v", d, providers[d])
		}
	}
	for name, want := range map[string]string{"issuer": document().Issuer, "authorizer": document().Authorizer, "sink": document().Sink, "credential": document().Credential} {
		if doc[name] != want {
			t.Errorf("%s = %v, want %q", name, doc[name], want)
		}
	}
	// The same bytes twice: a reader that compares two reads sees one
	// document.
	_, _, again := get(t, New(document()), http.MethodGet, "/")
	if again != body {
		t.Errorf("two reads differ:\n%s\n%s", body, again)
	}
}

// TestIndexDecodesIntoTheDocument: the document a reader decodes carries
// what the handler was built with, so a member renamed on one side is a
// failure here and not a silent empty field.
func TestIndexDecodesIntoTheDocument(t *testing.T) {
	_, _, body := get(t, New(document()), http.MethodGet, "/")
	var got Document
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	want := document()
	if got.Issuer != want.Issuer || got.Authorizer != want.Authorizer || got.Sink != want.Sink || got.Credential != want.Credential || len(got.Providers) != len(want.Providers) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
}

// TestIndexEmptyProvidersIsAnObject: an index built with no providers
// answers an empty object rather than null, so a reader's map is never
// nil.
func TestIndexEmptyProvidersIsAnObject(t *testing.T) {
	_, _, body := get(t, New(Document{}), http.MethodGet, "/")
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if providers, ok := doc["providers"].(map[string]any); !ok || len(providers) != 0 {
		t.Fatalf("providers = %v in %s", doc["providers"], body)
	}
}

// TestIndexRefusesEveryOtherRoute: a path that is not / is 404 naming
// the one route, and a method that is not GET or HEAD is 405 with Allow.
func TestIndexRefusesEveryOtherRoute(t *testing.T) {
	h := New(document())
	if status, _, body := get(t, h, http.MethodGet, "/providers"); status != http.StatusNotFound {
		t.Errorf("GET /providers = %d %s", status, body)
	}
	status, header, _ := get(t, h, http.MethodPost, "/")
	if status != http.StatusMethodNotAllowed || header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST / = %d Allow %q", status, header.Get("Allow"))
	}
	if status, _, body := get(t, h, http.MethodHead, "/"); status != http.StatusOK || body != "" {
		t.Errorf("HEAD / = %d %q", status, body)
	}
}
