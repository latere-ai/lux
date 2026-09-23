// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/localissuer"
	stubsink "latere.ai/x/lux/test/stubs/sink"
)

// issuerKey is a fresh P-256 key as LUX_LOCAL_ISSUER_KEY carries it,
// with its key id.
func issuerKey(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	id, err := localissuer.KeyID(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), id
}

// mintToken runs luxd token under vars with args and returns the one
// line it printed, failing the test on any other outcome.
func mintToken(t *testing.T, vars map[string]string, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(t.Context(), append([]string{"token"}, args...), env(vars), &out, &errOut); code != 0 {
		t.Fatalf("luxd token %v: exit %d, stderr %q", args, code, errOut.String())
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// claimsOf is a token's payload, read without verifying it.
func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("%q is not a compact JWS", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// TestLocalIssuerAloneStartsTheServer is spec 035's first criterion: with
// LUX_LOCAL_ISSUER_KEY set, LUX_OIDC_ISSUERS unset, and no manifest
// directory, serve starts in the server mode, names the local issuer in
// its identity line, and answers GET /v1/self for a token it minted with
// the caller rendered as <LUX_PUBLIC_URL>|<sub>.
func TestLocalIssuerAloneStartsTheServer(t *testing.T) {
	key, kid := issuerKey(t)
	vars := map[string]string{"LUX_OIDC_ISSUERS": "", "LUX_LOCAL_ISSUER_KEY": key, "LUX_ADMIN_SUBJECTS": publicURL + "|root"}
	srv := startServe(t, vars)
	defer srv.stop()
	want := "luxd: identity: no listed issuer; local issuer " + publicURL + " (ES256 key " + kid + "); audience lux; owner policy with 1 admin subject(s)"
	if !strings.Contains(srv.out.String(), want) {
		t.Fatalf("no local issuer identity line in:\n%s", srv.out.String())
	}
	token := mintToken(t, serveEnv(t, vars))
	resp, body := do(t, http.MethodGet, srv.publicURL+"/v1/self", "", "Authorization", "Bearer "+token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/self: %d %s", resp.StatusCode, body)
	}
	var self struct {
		Subject, Issuer, Policy string
	}
	if err := json.Unmarshal([]byte(body), &self); err != nil {
		t.Fatal(err)
	}
	if self.Subject != publicURL+"|root" || self.Issuer != publicURL || self.Policy != "owner" {
		t.Fatalf("GET /v1/self = %s", body)
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/.well-known/lux", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"issuers":["`+publicURL+`"]`) || !strings.Contains(body, `"mode":"server"`) {
		t.Fatalf("GET /.well-known/lux: %d %s", resp.StatusCode, body)
	}
}

// TestTokenCommand is spec 035's luxd token: the token's subject,
// audience, and lifetime follow the flags and their defaults, it is
// printed alone on stdout, and a TTL out of range, a missing or ambiguous
// subject, an unlisted audience, an unset key, and a bad flag are usage
// errors, exit 2, while a variable it reads that does not parse is exit 1.
func TestTokenCommand(t *testing.T) {
	key, kid := issuerKey(t)
	base := map[string]string{
		"LUX_LOCAL_ISSUER_KEY": key,
		"LUX_OIDC_AUDIENCE":    "lux,api.example.com",
		"LUX_ADMIN_SUBJECTS":   "https://login.example.com|alice," + publicURL + "/|root",
	}
	with := func(edits map[string]string) map[string]string {
		m := maps.Clone(base)
		maps.Copy(m, edits)
		return m
	}
	for _, tc := range []struct {
		name   string
		env    map[string]string
		args   []string
		code   int
		sub    string        // the accepted token's sub
		aud    string        // its aud
		ttl    time.Duration // its exp minus its iat
		stderr string        // a fragment of the refusal
	}{
		{"every default", base, nil, 0, "root", "lux", time.Hour, ""},
		{"every flag", base, []string{"--subject", "ops", "--audience", "api.example.com", "--ttl", "30m"}, 0, "ops", "api.example.com", 30 * time.Minute, ""},
		{"the cap", base, []string{"-ttl=24h"}, 0, "root", "lux", 24 * time.Hour, ""},
		{"a TTL above the cap", base, []string{"--ttl", "25h"}, 2, "", "", 0, "--ttl is 25h0m0s, and a token lives above zero and at most 24h0m0s"},
		{"a zero TTL", base, []string{"--ttl", "0s"}, 2, "", "", 0, "--ttl is 0s"},
		{"a negative TTL", base, []string{"--ttl", "-1m"}, 2, "", "", 0, "--ttl is -1m0s"},
		{"a TTL that is not a duration", base, []string{"--ttl", "soon"}, 2, "", "", 0, "invalid value"},
		{"no admin subject of the local issuer", with(map[string]string{"LUX_ADMIN_SUBJECTS": "https://login.example.com|alice"}), nil, 2, "", "", 0,
			"--subject is required, since LUX_ADMIN_SUBJECTS names 0 subject(s) of the local issuer " + publicURL},
		{"two admin subjects of the local issuer", with(map[string]string{"LUX_ADMIN_SUBJECTS": publicURL + "|root," + publicURL + "|ops"}), nil, 2, "", "", 0,
			"LUX_ADMIN_SUBJECTS names 2 subject(s) of the local issuer"},
		{"a subject the flag names beside two admins", with(map[string]string{"LUX_ADMIN_SUBJECTS": publicURL + "|root," + publicURL + "|ops"}), []string{"--subject", "ops"}, 0, "ops", "lux", time.Hour, ""},
		{"a subject carrying the separator", base, []string{"--subject", "a|b"}, 2, "", "", 0, `--subject is "a|b", and a sub carries no |`},
		{"an unlisted audience", base, []string{"--audience", "other"}, 2, "", "", 0,
			`--audience is "other", and a token is minted for a name of LUX_OIDC_AUDIENCE: lux, api.example.com`},
		{"an unset key", with(map[string]string{"LUX_LOCAL_ISSUER_KEY": ""}), nil, 2, "", "", 0, "LUX_LOCAL_ISSUER_KEY is unset, and luxd token signs with the local issuer's key"},
		{"an argument", base, []string{"root"}, 2, "", "", 0, `unexpected argument "root"`},
		{"a bad flag", base, []string{"--no-such-flag"}, 2, "", "", 0, "-no-such-flag"},
		{"a key that is not a private key", with(map[string]string{"LUX_LOCAL_ISSUER_KEY": "sk-not-a-key"}), nil, 1, "", "", 0,
			"luxd: configuration: LUX_LOCAL_ISSUER_KEY is not a PEM encoded private key"},
		{"no public address", with(map[string]string{"LUX_PUBLIC_URL": ""}), nil, 1, "", "", 0, "LUX_PUBLIC_URL is unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(t.Context(), append([]string{"token"}, tc.args...), env(tc.env), &out, &errOut)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stdout %q, stderr %q", code, tc.code, out.String(), errOut.String())
			}
			if tc.code != 0 {
				if out.Len() != 0 || !strings.Contains(errOut.String(), tc.stderr) {
					t.Errorf("stdout %q, stderr %q; want nothing on stdout and %q on stderr", out.String(), errOut.String(), tc.stderr)
				}
				if strings.Contains(errOut.String(), strings.Split(key, "\n")[1]) {
					t.Errorf("stderr carries the key: %q", errOut.String())
				}
				return
			}
			if errOut.Len() != 0 || strings.Count(out.String(), "\n") != 1 || !strings.HasSuffix(out.String(), "\n") {
				t.Fatalf("stdout %q, stderr %q; want the token alone and a newline", out.String(), errOut.String())
			}
			token := strings.TrimSuffix(out.String(), "\n")
			claims := claimsOf(t, token)
			iat, exp := claims["iat"].(float64), claims["exp"].(float64)
			if claims["iss"] != publicURL || claims["sub"] != tc.sub || claims["aud"] != tc.aud || time.Duration(exp-iat)*time.Second != tc.ttl {
				t.Errorf("claims %v, want sub %s, aud %s, and a lifetime of %s", claims, tc.sub, tc.aud, tc.ttl)
			}
			var head map[string]any
			raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
			if err != nil || json.Unmarshal(raw, &head) != nil || head["kid"] != kid || head["alg"] != "ES256" {
				t.Errorf("header %s, want ES256 under kid %s", raw, kid)
			}
		})
	}
}

