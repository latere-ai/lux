// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	v1 "latere.ai/x/lux/manifest/v1"
)

// audience is the gateway's audience in every test.
const audience = "lux"

// newIssuer starts a stub issuer whose tokens carry the gateway's
// audience unless a test says otherwise.
func newIssuer(t *testing.T, opts ...issuertest.Option) *issuertest.Server {
	t.Helper()
	return issuertest.New(t, append([]issuertest.Option{issuertest.WithDefaultAudience(audience)}, opts...)...)
}

// newVerifier builds a Verifier over the given issuers, failing the test
// on a start-up refusal.
func newVerifier(t *testing.T, issuers ...string) *Verifier {
	t.Helper()
	v, err := NewVerifier(t.Context(), VerifierOptions{Issuers: issuers, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// request is a GET with the given Authorization header value.
func request(authorization string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/keys", nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

// unauthenticated asserts err is an *Error with code unauthenticated whose
// detail contains want and whose message is the fixed sentence.
func unauthenticated(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeUnauthenticated {
		t.Fatalf("err = %v, want unauthenticated", err)
	}
	if e.Message != "This request needs a valid credential." {
		t.Errorf("message %q is not the fixed sentence", e.Message)
	}
	if !strings.Contains(e.Detail, want) {
		t.Errorf("detail %q lacks %q", e.Detail, want)
	}
	if strings.Contains(e.Message, e.Detail) {
		t.Errorf("the detail %q leaked into the sentence", e.Detail)
	}
}

// fakeIssuer serves a discovery document and a key set a test shapes, so
// the start-up check sees what a misconfigured issuer would serve.
type fakeIssuer struct {
	srv  *httptest.Server
	doc  func(url string) any // the discovery document; nil serves the right one
	jwks func(w http.ResponseWriter)
}

func startFakeIssuer(t *testing.T, f *fakeIssuer) string {
	t.Helper()
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		var doc any = map[string]string{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/jwks"}
		if f.doc != nil {
			doc = f.doc(f.srv.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) { f.jwks(w) })
	return f.srv.URL
}

func jsonKeys(keys ...map[string]string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}
}

// TestUnreachableIssuerIsAStartupFailure: an issuer that refuses the
// connection, one that never answers, one whose discovery is not a 200,
// one whose document names another issuer or no key set, and one whose
// key set is not JSON each refuse the start, naming the issuer.
func TestUnreachableIssuerIsAStartupFailure(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + closed.Addr().String()
	_ = closed.Close()

	hanging := newIssuer(t)
	hanging.Hang()

	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)

	otherIssuer := startFakeIssuer(t, &fakeIssuer{
		doc: func(string) any {
			return map[string]string{"issuer": "https://other.example.com", "jwks_uri": "https://other.example.com/jwks"}
		},
		jwks: jsonKeys(),
	})
	noJWKS := startFakeIssuer(t, &fakeIssuer{
		doc:  func(url string) any { return map[string]string{"issuer": url} },
		jwks: jsonKeys(),
	})
	badJWKS := startFakeIssuer(t, &fakeIssuer{
		jwks: func(w http.ResponseWriter) { _, _ = io.WriteString(w, "{not json") },
	})
	jwks404 := startFakeIssuer(t, &fakeIssuer{
		jwks: func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
	})
	badDiscovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") }))
	t.Cleanup(badDiscovery.Close)

	good := newIssuer(t)
	client := &http.Client{Timeout: 300 * time.Millisecond}
	for _, tc := range []struct {
		name, issuer, want string
	}{
		{"a refused connection", refused, "discovery: "},
		{"an issuer that is not a URL", "::not-a-url", "discovery: "},
		{"an issuer that never answers", hanging.URL(), "discovery: "},
		{"a discovery that is not a 200", notFound.URL, "answered 404"},
		{"a discovery that is not JSON", badDiscovery.URL, "is not JSON"},
		{"a document naming another issuer", otherIssuer, `names issuer "https://other.example.com"`},
		{"a document with no key set", noJWKS, "names no jwks_uri"},
		{"a key set that is not JSON", badJWKS, "key set: "},
		{"a key set that is not a 200", jwks404, "key set: GET " + jwks404 + "/jwks answered 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The good issuer comes second, so the failure is the bad one's
			// and the good one is never reached.
			_, err := NewVerifier(t.Context(), VerifierOptions{Issuers: []string{tc.issuer, good.URL()}, Audience: audience, HTTP: client})
			if err == nil {
				t.Fatal("NewVerifier accepted the issuer")
			}
			if got := err.Error(); !strings.Contains(got, "LUX_OIDC_ISSUERS: issuer "+tc.issuer+": ") || !strings.Contains(got, tc.want) {
				t.Errorf("err = %q, want it to name %s and contain %q", got, tc.issuer, tc.want)
			}
		})
	}
	t.Run("no issuer", func(t *testing.T) {
		if _, err := NewVerifier(t.Context(), VerifierOptions{Audience: audience}); err == nil {
			t.Fatal("NewVerifier built a verifier over no issuer")
		}
	})
	t.Run("no audience", func(t *testing.T) {
		if _, err := NewVerifier(t.Context(), VerifierOptions{Issuers: []string{good.URL()}}); err == nil {
			t.Fatal("NewVerifier built a verifier with no audience")
		}
	})
	t.Run("an issuer listed twice", func(t *testing.T) {
		_, err := NewVerifier(t.Context(), VerifierOptions{Issuers: []string{good.URL(), good.URL() + "/"}, Audience: audience})
		if err == nil || !strings.Contains(err.Error(), "listed twice") {
			t.Fatalf("err = %v", err)
		}
	})
}

// TestIssuerWithoutUsableKeysIsAStartupFailure: a key set of keys the
// shared verifier skips, an RSA key of another algorithm, an EC key on
// another curve, a symmetric key, a key with no coordinates, or no keys
// at all, refuses the start naming the issuer and the count; a set with
// one RS256 or one ES256 key beside them starts.
func TestIssuerWithoutUsableKeysIsAStartupFailure(t *testing.T) {
	rs512 := map[string]string{"kty": "RSA", "alg": "RS512", "kid": "a", "n": "AQAB", "e": "AQAB"}
	p384 := map[string]string{"kty": "EC", "crv": "P-384", "kid": "b", "x": "AA", "y": "AA"}
	oct := map[string]string{"kty": "oct", "kid": "c", "k": "AA"}
	bare := map[string]string{"kty": "RSA", "kid": "d"}
	rs256 := map[string]string{"kty": "RSA", "kid": "e", "n": "AQAB", "e": "AQAB"}
	es256 := map[string]string{"kty": "EC", "alg": "ES256", "crv": "P-256", "kid": "f", "x": "AA", "y": "AA"}
	for _, tc := range []struct {
		name string
		keys []map[string]string
		ok   bool
	}{
		{"no keys", nil, false},
		{"an RS512 key", []map[string]string{rs512}, false},
		{"a P-384 key", []map[string]string{p384}, false},
		{"a symmetric key", []map[string]string{oct}, false},
		{"an RSA key without coordinates", []map[string]string{bare}, false},
		{"every unusable kind", []map[string]string{rs512, p384, oct, bare}, false},
		{"an RS256 key among them", []map[string]string{rs512, rs256, oct}, true},
		{"an ES256 key among them", []map[string]string{p384, es256}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iss := startFakeIssuer(t, &fakeIssuer{jwks: jsonKeys(tc.keys...)})
			_, err := NewVerifier(t.Context(), VerifierOptions{Issuers: []string{iss}, Audience: audience})
			if tc.ok {
				if err != nil {
					t.Fatalf("NewVerifier refused a usable key set: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("NewVerifier accepted a key set with no usable key")
			}
			want := "LUX_OIDC_ISSUERS: issuer " + iss + ": the key set at " + iss + "/jwks has " + string(rune('0'+len(tc.keys))) + " key(s) and none is an RS256 or ES256 key"
			if err.Error() != want {
				t.Errorf("err = %q\nwant %q", err, want)
			}
		})
	}
	t.Run("the stub issuer under each algorithm", func(t *testing.T) {
		rs := newIssuer(t)
		es := newIssuer(t, issuertest.WithES256())
		newVerifier(t, rs.URL(), es.URL())
	})
}

// TestBearerAcceptance is spec 006's verification table: a token from a
// listed issuer with the audience is accepted under RS256 and under
// ES256, and every other row is unauthenticated with the row's finding
// in the developer detail.
func TestBearerAcceptance(t *testing.T) {
	rs := newIssuer(t)
	es := newIssuer(t, issuertest.WithES256())
	other := newIssuer(t)
	// forger signs with its own key but claims to be rs, so its tokens
	// carry a listed iss and a signature no listed key made. Which
	// refusal that earns is the header's kid: under forger's own kid
	// rs's set holds no such key and the token is refused before any
	// signature is read; under rs's kid the set holds the key the kid
	// names and the signature is what fails.
	forger := newIssuer(t, issuertest.WithIssuer(rs.URL()))
	v := newVerifier(t, rs.URL(), es.URL())
	now := time.Now()

	valid := rs.Mint(issuertest.Claims{Sub: "alice"})
	// The first character of the signature segment is flipped: every one
	// of its six bits is signature, where the last character's low bits
	// are base64url padding and a flip there can leave the bytes intact.
	sigStart := strings.LastIndexByte(valid, '.') + 1
	flipped := byte('A')
	if valid[sigStart] == 'A' {
		flipped = 'B'
	}
	tampered := valid[:sigStart] + string(flipped) + valid[sigStart+1:]

	for _, tc := range []struct {
		name    string
		header  string
		subject string // the accepted subject, or "" for a refusal
		detail  string // the refusal's finding
	}{
		{"an RS256 token from a listed issuer", "Bearer " + valid, rs.URL() + "|alice", ""},
		{"an ES256 token from a listed issuer", "Bearer " + es.Mint(issuertest.Claims{Sub: "bob"}), es.URL() + "|bob", ""},
		{"a lower-case scheme", "bearer " + valid, rs.URL() + "|alice", ""},
		{"an aud list containing the audience", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"other", audience}}), rs.URL() + "|alice", ""},
		{"no bearer", "", "", "no Authorization: Bearer header"},
		{"another scheme", "Basic YWxpY2U6cHc=", "", "no Authorization: Bearer header"},
		{"another issuer", "Bearer " + other.Mint(issuertest.Claims{Sub: "alice"}), "", `issuer "` + other.URL() + `" is not in LUX_OIDC_ISSUERS`},
		{"no issuer claim", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"iss"}}), "", `issuer "" is not in LUX_OIDC_ISSUERS`},
		{"another audience", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"other"}}), "", jwt.ErrInvalidAudience.Error()},
		{"no audience", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"aud"}}), "", jwt.ErrInvalidAudience.Error()},
		{"an expired exp", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Exp: now.Add(-time.Minute).Unix()}), "", jwt.ErrTokenExpired.Error()},
		{"no exp", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"exp"}}), "", "no exp claim"},
		{"a future nbf", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Nbf: now.Add(time.Hour).Unix()}), "", jwt.ErrTokenNotValidYet.Error()},
		{"a past nbf", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Nbf: now.Add(-time.Hour).Unix()}), rs.URL() + "|alice", ""},
		{"a signature of another key", "Bearer " + forger.Mint(issuertest.Claims{Sub: "alice", Kid: rs.KID()}), "", jwt.ErrInvalidSignature.Error()},
		{"a kid no listed key has", "Bearer " + forger.Mint(issuertest.Claims{Sub: "alice"}), "", jwt.ErrUnknownKey.Error()},
		{"a tampered signature", "Bearer " + tampered, "", jwt.ErrInvalidSignature.Error()},
		{"an unsupported algorithm", "Bearer " + rs.Mint(issuertest.Claims{Sub: "alice", Alg: "HS256"}), "", jwt.ErrUnsupportedAlg.Error()},
		{"no sub", "Bearer " + rs.Mint(issuertest.Claims{Omit: []string{"sub"}}), "", jwt.ErrMalformedToken.Error()},
		{"two segments", "Bearer " + valid[:strings.LastIndexByte(valid, '.')], "", "not a JWS"},
		{"a payload that is not JSON", "Bearer eyJhbGciOiJSUzI1NiJ9.bm90IGpzb24.c2ln", "", "not a JWS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := v.Authenticate(request(tc.header))
			if tc.subject != "" {
				if err != nil {
					t.Fatalf("Authenticate() = %v, want subject %s", err, tc.subject)
				}
				if c.Subject != tc.subject {
					t.Fatalf("subject %q, want %q", c.Subject, tc.subject)
				}
				return
			}
			if err == nil {
				t.Fatalf("Authenticate() accepted the token as %q", c.Subject)
			}
			unauthenticated(t, err, tc.detail)
		})
	}
}

