// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/pkg/authkit"
	"latere.ai/x/pkg/authkit/conformance"
)

// identity adapts the gateway's verifier to the family's
// authkit.Authenticator, which is the shape the shared audience suite
// speaks. Two steps and no third: the gateway's own Authenticate decides
// whether the token is admitted, and the payload it hands back is read
// into authkit.Identity by authkit's own claim names. The gateway reads
// none of those claims itself, because it renders a subject and forwards
// the payload to the authorizer verbatim (spec 006), so the mapping here
// is the family's rather than one this test invented.
type identity struct{ v *Verifier }

func (a identity) Authenticate(r *http.Request) (authkit.Identity, error) {
	c, err := a.v.Authenticate(r)
	if err != nil {
		return authkit.Identity{}, err
	}
	payload, err := json.Marshal(c.Claims)
	if err != nil {
		return authkit.Identity{}, err
	}
	var id authkit.Identity
	if err := json.Unmarshal(payload, &id); err != nil {
		return authkit.Identity{}, err
	}
	return id, nil
}

// TestConformance runs the family's rule R2 against the verifier luxd
// installs in production: a token addressed to LUX_OIDC_AUDIENCE is
// admitted and yields one Identity, a token addressed to the issuer
// itself or to another service is refused, a token that names nobody is
// refused, the issuer is called for its key set alone, and the retired
// is_superadmin flag grants no platform role. NewVerifier is the one
// constructor the node and this test share, so the shape the suite
// proves is the shape production verifies with. The suite hands the
// constructor a stub issuer, so the test needs nothing running.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Service{
		Audience: audience,
		New: func(tb testing.TB, issuerURL, _ string) authkit.Authenticator {
			v, err := NewVerifier(tb.Context(), VerifierOptions{Issuers: []string{issuerURL}, Audiences: []string{audience}})
			if err != nil {
				tb.Fatal(err)
			}
			return identity{v}
		},
	})
}
