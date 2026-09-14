// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package issuer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/lux/internal/auth"
)

// TestIssuerStubMintsVerifiableTokens: the stub's discovery document and
// key set verify a token minted through POST /mint, through the same
// verifier luxd builds at start, under RS256 and under ES256, and the
// caller's subject is the rendering every owner field carries.
func TestIssuerStubMintsVerifiableTokens(t *testing.T) {
	for _, tc := range []struct {
		name  string
		es256 bool
		alg   string
	}{
		{"RS256", false, `"alg":"RS256"`},
		{"ES256", true, `"alg":"ES256"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iss := New(t, tc.es256)
			v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{iss.URL()}, Audience: Audience, HTTP: &http.Client{}})
			if err != nil {
				t.Fatalf("the verifier luxd uses refused the stub: %v", err)
			}
			token, err := Mint(t.Context(), &http.Client{}, iss.URL(), issuertest.Claims{Sub: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/self", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			caller, err := v.Authenticate(req)
			if err != nil {
				t.Fatalf("the minted token does not verify: %v", err)
			}
			if caller.Subject != Subject(iss.URL(), "alice") || caller.Sub != "alice" || caller.Issuer != iss.URL() {
				t.Fatalf("caller = %+v", caller)
			}
			keys := get(t, iss.URL()+"/jwks")
			if !strings.Contains(keys, tc.alg) {
				t.Fatalf("the key set does not advertise %s: %s", tc.alg, keys)
			}
			// A token for another audience is refused: the default audience
			// is what makes the plain mint verify.
			other, err := Mint(t.Context(), &http.Client{}, iss.URL(), issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"elsewhere"}})
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+other)
			if _, err := v.Authenticate(req); err == nil {
				t.Fatal("a token for another audience verified")
			}
		})
	}
}

// TestIssuerHandlerServesTheURLItIsGiven: NewHandler is for a binary
// serving its own address, and the discovery document names the URL the
// other processes reach it at.
func TestIssuerHandlerServesTheURLItIsGiven(t *testing.T) {
	h := NewHandler("http://issuer.example.com:9000", true)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	doc := get(t, srv.URL+"/.well-known/openid-configuration")
	if !strings.Contains(doc, `"issuer":"http://issuer.example.com:9000"`) {
		t.Fatalf("discovery = %s", doc)
	}
	if h.URL() != "http://issuer.example.com:9000" {
		t.Fatalf("URL() = %q", h.URL())
	}
}

// TestMintReportsTheIssuersRefusal: a mint that fails is an error naming
// the route and the status, and an answer without a token is one too.
func TestMintReportsTheIssuersRefusal(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mint" && r.Header.Get("Content-Type") == "application/json" {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer refusing.Close()
	if _, err := Mint(t.Context(), &http.Client{}, refusing.URL, issuertest.Claims{}); err == nil || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("err = %v", err)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer empty.Close()
	if _, err := Mint(t.Context(), &http.Client{}, empty.URL, issuertest.Claims{}); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Mint(t.Context(), &http.Client{}, "http://127.0.0.1:1", issuertest.Claims{}); err == nil {
		t.Fatal("a refused connection minted")
	}
	if _, err := Mint(t.Context(), &http.Client{}, "::not a url", issuertest.Claims{}); err == nil {
		t.Fatal("a URL that does not parse minted")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}