// TestPlanesRefuseEachOthersCredential is the control plane's half of
// the boundary: a Key value, minted or supplied, presented as the bearer
// of /v1 is unauthenticated, because it is not a JWS a listed issuer
// signed. The doors' half, an issuer token no Key was created with, is
// spec 007's TestPlaneBoundaryHoldsByVerification.
func TestPlanesRefuseEachOthersCredential(t *testing.T) {
	v := newVerifier(t, newIssuer(t).URL())
	for _, key := range []string{
		"lux_" + strings.Repeat("a", 40),
		"sk-" + strings.Repeat("0", 48),
		strings.Repeat("x", 4096),
	} {
		_, err := v.Authenticate(request("Bearer " + key))
		unauthenticated(t, err, "not a JWS")
	}
}

// TestStaleKeySetServesUntilRefresh: once a key set has been read, an
// issuer that goes away leaves the verifier accepting tokens signed by
// the keys it holds and refusing one whose kid it never saw, so an
// outage at the issuer degrades to refusing new keys and not every
// request. The kid the cached set does not hold is an unknown key, not
// a bad signature: the refusal is decided by the kid, and the refresh
// the miss forces cannot reach the closed issuer, so the cached set
// survives the miss and the kid it does hold still verifies after it.
func TestStaleKeySetServesUntilRefresh(t *testing.T) {
	iss := newIssuer(t)
	v := newVerifier(t, iss.URL())
	known := iss.Mint(issuertest.Claims{Sub: "alice"})
	if _, err := v.Verify(known); err != nil {
		t.Fatalf("before the outage: %v", err)
	}
	iss.Close()
	if _, err := v.Verify(known); err != nil {
		t.Fatalf("during the outage, a token signed by a cached key: %v", err)
	}
	iss.Rotate()
	_, err := v.Verify(iss.Mint(issuertest.Claims{Sub: "alice"}))
	unauthenticated(t, err, jwt.ErrUnknownKey.Error())
	if _, err := v.Verify(known); err != nil {
		t.Fatalf("after the unknown kid forced a refresh that could not reach the issuer: %v", err)
	}
}

