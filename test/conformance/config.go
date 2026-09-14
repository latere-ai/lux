// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
)

// Config is what Run needs to drive one server: where it is, how to get a
// token, and where the stubs are.
type Config struct {
	// URL is the base the doors and /v1 hang off, the value of
	// LUX_PUBLIC_URL on the server under test.
	URL string
	// Token returns an issuer token for a rendered subject, issuer and
	// sub joined by a bar, and false when this deployment cannot mint
	// one; a case that needs a subject it cannot get skips with the
	// subject named. A caller that can mint returns a token per subject;
	// a caller holding one static token returns it for Subject and false
	// for every other name. Nil is a run without a bearer, which keeps
	// the cases that need none.
	Token func(subject string) (string, bool)
	// Subject is who Token's default token speaks for, the value the
	// owner assertions expect.
	Subject string
	// StubsURL is the address of a lux-stubs instance the server's
	// Providers can reach, whose GET / names each stub's URL. Empty skips
	// the cases in the stub table and prints the list.
	StubsURL string
	// InternalURL is the server's internal listener, read in the file
	// mode alone, where /v1 is mounted there and not under URL. Empty in
	// the file mode skips the api group naming the variable.
	InternalURL string
}

// The variables TestContract reads. They are the suite's and are read by
// no server.
const (
	EnvURL         = "LUX_TEST_URL"
	EnvToken       = "LUX_TEST_TOKEN"
	EnvSubject     = "LUX_TEST_SUBJECT"
	EnvIssuerURL   = "LUX_TEST_ISSUER_URL"
	EnvStubsURL    = "LUX_TEST_STUBS_URL"
	EnvInternalURL = "LUX_TEST_INTERNAL_URL"
)

// fromEnv builds the Config TestContract runs with. skip is the one line
// the suite skips with when LUX_TEST_URL is unset, and empty otherwise.
func fromEnv(getenv func(string) string) (Config, string) {
	url := strings.TrimRight(strings.TrimSpace(getenv(EnvURL)), "/")
	if url == "" {
		return Config{}, EnvURL + " is unset, so there is no server to run the suite against"
	}
	cfg := Config{
		URL:         url,
		StubsURL:    strings.TrimRight(strings.TrimSpace(getenv(EnvStubsURL)), "/"),
		InternalURL: strings.TrimRight(strings.TrimSpace(getenv(EnvInternalURL)), "/"),
	}
	token := strings.TrimSpace(getenv(EnvToken))
	if token == "" {
		return cfg, ""
	}
	claims := payloadOf(token)
	cfg.Subject = strings.TrimSpace(getenv(EnvSubject))
	if cfg.Subject == "" {
		cfg.Subject = authz.Subject(claims.iss, claims.sub)
	}
	cfg.Token = tokenSource(token, cfg.Subject, claims, strings.TrimRight(strings.TrimSpace(getenv(EnvIssuerURL)), "/"))
	return cfg, ""
}

// tokenClaims are the three claims the suite reads off the token it was
// given: who signed it, whom it speaks for, and which audience it
// carries, so a token minted for another subject carries the same.
type tokenClaims struct {
	iss, sub string
	aud      []string
}

// payloadOf reads a token's claims through the shared reader without
// verifying it; the suite holds no key and the server is the verifier.
// A token that is not a JWS yields empty claims.
func payloadOf(token string) tokenClaims {
	var p struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Aud any    `json:"aud"`
	}
	if jwt.DecodePayload(token, &p) != nil {
		return tokenClaims{}
	}
	c := tokenClaims{iss: p.Iss, sub: p.Sub}
	switch a := p.Aud.(type) {
	case string:
		c.aud = []string{a}
	case []any:
		for _, v := range a {
			if s, ok := v.(string); ok {
				c.aud = append(c.aud, s)
			}
		}
	}
	return c
}

// tokenSource is the Token of a Config built from the environment: the
// given token for the default subject; through the stub issuer's POST
// /mint for any other subject of that issuer when LUX_TEST_ISSUER_URL is
// set; and nothing otherwise.
func tokenSource(token, subject string, claims tokenClaims, issuerURL string) func(string) (string, bool) {
	client := newHTTPClient()
	return func(s string) (string, bool) {
		if s == subject {
			return token, true
		}
		if issuerURL == "" {
			return "", false
		}
		iss, sub, ok := authz.SplitSubject(s)
		if !ok || strings.TrimRight(iss, "/") != issuerURL {
			return "", false
		}
		minted, err := mint(client, issuerURL, sub, claims.aud)
		if err != nil {
			return "", false
		}
		return minted, true
	}
}

// mint asks a latere.ai/x/pkg/authkit/issuertest for a token: POST /mint
// with the subject and the audience the given token carries, so the
// server accepts both alike.
func mint(client *http.Client, issuerURL, sub string, aud []string) (string, error) {
	body, err := json.Marshal(map[string]any{"sub": sub, "aud": aud})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuerURL+"/mint", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("POST " + issuerURL + "/mint: status " + resp.Status + ": " + string(data))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return "", errors.New("POST " + issuerURL + "/mint: the answer carries no token: " + string(data))
	}
	return out.Token, nil
}

// newHTTPClient is the client the suite speaks to the server under test
// and the stubs with. It is built with a transport of its own rather than
// the process default, follows no redirect, because a redirect is not an
// answer in either plane's table, and reads a whole response within the
// timeout below; a streamed door answer is bounded by the Provider's
// timeout on the server side and by the case's own deadline here.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       60 * time.Second,
	}
}
