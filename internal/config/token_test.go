// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TestLoadTokenReadsItsFourVariables is the token role's configuration:
// the public address, the key, the audiences, and the admin subjects,
// through Load's own parsers, and nothing else, so neither the key
// encryption key nor a database is required or read, and an unset key is
// no problem here because the role names it as a usage error.
func TestLoadTokenReadsItsFourVariables(t *testing.T) {
	key, kid := pemKey(t)
	tok, err := LoadToken(env(map[string]string{
		"LUX_LOCAL_ISSUER_KEY": key,
		"LUX_OIDC_AUDIENCE":    "lux, api.example.com",
		"LUX_ADMIN_SUBJECTS":   publicURLValue + "|root",
		// Values Load would refuse, which the role never reads.
		"LUX_SECRETS_KEK":  "not-a-key",
		"LUX_DB_URL":       "mysql://nowhere",
		"LUX_PUBLIC_ADDR":  "nope",
		"LUX_OIDC_ISSUERS": publicURLValue,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if tok.PublicURL.String() != publicURLValue || tok.LocalIssuerKey == nil || tok.LocalIssuerKey.ID() != kid ||
		strings.Join(tok.OIDCAudiences, ",") != "lux,api.example.com" || strings.Join(tok.AdminSubjects, ",") != publicURLValue+"|root" {
		t.Fatalf("LoadToken() = %+v", tok)
	}
	unset, err := LoadToken(env(nil))
	if err != nil || unset.LocalIssuerKey != nil || strings.Join(unset.OIDCAudiences, ",") != DefaultOIDCAudience {
		t.Fatalf("LoadToken() with no key = %+v, %v", unset, err)
	}
	_, err = LoadToken(env(map[string]string{
		"LUX_PUBLIC_URL":       "",
		"LUX_LOCAL_ISSUER_KEY": "sk-not-a-key",
		"LUX_OIDC_AUDIENCE":    "lux,,",
		"LUX_ADMIN_SUBJECTS":   "root",
	}))
	if err == nil {
		t.Fatal("LoadToken() accepted four problems")
	}
	got := err.Error()
	for _, want := range []string{
		`LUX_ADMIN_SUBJECTS entry "root" is not a rendered subject`,
		"LUX_LOCAL_ISSUER_KEY is not a PEM encoded private key",
		"LUX_OIDC_AUDIENCE is",
		"LUX_PUBLIC_URL is unset",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("LoadToken() = %q, want it to contain %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "configuration: LUX_ADMIN_SUBJECTS") {
		t.Errorf("the problems are not sorted by variable: %q", got)
	}
}
