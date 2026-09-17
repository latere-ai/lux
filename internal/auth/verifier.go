// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"
	"latere.ai/x/pkg/otel"
)

// Caller is a verified control plane caller: the rendered subject, its
// two halves apart, and every claim of the token verbatim, which the
// authorizer reads and this package does not.
type Caller struct {
	// Subject is authz.Subject(Issuer, Sub), the string every owner field
	// and every authorizer request carries.
	Subject string
	// Issuer is the token's iss without its trailing slash; Sub is its sub.
	Issuer string
	Sub    string
	// Claims is the token's payload as it was signed.
	Claims map[string]any
}

// VerifierOptions configures a Verifier.
type VerifierOptions struct {
	// Issuers are the issuer URLs whose tokens are accepted, each fetched
	// at start. At least one is required.
	Issuers []string
	// Audience is the one value a token's aud must contain.
	Audience string
	// HTTP fetches discovery and the key sets, at start and on a refresh.
	// Optional: the default is an instrumented client with a ten second
	// timeout.
	HTTP *http.Client
	// CacheTTL is how long a fetched key set is served before a refresh;
	// zero is the verifier package's default.
	CacheTTL time.Duration
}

// Verifier accepts a bearer signed by any listed issuer: one validator per
// issuer, each holding that issuer's key set and refreshing it on its own
// schedule, so a request path never waits on an issuer that is up and
// keeps working against one that has gone away.
type Verifier struct {
	audience   string
	issuers    []string
	validators map[string]*jwt.Validator
}

// The bound of one discovery document or key set.
const maxDocumentBytes = 1 << 20

// discovery is the part of an OpenID configuration the verifier reads.
type discovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// jwk is the part of one key the start-up check reads: enough to know
// whether the shared verifier will use it.
type jwk struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// usable reports whether the shared verifier will check a signature with
// this key: an RSA key, which answers RS256, or a P-256 key, which
// answers ES256, each with its coordinates and with alg absent or the one
// it answers.
func (k jwk) usable() bool {
	switch k.Kty {
	case "RSA":
		return k.N != "" && k.E != "" && (k.Alg == "" || k.Alg == "RS256")
	case "EC":
		return k.Crv == "P-256" && k.X != "" && k.Y != "" && (k.Alg == "" || k.Alg == "ES256")
	}
	return false
}

// NewVerifier fetches each issuer's discovery document and key set and
// refuses any issuer that is unreachable, whose document names another
// issuer, or whose key set has no RS256 or ES256 key, so a gateway never
// starts trusting an issuer it cannot verify against. The error names the
// issuer and what was read.
//
// Each issuer's validator is left warm, holding the key set it will
// verify against, so the fetch is paid for at start-up and not by the
// first request.
func NewVerifier(ctx context.Context, o VerifierOptions) (*Verifier, error) {
	if len(o.Issuers) == 0 {
		return nil, errors.New("LUX_OIDC_ISSUERS: no issuer to verify against")
	}
	if o.Audience == "" {
		return nil, errors.New("LUX_OIDC_AUDIENCE: no audience to verify")
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, Transport: otel.Transport(nil)}
	}
	v := &Verifier{audience: o.Audience, validators: make(map[string]*jwt.Validator, len(o.Issuers))}
	for _, raw := range o.Issuers {
		iss := strings.TrimRight(raw, "/")
		if _, dup := v.validators[iss]; dup {
			return nil, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s is listed twice", iss)
		}
		doc, err := fetchIssuer(ctx, client, iss)
		if err != nil {
			return nil, err
		}
		v.issuers = append(v.issuers, iss)
		validator := jwt.New(jwt.Config{
			JWKSURL:   doc.JWKSURI,
			Issuer:    doc.Issuer,
			Audiences: []string{o.Audience},
			CacheTTL:  o.CacheTTL,
			// A personal access token carries the grants its holder chose
			// as RFC 9396's authorization_details (spec 006). This gateway
			// reads them: the claim reaches the authorizer in
			// Caller.Claims, the owner policy intersects its own answer
			// with it, and an operator's endpoint gets it forwarded
			// verbatim. The flag is that promise, and a validator that has
			// not made it refuses such a token outright rather than
			// granting more than the person asked for.
			ReadsGrants: true,
			HTTPClient:  client,
		})
		// The check above read this issuer's key set with its own client,
		// which left the validator that verifies the tokens holding
		// nothing: the first request of the day paid for the fetch a
		// second time. Warm reads the set into the validator once. A
		// failure here is the issuer this gateway was just told to trust
		// answering nothing, which is the start-up refusal above.
		if err := validator.Warm(ctx); err != nil {
			return nil, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s: key set: %w", iss, err)
		}
		v.validators[iss] = validator
	}
	return v, nil
}

