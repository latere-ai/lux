// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/lux/client"
)

// TestImporterAppliesAndReadsAnObject is the surface another module
// holds: construct a Client with a token source, apply one object, read
// it back byte for byte, and decode a refusal into an *Error. Everything
// it names is exported, so the test fails to compile if the surface a
// plane or a migration tool drives stops being reachable from outside
// the package.
func TestImporterAppliesAndReadsAnObject(t *testing.T) {
	applied := []byte(`{"apiVersion":"lux.latere.ai/v1beta1","kind":"Key","metadata":{"name":"run-42"}}`)
	stored := map[string][]byte{}
	var sawAuth, sawIfMatch, sawContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(client.HeaderRequestID, "req_EXTERNAL")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/keys/run-42":
			sawAuth = r.Header.Get("Authorization")
			sawIfMatch = r.Header.Get("If-Match")
			sawContentType = r.Header.Get("Content-Type")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			stored[r.URL.Path] = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			if _, err := w.Write(body); err != nil {
				t.Errorf("write the applied object: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/keys/run-42":
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write(stored[r.URL.Path]); err != nil {
				t.Errorf("write the stored object: %v", err)
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			if _, err := w.Write([]byte(`{"error":{"code":"not_found","message":"There is no such object.","details":{"detail":"no route"}}}`)); err != nil {
				t.Errorf("write the refusal: %v", err)
			}
		}
	}))
	defer srv.Close()

	c := &client.Client{BaseURL: srv.URL, Token: client.StaticToken("token-abc"), UserAgent: "plane/1"}

	put, err := c.Apply(t.Context(), "keys", "run-42", applied, "application/json", "*")
	if err != nil {
		t.Fatalf("apply the object: %v", err)
	}
	if put.Status != http.StatusCreated {
		t.Errorf("apply answered %d, want %d", put.Status, http.StatusCreated)
	}
	if put.RequestID != "req_EXTERNAL" {
		t.Errorf("apply read the request id %q, want %q", put.RequestID, "req_EXTERNAL")
	}
	if sawAuth != "Bearer token-abc" {
		t.Errorf("the server saw Authorization %q, want %q", sawAuth, "Bearer token-abc")
	}
	if sawIfMatch != "*" {
		t.Errorf("the server saw If-Match %q, want %q", sawIfMatch, "*")
	}
	if sawContentType != "application/json" {
		t.Errorf("the server saw Content-Type %q, want %q", sawContentType, "application/json")
	}

	got, err := c.Get(t.Context(), "keys", "run-42")
	if err != nil {
		t.Fatalf("read the object back: %v", err)
	}
	if !bytes.Equal(got.Body, applied) {
		t.Errorf("read back %s, want %s", got.Body, applied)
	}

	_, err = c.Get(t.Context(), "keys", "absent")
	var refusal *client.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("a missing object answered %v, want an *Error", err)
	}
	if refusal.Code != "not_found" || refusal.Status != http.StatusNotFound {
		t.Errorf("the refusal is %d %s, want 404 not_found", refusal.Status, refusal.Code)
	}
	if refusal.Message != "There is no such object." {
		t.Errorf("the refusal's sentence is %q", refusal.Message)
	}
	if refusal.RequestID != "req_EXTERNAL" {
		t.Errorf("the refusal carries the request id %q, want %q", refusal.RequestID, "req_EXTERNAL")
	}
	if want := "/v1/keys/absent"; client.ObjectPath("keys", "absent") != want {
		t.Errorf("ObjectPath is %q, want %q", client.ObjectPath("keys", "absent"), want)
	}
}
