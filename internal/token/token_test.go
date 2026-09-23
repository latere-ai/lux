// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"maps"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/jwt"

	"latere.ai/x/lux/internal/localissuer"
)

const publicURL = "https://lux.example.com"

// TestMint is the role's decision table: the defaults and the flags
// resolve to the token's sub, aud, and lifetime, the minted token
// verifies under the key it names with iss LUX_PUBLIC_URL, and each
// refusal is a usage error or a configuration problem as luxd's exit code
// needs to tell them apart.
func TestMint(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	text := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	key, err := localissuer.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"LUX_PUBLIC_URL":       publicURL + "/",
		"LUX_LOCAL_ISSUER_KEY": text,
		"LUX_OIDC_AUDIENCE":    "lux,api.example.com",
		"LUX_ADMIN_SUBJECTS":   "https://login.example.com|alice," + publicURL + "|root",
	}
	with := func(edits map[string]string) func(string) string {
		m := maps.Clone(base)
		maps.Copy(m, edits)
		return func(k string) string { return m[k] }
	}
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		o     Options
		sub   string // the accepted token's sub, or "" for a refusal
		aud   string
		usage bool   // the refusal is a UsageError
		want  string // a fragment of the refusal
	}{
		{name: "the defaults", o: Options{Getenv: with(nil), TTL: DefaultTTL}, sub: "root", aud: "lux"},
		{name: "every flag", o: Options{Getenv: with(nil), Subject: "ops", Audience: "api.example.com", TTL: MaxTTL}, sub: "ops", aud: "api.example.com"},
		{name: "a TTL above the cap", o: Options{Getenv: with(nil), TTL: MaxTTL + time.Second}, usage: true, want: "--ttl is 24h0m1s"},
		{name: "a zero TTL", o: Options{Getenv: with(nil)}, usage: true, want: "--ttl is 0s"},
		{name: "an unset key", o: Options{Getenv: with(map[string]string{"LUX_LOCAL_ISSUER_KEY": ""}), TTL: DefaultTTL}, usage: true, want: "LUX_LOCAL_ISSUER_KEY is unset"},
		{name: "no default subject", o: Options{Getenv: with(map[string]string{"LUX_ADMIN_SUBJECTS": ""}), TTL: DefaultTTL}, usage: true, want: "--subject is required, since LUX_ADMIN_SUBJECTS names 0 subject(s) of the local issuer " + publicURL},
		{name: "a subject with the separator", o: Options{Getenv: with(nil), Subject: "x|y", TTL: DefaultTTL}, usage: true, want: `--subject is "x|y"`},
		{name: "a subject with surrounding space", o: Options{Getenv: with(nil), Subject: " root", TTL: DefaultTTL}, usage: true, want: `--subject is " root"`},
		{name: "an unlisted audience", o: Options{Getenv: with(nil), Audience: "other", TTL: DefaultTTL}, usage: true, want: `--audience is "other"`},
		{name: "a configuration problem", o: Options{Getenv: with(map[string]string{"LUX_PUBLIC_URL": ""}), TTL: DefaultTTL}, want: "configuration: LUX_PUBLIC_URL is unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.Now = func() time.Time { return now }
			token, err := Mint(tc.o)
			if tc.sub == "" {
				if err == nil || IsUsage(err) != tc.usage || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("Mint() = %q, %v; want a refusal containing %q, usage %v", token, err, tc.want, tc.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			v := jwt.New(jwt.Config{LocalIssuer: publicURL, LocalKeys: []jwt.LocalKey{{KeyID: key.ID(), Key: key.Public()}}, Audiences: []string{tc.aud}, Now: func() time.Time { return now }})
			c, err := v.Validate(token)
			if err != nil {
				t.Fatalf("the minted token does not verify: %v", err)
			}
			if c.Iss != publicURL || c.Sub != tc.sub || !c.Exp.Equal(now.Add(tc.o.TTL)) {
				t.Errorf("claims = %+v, want sub %s expiring %s", c, tc.sub, now.Add(tc.o.TTL))
			}
		})
	}
}
