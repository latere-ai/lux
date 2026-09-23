// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/localissuer"
)

// issuerKey is a fresh P-256 key as LUX_LOCAL_ISSUER_KEY carries it,
// with its key id.
func issuerKey(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	id, err := localissuer.KeyID(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), id
}

// TestCheckReportsTheLocalIssuer is spec 035's local issuer row: ok with
// the key's algorithm and key id when LUX_LOCAL_ISSUER_KEY is set and
// parses, fail naming the variable when it or a rotation entry does not,
// whether or not the rest of the configuration loaded, and never warn;
// the row follows the issuers row. With a local key and no listed issuer
// the issuers row names the local issuer and the authorizer row probes
// the endpoint, since the control plane has callers to authorize.
func TestCheckReportsTheLocalIssuer(t *testing.T) {
	s := newStack(t)
	key, kid := issuerKey(t)
	older, _ := issuerKey(t)
	for _, tc := range []struct {
		name  string
		edit  func(map[string]string)
		state State
		want  string // a fragment of the row's sentence
	}{
		{"a key beside a listed issuer", func(m map[string]string) { m["LUX_LOCAL_ISSUER_KEY"] = key },
			OK, "ES256 key " + kid + " signs tokens issued as https://lux.example.com, and 0 further key(s) of LUX_LOCAL_ISSUER_KEYS verify"},
		{"a key and a rotation", func(m map[string]string) { m["LUX_LOCAL_ISSUER_KEY"], m["LUX_LOCAL_ISSUER_KEYS"] = key, older },
			OK, "ES256 key " + kid + " signs tokens issued as https://lux.example.com, and 1 further key(s) of LUX_LOCAL_ISSUER_KEYS verify"},
		{"a key that is not a private key", func(m map[string]string) { m["LUX_LOCAL_ISSUER_KEY"] = "sk-not-a-key" },
			Fail, "LUX_LOCAL_ISSUER_KEY is not a PEM encoded private key"},
		{"a rotation entry that is not a private key", func(m map[string]string) {
			m["LUX_LOCAL_ISSUER_KEY"], m["LUX_LOCAL_ISSUER_KEYS"] = key, older+",sk-not-a-key"
		}, Fail, "LUX_LOCAL_ISSUER_KEYS entry 2 is not a PEM encoded private key"},
		{"a good key in a configuration that did not load", func(m map[string]string) {
			m["LUX_LOCAL_ISSUER_KEY"], m["LUX_PUBLIC_ADDR"] = key, "nonsense"
		}, OK, "ES256 key " + kid + " signs, and 0 further key(s) of LUX_LOCAL_ISSUER_KEYS verify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := s.serverEnv()
			tc.edit(m)
			lines := Lines(t.Context(), Options{Getenv: env(m)})
			var names []string
			for _, l := range lines {
				names = append(names, l.Name)
			}
			if !slices.Equal(names, Names) {
				t.Fatalf("the lines are %v, want every row %v", names, Names)
			}
			l := byName(lines)["local issuer"]
			if l.State != tc.state || !strings.Contains(l.Detail, tc.want) {
				t.Errorf("local issuer: %s, want %s containing %q", l, tc.state, tc.want)
			}
			if body := strings.Split(key, "\n")[1]; strings.Contains(l.Detail, body) {
				t.Errorf("the row prints the key: %s", l)
			}
		})
	}

	t.Run("a key alone", func(t *testing.T) {
		m := s.serverEnv()
		m["LUX_OIDC_ISSUERS"], m["LUX_LOCAL_ISSUER_KEY"] = "", key
		lines := byName(Lines(t.Context(), Options{Getenv: env(m)}))
		if l := lines["issuers"]; l.State != OK || l.Detail != "none listed; the local issuer https://lux.example.com is the one issuer" {
			t.Errorf("issuers: %s", l)
		}
		if l := lines["authorizer"]; l.State != OK || !strings.HasPrefix(l.Detail, s.authz.URL()+" denies the probe") {
			t.Errorf("authorizer: %s, want the endpoint probed", l)
		}
		if l := lines["local issuer"]; l.State != OK {
			t.Errorf("local issuer: %s", l)
		}
	})
}