// fetchIssuer reads an issuer's discovery document and its key set, and
// returns the document when the key set has a usable key.
func fetchIssuer(ctx context.Context, client *http.Client, iss string) (discovery, error) {
	var doc discovery
	if err := fetchJSON(ctx, client, iss+"/.well-known/openid-configuration", &doc); err != nil {
		return doc, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s: discovery: %w", iss, err)
	}
	if strings.TrimRight(doc.Issuer, "/") != iss {
		return doc, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s: the discovery document names issuer %q, so tokens it signs would carry another iss", iss, doc.Issuer)
	}
	if doc.JWKSURI == "" {
		return doc, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s: the discovery document names no jwks_uri", iss)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := fetchJSON(ctx, client, doc.JWKSURI, &set); err != nil {
		return doc, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s: key set: %w", iss, err)
	}
	usable := 0
	for _, k := range set.Keys {
		if k.usable() {
			usable++
		}
	}
	if usable == 0 {
		return doc, fmt.Errorf("LUX_OIDC_ISSUERS: issuer %s: the key set at %s has %d key(s) and none is an RS256 or ES256 key", iss, doc.JWKSURI, len(set.Keys))
	}
	return doc, nil
}

// fetchJSON reads one document into v: a 200 whose body is JSON, under
// the size bound.
func fetchJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return fmt.Errorf("GET %s: reading the body: %w", url, err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: the body is not JSON: %w", url, err)
	}
	return nil
}

// Issuers lists the issuers in the order they were given, each without
// its trailing slash.
func (v *Verifier) Issuers() []string {
	out := make([]string, len(v.issuers))
	copy(out, v.issuers)
	return out
}

// Audience is the one value a token's aud must contain.
func (v *Verifier) Audience() string { return v.audience }

// Authenticate reads the request's bearer and verifies it. Every refusal
// is an *Error with code unauthenticated and the developer's finding in
// its detail: no bearer, a value that is not a JWS, an issuer that is not
// listed, or a token the issuer's validator refuses, for its signature,
// its exp, its nbf, or its aud.
func (v *Verifier) Authenticate(r *http.Request) (Caller, error) {
	token, ok := bearer.FromRequest(r)
	if !ok {
		return Caller{}, refuse(CodeUnauthenticated, "the request carries no Authorization: Bearer header")
	}
	return v.Verify(token)
}

// Verify checks one bearer value.
func (v *Verifier) Verify(token string) (Caller, error) {
	var claims map[string]any
	if err := jwt.DecodePayload(token, &claims); err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "the bearer is not a JWS a listed issuer could have signed: "+err.Error())
	}
	iss, _ := claims["iss"].(string)
	validator, ok := v.validators[strings.TrimRight(iss, "/")]
	if !ok {
		return Caller{}, refuse(CodeUnauthenticated, "the token's issuer "+strconv.Quote(iss)+" is not in LUX_OIDC_ISSUERS")
	}
	if _, has := claims["exp"]; !has {
		return Caller{}, refuse(CodeUnauthenticated, "the token has no exp claim, and a token without an expiry is not accepted")
	}
	c, err := validator.Validate(token)
	if err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "issuer "+iss+" token: "+err.Error())
	}
	issuer := strings.TrimRight(c.Iss, "/")
	return Caller{Subject: authz.Subject(issuer, c.Sub), Issuer: issuer, Sub: c.Sub, Claims: claims}, nil
}
