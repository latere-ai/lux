// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authkit/jwt"

	"latere.ai/x/lux/internal/localissuer"
)

// localURL is LUX_PUBLIC_URL in these tests, which load fills in, and so
// the local issuer's name.
const localURL = "https://lux.example.com"

// localKey is a fresh P-256 key as the PEM text LUX_LOCAL_ISSUER_KEY
// carries, and parsed, so a test signs with the key it configured.
func localKey(t *testing.T) (string, *localissuer.Key) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pemOf(t, k)
}

// rsaLocalKey is localKey for an RSA key.
func rsaLocalKey(t *testing.T) (string, *localissuer.Key) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pemOf(t, k)
}

func pemOf(t *testing.T, k crypto.PrivateKey) (string, *localissuer.Key) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	text := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	parsed, err := localissuer.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return text, parsed
}

// mint signs a token of the local issuer for sub with the given key,
// audience, and lifetime, the way luxd token does.
func mint(t *testing.T, k *localissuer.Key, iss, sub, aud string, ttl time.Duration) string {
	t.Helper()
	token, err := k.Mint(localissuer.Claims{Issuer: iss, Subject: sub, Audience: aud, IssuedAt: time.Now(), TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestLocalIssuerTokenIsAccepted is spec 035's third criterion at the
// verifier: with LUX_LOCAL_ISSUER_KEY set, a token that key signed with
// iss LUX_PUBLIC_URL and a listed audience is accepted and renders
// <LUX_PUBLIC_URL>|<sub>; the audiences apply to it exactly as to a
// listed issuer's token, and a token the key did not sign, or one that
// expired, is unauthenticated.
func TestLocalIssuerTokenIsAccepted(t *testing.T) {
	text, key := localKey(t)
	_, stranger := localKey(t)
	a, err := Startup(t.Context(), load(t, map[string]string{
		"LUX_LOCAL_ISSUER_KEY": text, "LUX_OIDC_AUDIENCE": "lux,api.example.com",
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verifier.LocalIssuer() != localURL {
		t.Fatalf("LocalIssuer() = %q, want %q", a.Verifier.LocalIssuer(), localURL)
	}
	for _, tc := range []struct {
		name   string
		token  string
		detail string // the refusal's finding, or "" for an accepted token
	}{
		{"the primary audience", mint(t, key, localURL, "alice", "lux", time.Hour), ""},
		{"the second audience", mint(t, key, localURL, "alice", "api.example.com", time.Hour), ""},
		{"the issuer with a trailing slash", mint(t, key, localURL+"/", "alice", "lux", time.Hour), ""},
		{"an unlisted audience", mint(t, key, localURL, "alice", "other", time.Hour), "its aud names none of LUX_OIDC_AUDIENCE lux, api.example.com"},
		{"another key", mint(t, stranger, localURL, "alice", "lux", time.Hour), jwt.ErrUnknownKey.Error()},
		{"an expired token", func() string {
			token, err := key.Mint(localissuer.Claims{Issuer: localURL, Subject: "alice", Audience: "lux", IssuedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			return token
		}(), jwt.ErrTokenExpired.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := a.Verifier.Authenticate(request("Bearer " + tc.token))
			if tc.detail != "" {
				unauthenticated(t, err, tc.detail)
				return
			}
			if err != nil {
				t.Fatalf("Authenticate() = %v", err)
			}
			if c.Subject != localURL+"|alice" || c.Issuer != localURL || c.Sub != "alice" || c.Claims["sub"] != "alice" {
				t.Errorf("caller = %+v", c)
			}
		})
	}
}

// TestLocalIssuerBesideAListedIssuer is the second criterion: with a
// local key and a listed issuer both configured, a token from either is
// accepted, and each is verified by its own path, selected by its iss. A
// local token reaches no issuer, the local key verifies nothing that
// names the listed issuer, and the listed issuer's keys verify nothing
// that names the local one.
func TestLocalIssuerBesideAListedIssuer(t *testing.T) {
	iss := newIssuer(t)
	// forger is a stub that signs as the local issuer with its own keys.
	forger := newIssuer(t, issuertest.WithIssuer(localURL))
	text, key := localKey(t)
	a, err := Startup(t.Context(), load(t, map[string]string{
		"LUX_OIDC_ISSUERS": iss.URL(), "LUX_LOCAL_ISSUER_KEY": text,
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := reads(iss.Requests(), "/jwks")
	c, err := a.Verifier.Verify(mint(t, key, localURL, "alice", "lux", time.Hour))
	if err != nil || c.Subject != localURL+"|alice" {
		t.Fatalf("the local token: %+v, %v", c, err)
	}
	if after := reads(iss.Requests(), "/jwks"); after != before {
		t.Errorf("the local token read the listed issuer's key set %d time(s)", after-before)
	}
	c, err = a.Verifier.Verify(iss.Mint(issuertest.Claims{Sub: "bob"}))
	if err != nil || c.Subject != iss.URL()+"|bob" {
		t.Fatalf("the listed issuer's token: %+v, %v", c, err)
	}
	_, err = a.Verifier.Verify(mint(t, key, iss.URL(), "alice", "lux", time.Hour))
	unauthenticated(t, err, "issuer "+iss.URL()+" token: "+jwt.ErrUnknownKey.Error())
	_, err = a.Verifier.Verify(forger.Mint(issuertest.Claims{Sub: "alice"}))
	unauthenticated(t, err, "issuer "+localURL+" token: "+jwt.ErrUnknownKey.Error())
	if got := a.String(); !strings.HasPrefix(got, "identity: issuers "+iss.URL()+"; local issuer "+localURL+" (ES256 key "+key.ID()+"); audience lux; ") {
		t.Errorf("String() = %q", got)
	}
}

// TestLocalIssuerIsOffByDefault is the fourth criterion: with the
// variable unset no local issuer is configured on the verifier, and a
// token naming LUX_PUBLIC_URL as its issuer is unauthenticated, whatever
// key signed it.
func TestLocalIssuerIsOffByDefault(t *testing.T) {
	iss := newIssuer(t)
	_, key := localKey(t)
	a, err := Startup(t.Context(), load(t, map[string]string{"LUX_OIDC_ISSUERS": iss.URL()}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Verifier.LocalIssuer() != "" || a.Verifier.DescribeLocal() != "" {
		t.Fatalf("a local issuer is configured: %q", a.Verifier.DescribeLocal())
	}
	if _, has := a.Verifier.validators[localURL]; has {
		t.Fatal("the verifier holds a validator under LUX_PUBLIC_URL")
	}
	_, err = a.Verifier.Verify(mint(t, key, localURL, "alice", "lux", time.Hour))
	unauthenticated(t, err, `the token's issuer "`+localURL+`" is not in LUX_OIDC_ISSUERS`)
	if strings.Contains(a.String(), "local issuer") {
		t.Errorf("String() = %q names a local issuer", a.String())
	}
}

// TestLocalIssuerRotation is the eighth criterion: the previous key keeps
// verifying the tokens it signed while LUX_LOCAL_ISSUER_KEYS lists it,
// beside the new key's, and they stop verifying once it is dropped. The
// key id in each token's header is what selects the key, across the two
// families: the previous key here is RSA and the new one P-256.
func TestLocalIssuerRotation(t *testing.T) {
	previousText, previous := rsaLocalKey(t)
	currentText, current := localKey(t)
	oldToken := mint(t, previous, localURL, "alice", "lux", time.Hour)
	newToken := mint(t, current, localURL, "alice", "lux", time.Hour)

	during, err := Startup(t.Context(), load(t, map[string]string{
		"LUX_LOCAL_ISSUER_KEY": currentText, "LUX_LOCAL_ISSUER_KEYS": previousText,
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"the previous key's token": oldToken, "the new key's token": newToken} {
		if _, err := during.Verifier.Verify(token); err != nil {
			t.Errorf("during the rotation, %s: %v", name, err)
		}
	}
	if want := localURL + " (ES256 key " + current.ID() + ", RS256 key " + previous.ID() + ")"; during.Verifier.DescribeLocal() != want {
		t.Errorf("DescribeLocal() = %q, want %q", during.Verifier.DescribeLocal(), want)
	}

	after, err := Startup(t.Context(), load(t, map[string]string{"LUX_LOCAL_ISSUER_KEY": currentText}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.Verifier.Verify(newToken); err != nil {
		t.Errorf("after the rotation, the new key's token: %v", err)
	}
	_, err = after.Verifier.Verify(oldToken)
	unauthenticated(t, err, jwt.ErrUnknownKey.Error())
}

// TestLocalIssuerAloneIsNotTheFileMode: a local key with no listed issuer
// builds a verifier and the owner policy, beside a manifest directory
// too, and the start-up line names the local issuer's algorithm and key
// id; only no issuer of either kind is the file mode.
func TestLocalIssuerAloneIsNotTheFileMode(t *testing.T) {
	text, key := localKey(t)
	for name, m := range map[string]map[string]string{
		"the memory store":          {"LUX_LOCAL_ISSUER_KEY": text},
		"the file mode's directory": {"LUX_LOCAL_ISSUER_KEY": text, "LUX_MANIFEST_DIR": t.TempDir()},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := Startup(t.Context(), load(t, m), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if a.Policy != PolicyOwner || a.Verifier == nil || a.Authorizer(objectsOf()) == nil {
				t.Fatalf("auth = %+v", a)
			}
			want := "identity: no listed issuer; local issuer " + localURL + " (ES256 key " + key.ID() + "); audience lux; owner policy with 0 admin subject(s)"
			if got := a.String(); got != want {
				t.Errorf("String() = %q\nwant %q", got, want)
			}
		})
	}
}

// TestVerifierRefusesAnIncompleteLocalIssuer: no issuer of either kind, a
// local name without a key, keys without a name, and a listed issuer
// carrying the local issuer's name each refuse to build a verifier.
func TestVerifierRefusesAnIncompleteLocalIssuer(t *testing.T) {
	_, key := localKey(t)
	keys := []jwt.LocalKey{{KeyID: key.ID(), Key: key.Public()}}
	iss := newIssuer(t)
	for _, tc := range []struct {
		name string
		o    VerifierOptions
		want string
	}{
		{"no issuer of either kind", VerifierOptions{}, "LUX_OIDC_ISSUERS: no issuer to verify against, and LUX_LOCAL_ISSUER_KEY sets no local one"},
		{"a local name without a key", VerifierOptions{LocalIssuer: localURL}, "LUX_LOCAL_ISSUER_KEY: the local issuer " + localURL + " has no key"},
		{"keys without a name", VerifierOptions{Issuers: []string{iss.URL()}, LocalKeys: keys}, "LUX_PUBLIC_URL: the local issuer has keys and no name"},
		{"a listed issuer with the local name", VerifierOptions{Issuers: []string{iss.URL(), localURL + "/"}, LocalIssuer: localURL, LocalKeys: keys},
			"LUX_OIDC_ISSUERS: issuer " + localURL + " is LUX_PUBLIC_URL, the local issuer's name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.Audiences = []string{audience}
			if _, err := NewVerifier(t.Context(), tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewVerifier() = %v, want %q", err, tc.want)
			}
		})
	}
}
