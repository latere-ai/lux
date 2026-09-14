// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// issuer is the one variable a server-mode configuration cannot do
// without, so every test that is not about it sets it.
const issuer = "https://login.example.com"

func TestLoadAppliesEveryDefault(t *testing.T) {
	c, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr: ":8080", InternalAddr: ":8081",
		OIDCIssuers: []string{issuer}, OIDCAudience: "lux", AuthorizerTimeout: 5 * time.Second,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":           "127.0.0.1:9000",
		"LUX_INTERNAL_ADDR":         "127.0.0.1:9001",
		"LUX_OIDC_ISSUERS":          issuer + "/, http://issuer.internal.example",
		"LUX_OIDC_AUDIENCE":         "gateway",
		"LUX_OIDC_INSECURE_ISSUERS": "http://issuer.internal.example/",
		"LUX_AUTHORIZER_URL":        "https://authz.example.com/decide",
		"LUX_AUTHORIZER_TOKEN":      " s3cret\n",
		"LUX_AUTHORIZER_TIMEOUT":    "2s",
		"LUX_ADMIN_SUBJECTS":        issuer + "|alice, " + issuer + "|ops",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		PublicAddr: "127.0.0.1:9000", InternalAddr: "127.0.0.1:9001",
		OIDCIssuers:         []string{issuer, "http://issuer.internal.example"},
		OIDCAudience:        "gateway",
		OIDCInsecureIssuers: []string{"http://issuer.internal.example"},
		AuthorizerURL:       "https://authz.example.com/decide",
		AuthorizerToken:     "s3cret",
		AuthorizerTimeout:   2 * time.Second,
		AdminSubjects:       []string{issuer + "|alice", issuer + "|ops"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load() = %+v, want %+v", c, want)
	}
}

func TestLoadReportsEveryProblemInOneSortedMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":   "nope",
		"LUX_INTERNAL_ADDR": "nope",
		"LUX_OIDC_ISSUERS":  issuer,
	}))
	if err == nil {
		t.Fatal("Load() accepted two addresses that are not host:port")
	}
	got := err.Error()
	for _, want := range []string{
		"configuration: ",
		`LUX_INTERNAL_ADDR is "nope", not a host:port address`,
		`LUX_PUBLIC_ADDR is "nope", not a host:port address`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	// An address that does not parse cannot be compared as a socket, so
	// the equality problem is not reported on top of the two syntax ones.
	if strings.Contains(got, "must differ from") {
		t.Errorf("two unparseable addresses were also reported as one socket:\n%s", got)
	}
	if i, j := strings.Index(got, "LUX_INTERNAL_ADDR is"), strings.Index(got, "LUX_PUBLIC_ADDR"); i > j {
		t.Errorf("problems are not sorted by name:\n%s", got)
	}
}

func TestLoadTreatsBlankAsUnset(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR": "  ", "LUX_INTERNAL_ADDR": "", "LUX_OIDC_ISSUERS": issuer,
		"LUX_OIDC_AUDIENCE": " ", "LUX_AUTHORIZER_URL": " ", "LUX_AUTHORIZER_TOKEN": "",
		"LUX_AUTHORIZER_TIMEOUT": "  ", "LUX_ADMIN_SUBJECTS": " , ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != DefaultPublicAddr || c.InternalAddr != DefaultInternalAddr {
		t.Fatalf("blank values did not fall back to defaults: %+v", c)
	}
	if c.OIDCAudience != DefaultOIDCAudience || c.AuthorizerURL != "" || c.AuthorizerTimeout != DefaultAuthorizerTimeout || c.AdminSubjects != nil {
		t.Fatalf("blank identity values did not fall back to defaults: %+v", c)
	}
}

func TestLoadRefusesOneSocketForBothListeners(t *testing.T) {
	_, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:9000", "LUX_INTERNAL_ADDR": "127.0.0.1:9000", "LUX_OIDC_ISSUERS": issuer}))
	if err == nil || !strings.Contains(err.Error(), "must differ from LUX_PUBLIC_ADDR; both are 127.0.0.1:9000") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadAllowsPortZeroOnBothListeners(t *testing.T) {
	if _, err := Load(env(map[string]string{"LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0", "LUX_OIDC_ISSUERS": issuer})); err != nil {
		t.Fatal(err)
	}
}
