// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/localissuer"
)

// pemKey is a fresh P-256 key as the PKCS#8 PEM text an operator sets,
// with its key id.
func pemKey(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return encodeKey(t, k), keyID(t, k.Public())
}

func encodeKey(t *testing.T, k crypto.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func keyID(t *testing.T, pub crypto.PublicKey) string {
	t.Helper()
	id, err := localissuer.KeyID(pub)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestLocalIssuerRules is spec 035's configuration of the local issuer: a
// key alone satisfies the issuer rule, whose sentence names all three
// ways out; a value that is not a usable PKCS#8 private key is a problem
// naming its variable and echoing nothing; LUX_OIDC_ISSUERS may not list
// LUX_PUBLIC_URL while a key is set, however the address is spelled; the
// rotation list requires the key and repeats no key id.
func TestLocalIssuerRules(t *testing.T) {
	key, kid := pemKey(t)
	older, olderID := pemKey(t)
	oldest, oldestID := pemKey(t)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(p384)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := func(s string) string { return strings.TrimSpace(s) }
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string   // a fragment of the problem, or "" for a configuration that loads
		not  string   // a fragment no problem may carry
		ids  []string // the key ids loaded, the signing key first
	}{
		{"a key alone", map[string]string{"LUX_LOCAL_ISSUER_KEY": key}, "", "", []string{kid}},
		{"a key beside a listed issuer", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_OIDC_ISSUERS": issuer}, "", "", []string{kid}},
		{"a key beside the file mode", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_MANIFEST_DIR": t.TempDir()}, "", "", []string{kid}},
		{"a rotation list with a comma", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_LOCAL_ISSUER_KEYS": trimmed(older) + "," + trimmed(oldest)}, "", "", []string{kid, olderID, oldestID}},
		{"a rotation list one block per line", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_LOCAL_ISSUER_KEYS": older + oldest}, "", "", []string{kid, olderID, oldestID}},
		{"a blank rotation list", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_LOCAL_ISSUER_KEYS": " "}, "", "", []string{kid}},
		{"the public address listed while no key is set", map[string]string{"LUX_OIDC_ISSUERS": publicURLValue}, "", "", nil},
		{"no issuer, no key, and no directory", map[string]string{"LUX_LOCAL_ISSUER_KEY": " "},
			"LUX_OIDC_ISSUERS is unset, and the control plane needs an issuer to verify a bearer against unless LUX_LOCAL_ISSUER_KEY sets a local one or LUX_MANIFEST_DIR selects the file mode", "", nil},
		{"a key that is not PEM", map[string]string{"LUX_LOCAL_ISSUER_KEY": "sk-not-a-key"},
			"LUX_LOCAL_ISSUER_KEY is not a PEM encoded private key (the value is not echoed)", "LUX_OIDC_ISSUERS is unset", nil},
		{"a SEC 1 key", map[string]string{"LUX_LOCAL_ISSUER_KEY": string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}))},
			`LUX_LOCAL_ISSUER_KEY holds a PEM block of type "EC PRIVATE KEY"`, "", nil},
		{"a key on P-384", map[string]string{"LUX_LOCAL_ISSUER_KEY": encodeKey(t, p384)},
			"LUX_LOCAL_ISSUER_KEY holds an ECDSA key on P-384, and an ECDSA key of the local issuer is on P-256", "", nil},
		{"two keys in the one variable", map[string]string{"LUX_LOCAL_ISSUER_KEY": key + older},
			"LUX_LOCAL_ISSUER_KEY holds more than one PEM block", "", nil},
		{"the public address listed", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_OIDC_ISSUERS": issuer + "," + publicURLValue},
			"LUX_OIDC_ISSUERS lists LUX_PUBLIC_URL " + publicURLValue + " while LUX_LOCAL_ISSUER_KEY is set, and that address is the local issuer's name, which no listed issuer may carry", "", nil},
		{"the public address listed with its trailing slash", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_OIDC_ISSUERS": publicURLValue + "/"},
			"LUX_OIDC_ISSUERS lists LUX_PUBLIC_URL " + publicURLValue + " while LUX_LOCAL_ISSUER_KEY is set", "", nil},
		{"the public address under a base path listed", map[string]string{
			"LUX_LOCAL_ISSUER_KEY": key, "LUX_PUBLIC_URL": "https://api.example.com/v1/models/", "LUX_BASE_PATH": "/v1/models",
			"LUX_OIDC_ISSUERS": "https://api.example.com/v1/models"},
			"LUX_OIDC_ISSUERS lists LUX_PUBLIC_URL https://api.example.com/v1/models while LUX_LOCAL_ISSUER_KEY is set", "", nil},
		{"the public address's host listed without its path", map[string]string{
			"LUX_LOCAL_ISSUER_KEY": key, "LUX_PUBLIC_URL": "https://api.example.com/v1/models", "LUX_BASE_PATH": "/v1/models",
			"LUX_OIDC_ISSUERS": "https://api.example.com"}, "", "", []string{kid}},
		{"a rotation list without the key", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_LOCAL_ISSUER_KEYS": older},
			"LUX_LOCAL_ISSUER_KEYS is set while LUX_LOCAL_ISSUER_KEY is unset", "", nil},
		{"the signing key in the rotation list", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_LOCAL_ISSUER_KEYS": older + "," + key},
			"LUX_LOCAL_ISSUER_KEYS entry 2 is the key LUX_LOCAL_ISSUER_KEY holds, key id " + kid + ", and a token naming a key id held twice verifies against neither", "", nil},
		{"a key listed twice in the rotation list", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_LOCAL_ISSUER_KEYS": older + older},
			"LUX_LOCAL_ISSUER_KEYS lists key id " + olderID + " twice", "", nil},
		{"an unusable entry in the rotation list", map[string]string{"LUX_LOCAL_ISSUER_KEY": key, "LUX_LOCAL_ISSUER_KEYS": older + ",sk-not-a-key"},
			"LUX_LOCAL_ISSUER_KEYS entry 2 is not a PEM encoded private key", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(withKEK(tc.env)))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() = %v, want no problem", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Load() accepted the configuration; want a problem containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Load() = %v, want a problem containing %q", err, tc.want)
			case tc.not != "" && strings.Contains(err.Error(), tc.not):
				t.Errorf("Load() = %v, which should not say %q", err, tc.not)
			}
			if err != nil {
				for _, v := range []string{key, older} {
					if body := strings.Split(v, "\n")[1]; strings.Contains(err.Error(), body) {
						t.Errorf("the problem echoes a key: %v", err)
					}
				}
				return
			}
			var ids []string
			if c.LocalIssuerKey != nil {
				ids = append(ids, c.LocalIssuerKey.ID())
			}
			for _, k := range c.LocalIssuerKeys {
				ids = append(ids, k.ID())
			}
			if strings.Join(ids, ",") != strings.Join(tc.ids, ",") {
				t.Errorf("key ids %v, want %v", ids, tc.ids)
			}
			if body := strings.Split(key, "\n")[1]; strings.Contains(fmt.Sprintf("%+v %#v", c, c), body) {
				t.Error("the configuration renders the key's material")
			}
		})
	}
}
