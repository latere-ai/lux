// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"reflect"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/localissuer"
	"latere.ai/x/lux/internal/serve"
)

// TestWellKnownListsTheLocalIssuer is spec 035 at the discovery
// document: the local issuer is an issuer this server accepts, so its
// name, LUX_PUBLIC_URL, follows the listed issuers in issuers, and alone
// it is the one entry of a server that is not in the file mode. A token
// it signed is accepted on /v1 and GET /v1/self renders the caller as
// <LUX_PUBLIC_URL>|<sub>.
func TestWellKnownListsTheLocalIssuer(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	key, err := localissuer.Parse(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.Mint(localissuer.Claims{Issuer: publicURL, Subject: "root", Audience: audience, IssuedAt: time.Now(), TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		listed bool
	}{
		{"beside a listed issuer", true},
		{"alone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want []any
			h := newHarness(t, func(o *Options) {
				opts := auth.Options{
					LocalIssuer: publicURL, LocalKeys: []jwt.LocalKey{{KeyID: key.ID(), Key: key.Public()}},
					Audiences: []string{audience}, HTTP: &http.Client{},
				}
				if tc.listed {
					opts.Issuers = o.Auth.Verifier.Issuers()
					want = append(want, opts.Issuers[0])
				}
				a, err := auth.New(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				o.Auth, o.Authorizer = a, a.Authorizer(&serve.ObjectOwners{Objects: o.Store.Objects()})
			})
			want = append(want, publicURL)
			doc := body(t, h.request(http.MethodGet, "/.well-known/lux", "", "Authorization", ""))
			if !reflect.DeepEqual(doc["issuers"], want) || doc["mode"] != "server" {
				t.Fatalf("issuers %v, mode %v; want issuers %v in the server mode", doc["issuers"], doc["mode"], want)
			}
			rec := h.request(http.MethodGet, "/v1/self", "", "Authorization", "Bearer "+token)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/self: %d %s", rec.Code, rec.Body.String())
			}
			if self := body(t, rec); self["subject"] != publicURL+"|root" || self["issuer"] != publicURL || self["policy"] != "owner" {
				t.Fatalf("self %v", self)
			}
		})
	}
}