// TestTokenCommandRoundTrip: a token luxd token prints is one the running
// server accepts, under a listed issuer beside it, for the primary
// audience and for the second, and GET /v1/self renders its subject.
func TestTokenCommandRoundTrip(t *testing.T) {
	key, _ := issuerKey(t)
	vars := serveEnv(t, map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_OIDC_AUDIENCE": "lux,api.example.com", "LUX_ADMIN_SUBJECTS": publicURL + "|root"})
	srv := startServe(t, vars)
	defer srv.stop()
	for _, args := range [][]string{nil, {"--audience", "api.example.com", "--subject", "ops", "--ttl", "5m"}} {
		token := mintToken(t, vars, args...)
		resp, body := do(t, http.MethodGet, srv.publicURL+"/v1/self", "", "Authorization", "Bearer "+token)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"subject":"`+publicURL+`|`+claimsOf(t, token)["sub"].(string)+`"`) {
			t.Fatalf("GET /v1/self with luxd token %v: %d %s", args, resp.StatusCode, body)
		}
	}
	other, _ := issuerKey(t)
	stranger := mintToken(t, map[string]string{"LUX_LOCAL_ISSUER_KEY": other, "LUX_ADMIN_SUBJECTS": publicURL + "|root"})
	if resp, body := do(t, http.MethodGet, srv.publicURL+"/v1/self", "", "Authorization", "Bearer "+stranger); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a token of another key: %d %s", resp.StatusCode, body)
	}
}

// TestTokenCommandIsReadOnly is spec 035's seventh criterion, in the
// shape of TestCheckIsReadOnly: luxd token against a serving installation
// changes no object, sends the sink no event, asks the authorizer
// nothing, and dials no issuer. It needs neither LUX_SECRETS_KEK nor a
// database: with the key encryption key unset and LUX_DB_URL naming a
// port nothing answers on, it still mints, which it could not if it
// opened a store.
func TestTokenCommandIsReadOnly(t *testing.T) {
	key, _ := issuerKey(t)
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	authorizer := stub.New(t)
	events := stubsink.New(stubsink.Options{Secret: []byte(stubsink.DefaultSecret)})
	sinkSrv := httptest.NewServer(events)
	defer sinkSrv.Close()
	vars := serveEnv(t, map[string]string{
		"LUX_OIDC_ISSUERS":     iss.URL(),
		"LUX_LOCAL_ISSUER_KEY": key,
		"LUX_ADMIN_SUBJECTS":   publicURL + "|root",
		"LUX_AUTHORIZER_URL":   authorizer.URL(),
		"LUX_AUTHORIZER_TOKEN": authorizer.Token(),
		"LUX_EVENTS_URL":       sinkSrv.URL,
		"LUX_EVENTS_SECRET":    stubsink.DefaultSecret,
	})
	srv := startServe(t, vars)
	defer srv.stop()
	bearer := []string{"Authorization", "Bearer " + mintToken(t, vars)}
	if resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/providers/openai",
		`{"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider","metadata":{"name":"openai"},"spec":{"dialect":"openai","baseURL":"https://api.example.com/v1","credential":{"value":"sk-canary"}}}`, bearer...); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	snapshot := func() string {
		var parts []string
		for _, kind := range []string{"providers", "models", "keys", "budgets"} {
			resp, body := do(t, http.MethodGet, srv.publicURL+"/v1/"+kind, "", bearer...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /v1/%s: %d %s", kind, resp.StatusCode, body)
			}
			var list struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal([]byte(body), &list); err != nil {
				t.Fatalf("GET /v1/%s: %v in %s", kind, err, body)
			}
			for _, item := range list.Items {
				if status, ok := item["status"].(map[string]any); ok {
					delete(status, "health")
				}
			}
			normalised, err := json.Marshal(list.Items)
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, string(normalised))
		}
		return strings.Join(parts, "\n")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(events.Events()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the serve delivered no event for the Provider it created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := snapshot()
	eventsBefore, decisionsBefore, issuerBefore := len(events.Events()), len(authorizer.Requests()), len(iss.Requests())

	vars["LUX_SECRETS_KEK"] = ""
	vars["LUX_DB_URL"] = "postgres://lux:secret@" + closedPort(t) + "/lux?sslmode=disable"
	for range 3 {
		mintToken(t, vars)
	}

	if got := len(authorizer.Requests()); got != decisionsBefore {
		t.Errorf("the authorizer received %d request(s) from luxd token", got-decisionsBefore)
	}
	if got := len(iss.Requests()); got != issuerBefore {
		t.Errorf("the issuer received %d request(s) from luxd token", got-issuerBefore)
	}
	if after := snapshot(); after != before {
		t.Errorf("the objects changed:\n%s\n---\n%s", before, after)
	}
	// The sink is read after the snapshot, whose reads journal nothing,
	// so a delivery the token role caused has had the snapshot's time to
	// arrive.
	if got := len(events.Events()); got != eventsBefore {
		t.Errorf("%d event(s) reached the sink from luxd token", got-eventsBefore)
	}
}
