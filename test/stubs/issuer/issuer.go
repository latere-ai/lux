// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package issuer mounts latere.ai/x/pkg/authkit/issuertest, the stub
// issuer of the contract spec 006 adopts, with Lux's vocabulary and
// nothing else: the default audience luxd verifies, so a POST /mint
// naming a subject alone yields a token the control plane accepts, and
// the subject rendering every owner field carries. The routes, the
// signing, and the request recorder are the package's own and are
// tested there.
package issuer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/config"
)

// Audience is the aud a minted token carries when the claims name none:
// LUX_OIDC_AUDIENCE's default, so a token from POST /mint {"sub": "dev"}
// verifies at a luxd started with the defaults.
const Audience = config.DefaultOIDCAudience

// NewHandler builds the stub for a binary serving its own address: url is
// the issuer the other processes reach it at, es256 selects ES256 over
// the default RS256, the two algorithms luxd accepts.
func NewHandler(url string, es256 bool) *issuertest.Server {
	return issuertest.NewHandler(options(es256, issuertest.WithIssuer(url))...)
}

// New starts the stub on a loopback listener for the test and closes it
// with the test.
func New(t testing.TB, es256 bool) *issuertest.Server {
	t.Helper()
	return issuertest.New(t, options(es256)...)
}

func options(es256 bool, extra ...issuertest.Option) []issuertest.Option {
	opts := append([]issuertest.Option{issuertest.WithDefaultAudience(Audience)}, extra...)
	if es256 {
		opts = append(opts, issuertest.WithES256())
	}
	return opts
}

// Subject renders the control plane subject a token from the issuer at
// url with sub becomes: the string every status.owner carries.
func Subject(url, sub string) string { return authz.Subject(url, sub) }

// Mint asks the issuer at url for a token through POST /mint, the way a
// tier in another process does; claims is the body. It is the one HTTP
// call this package makes, so a test that starts lux-stubs has no
// second reading of the route.
func Mint(ctx context.Context, client *http.Client, url string, claims issuertest.Claims) (string, error) {
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/mint", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s/mint: status %d: %s", url, resp.StatusCode, bytes.TrimSpace(data))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return "", errors.New("POST " + url + "/mint: the answer carries no token")
	}
	return out.Token, nil
}
