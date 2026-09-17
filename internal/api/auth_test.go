// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
)

// TestAudienceAtTheDoor is the family's audience rule on a route rather
// than on the verifier: internal/auth's TestConformance proves what the
// verifier decides, and this proves the decision is what /v1 answers. A
// token addressed to LUX_OIDC_AUDIENCE opens a protected route, and a
// token the same issuer signed for itself, for another service, or for
// nobody is unauthenticated at the same door.
func TestAudienceAtTheDoor(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, tc := range []struct {
		name   string
		claims issuertest.Claims
		code   int
	}{
		{"the gateway", issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}}, http.StatusOK},
		{"the gateway among others", issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"another-service", audience}}, http.StatusOK},
		{"the issuer", issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{h.iss.URL()}}, http.StatusUnauthorized},
		{"another service", issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"another-service"}}, http.StatusUnauthorized},
		{"no audience", issuertest.Claims{Sub: "alice", Omit: []string{"aud"}}, http.StatusUnauthorized},
		{"no subject", issuertest.Claims{Aud: issuertest.StringList{audience}, Omit: []string{"sub"}}, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.request(http.MethodGet, "/v1/keys", "", as(h.iss.Mint(tc.claims))...)
			if rec.Code != tc.code {
				t.Fatalf("GET /v1/keys: %d, want %d\n%s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.code == http.StatusUnauthorized {
				wantCode(t, rec, CodeUnauthenticated)
			}
		})
	}
}
