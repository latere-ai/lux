// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
)

// envOf reads a map as the environment.
func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestFromEnvBuildsTheConfig: the six variables land in Config, the
// subject is read from the token when unset, and a trailing slash on a
// URL is dropped.
func TestFromEnvBuildsTheConfig(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	token := iss.Mint(issuertest.Claims{Sub: "alice"})
	cfg, skip := fromEnv(envOf(map[string]string{
		EnvURL: "https://lux.example.com/", EnvToken: token, EnvStubsURL: "http://stubs.example.com/", EnvInternalURL: "http://internal.example.com",
	}))
	if skip != "" {
		t.Fatalf("skip %q", skip)
	}
	if cfg.URL != "https://lux.example.com" || cfg.StubsURL != "http://stubs.example.com" || cfg.InternalURL != "http://internal.example.com" {
		t.Errorf("config %+v", cfg)
	}
	if want := authz.Subject(iss.URL(), "alice"); cfg.Subject != want {
		t.Errorf("subject %q, want %q", cfg.Subject, want)
	}
	if got, ok := cfg.Token(cfg.Subject); !ok || got != token {
		t.Errorf("Token(default) = %q, %v", got, ok)
	}
	if _, ok := cfg.Token(authz.Subject(iss.URL(), "bob")); ok {
		t.Error("a second subject was minted without LUX_TEST_ISSUER_URL")
	}
	explicit, _ := fromEnv(envOf(map[string]string{EnvURL: "https://lux.example.com", EnvToken: token, EnvSubject: "https://other.example.com|alice"}))
	if explicit.Subject != "https://other.example.com|alice" {
		t.Errorf("an explicit subject read as %q", explicit.Subject)
	}
	bare, _ := fromEnv(envOf(map[string]string{EnvURL: "https://lux.example.com"}))
	if bare.Token != nil || bare.Subject != "" {
		t.Errorf("without a token: %+v", bare)
	}
}

// TestTokenMintsThroughTheIssuer: with LUX_TEST_ISSUER_URL the Token
// mints for another subject of that issuer with the given token's
// audience, and for no subject of another issuer.
func TestTokenMintsThroughTheIssuer(t *testing.T) {
	iss := issuertest.New(t)
	token := iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux"}})
	cfg, _ := fromEnv(envOf(map[string]string{EnvURL: "https://lux.example.com", EnvToken: token, EnvIssuerURL: iss.URL() + "/"}))
	minted, ok := cfg.Token(authz.Subject(iss.URL(), "bob"))
	if !ok {
		t.Fatal("the issuer minted nothing for bob")
	}
	claims := payloadOf(minted)
	if claims.sub != "bob" || claims.iss != iss.URL() || !strings.Contains(strings.Join(claims.aud, ","), "lux") {
		t.Errorf("minted %+v", claims)
	}
	if _, ok := cfg.Token("https://other.example.com|bob"); ok {
		t.Error("a subject of another issuer was minted")
	}
	if _, ok := cfg.Token("not-a-subject"); ok {
		t.Error("a bare string was minted")
	}
	iss.Close()
	if _, ok := cfg.Token(authz.Subject(iss.URL(), "carol")); ok {
		t.Error("a closed issuer minted")
	}
}

// TestMintRefusals: a mint answered with an error status or without a
// token is an error.
func TestMintRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"error status", http.StatusInternalServerError, "no"},
		{"no token", http.StatusOK, `{"other":"x"}`},
		{"not json", http.StatusOK, "{"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := mint(newHTTPClient(), srv.URL, "bob", nil); err == nil {
				t.Error("no error")
			}
		})
	}
	if _, err := mint(newHTTPClient(), "http://127.0.0.1:1", "bob", nil); err == nil {
		t.Error("a refused connection minted")
	}
}

// TestPayloadOfReadsTheClaims: iss, sub, and aud as a string or a list
// are read without verification, and anything that is not a JWS reads
// as no claims.
func TestPayloadOfReadsTheClaims(t *testing.T) {
	iss := issuertest.New(t)
	one := payloadOf(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux"}}))
	if one.iss != iss.URL() || one.sub != "alice" || len(one.aud) != 1 || one.aud[0] != "lux" {
		t.Errorf("claims %+v", one)
	}
	many := payloadOf(iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux", "other"}}))
	if len(many.aud) != 2 {
		t.Errorf("aud %v", many.aud)
	}
	for _, bad := range []string{"", "a.b", "a.!!!.c", "a." + "e30" + ".c"} {
		if got := payloadOf(bad); got.iss != "" || got.sub != "" {
			t.Errorf("%q read as %+v", bad, got)
		}
	}
}