// TestSubjectRendering: the subject is <iss>|<sub> with the issuer's
// trailing slash removed however the issuer was listed, Issuer and Sub
// are its halves, and Claims is the payload as signed, every claim
// included and none interpreted.
func TestSubjectRendering(t *testing.T) {
	iss := newIssuer(t)
	v := newVerifier(t, iss.URL()+"/")
	if got := v.Issuers(); !reflect.DeepEqual(got, []string{iss.URL()}) || v.Audience() != audience {
		t.Fatalf("Issuers() = %v, Audience() = %q", got, v.Audience())
	}
	token := iss.Mint(issuertest.Claims{Sub: "alice", Extra: map[string]any{
		"email": "alice@example.com", "groups": []string{"research"}, "plan": map[string]any{"name": "team", "seats": 5},
	}})
	c, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != iss.URL()+"|alice" || c.Issuer != iss.URL() || c.Sub != "alice" {
		t.Fatalf("caller = %+v", c)
	}
	var payload map[string]any
	if err := jwt.DecodePayload(token, &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Claims, payload) {
		t.Errorf("claims %v are not the payload %v", c.Claims, payload)
	}
	for _, k := range []string{"iss", "sub", "aud", "exp", "iat", "email", "groups", "plan"} {
		if _, ok := c.Claims[k]; !ok {
			t.Errorf("claim %s was dropped", k)
		}
	}
}

// TestOwnerIsTheTokensSubject is the identity half of spec 006's owner
// row: a token minted for a person renders as <iss>|<person> and a
// service token as <iss>|<service account>, each without a trailing
// slash, so an object applied with either is owned by that subject.
// The write of owner is spec 011's.
func TestOwnerIsTheTokensSubject(t *testing.T) {
	iss := newIssuer(t)
	v := newVerifier(t, iss.URL())
	service := iss.Mint(issuertest.Claims{Sub: "svc-platform", ClientID: "platform"})
	c, err := v.Verify(service)
	if err != nil || c.Subject != iss.URL()+"|svc-platform" {
		t.Fatalf("service token: %v, %+v", err, c)
	}
	// An actor token for a person, minted the way a platform's console
	// does, for the gateway's audience.
	body, _ := json.Marshal(map[string]any{"audience": audience, "ttl_seconds": 60})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, iss.URL()+"/actor-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+iss.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{iss.URL()}}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var minted struct {
		ActorToken string `json:"actor_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil || minted.ActorToken == "" {
		t.Fatalf("POST /actor-tokens: %d %v", resp.StatusCode, err)
	}
	c, err = v.Verify(minted.ActorToken)
	if err != nil || c.Subject != iss.URL()+"|alice" {
		t.Fatalf("actor token: %v, %+v", err, c)
	}
}

// TestVerifierReadsTheGrantsAPersonalTokenCarries is what
// jwt.Config.ReadsGrants turns on. A personal access token carries the
// grants its holder chose as RFC 9396's authorization_details (spec
// 006), and the verifier accepts it and hands the claim on verbatim, so
// the decision point can intersect its answer with it.
//
// The other half is why the flag exists. A validator that does not read
// the claim refuses the token with reason grants_unread rather than
// verifying it and applying nothing: the claim says what the credential
// may not do, so a reader that ignores it grants more than the person
// asked for, silently. Setting the flag is a promise this gateway keeps
// at its owner policy and forwards to an operator's authorizer.
func TestVerifierReadsTheGrantsAPersonalTokenCarries(t *testing.T) {
	iss := newIssuer(t)
	const model = "mdl_01J9TESTMODEL00000000000000"
	token := iss.Mint(issuertest.Claims{Sub: "alice", Extra: map[string]any{
		"token_use": authz.TokenUsePAT,
		"authorization_details": []any{map[string]any{
			"type":       authz.GrantType,
			"actions":    []string{"lux:" + authorizer.ActionModelUse},
			"datatypes":  []string{v1.KindModel},
			"locations":  []string{"https://api.example.com"},
			"identifier": model,
		}},
	}})

	c, err := newVerifier(t, iss.URL()).Verify(token)
	if err != nil {
		t.Fatalf("a personal access token carrying grants was refused: %v", err)
	}
	grants, err := authz.ParseGrants(c.Claims)
	if err != nil {
		t.Fatalf("the verified caller's claims do not read as grants: %v", err)
	}
	if len(grants) != 1 || !slices.Contains(grants[0].Actions, "lux:"+authorizer.ActionModelUse) || grants[0].Identifier != model {
		t.Errorf("the caller carries %+v; the claim the token was minted with names lux:%s on %s", grants, authorizer.ActionModelUse, model)
	}

	// The same token, the same key set, a validator that promises
	// nothing: the shared library's closed answer.
	unread := jwt.New(jwt.Config{JWKSURL: iss.JWKSURL(), Issuer: iss.URL(), Audiences: []string{audience}})
	if _, err := unread.Validate(token); !errors.Is(err, jwt.ErrGrantsUnread) || jwt.ReasonOf(err) != jwt.ReasonGrantsUnread {
		t.Errorf("a validator that does not read grants answered %v; a token carrying a restriction it would ignore is refused with %s", err, jwt.ReasonGrantsUnread)
	}
}
