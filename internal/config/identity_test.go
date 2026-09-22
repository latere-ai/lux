// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"strings"
	"testing"
)

// TestServeRefusesToStartWithoutAnIssuer is spec 006's first start-up
// rule at the configuration: no issuer and no manifest directory is a
// problem naming the variable, and either one satisfies it.
func TestServeRefusesToStartWithoutAnIssuer(t *testing.T) {
	_, err := Load(env(nil))
	if err == nil || !strings.Contains(err.Error(), "LUX_OIDC_ISSUERS is unset") {
		t.Fatalf("Load() without an issuer: %v", err)
	}
	if _, err := Load(env(map[string]string{"LUX_MANIFEST_DIR": t.TempDir()})); err != nil {
		t.Fatalf("Load() in the file mode: %v", err)
	}
	if _, err := Load(env(map[string]string{"LUX_MANIFEST_DIR": "  "})); err == nil {
		t.Fatal("Load() took a blank LUX_MANIFEST_DIR as the file mode")
	}
}

// TestIdentityRules holds every rule of spec 006's configuration table,
// one row per problem, each message naming its variable.
func TestIdentityRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"an https issuer", map[string]string{"LUX_OIDC_ISSUERS": issuer}, ""},
		{"an http issuer on loopback", map[string]string{"LUX_OIDC_ISSUERS": "http://127.0.0.1:9100"}, ""},
		{"an http issuer on localhost", map[string]string{"LUX_OIDC_ISSUERS": "http://localhost:9100/"}, ""},
		{"an http issuer on ::1", map[string]string{"LUX_OIDC_ISSUERS": "http://[::1]:9100"}, ""},
		{"an http issuer off loopback", map[string]string{"LUX_OIDC_ISSUERS": "http://issuer.internal.example"},
			"LUX_OIDC_ISSUERS http://issuer.internal.example is http:// on a host other than loopback"},
		{"an http issuer off loopback listed as insecure", map[string]string{
			"LUX_OIDC_ISSUERS": "http://issuer.internal.example/", "LUX_OIDC_INSECURE_ISSUERS": "http://issuer.internal.example"}, ""},
		{"an insecure issuer not in the list", map[string]string{
			"LUX_OIDC_ISSUERS": issuer, "LUX_OIDC_INSECURE_ISSUERS": "http://other.example"},
			"LUX_OIDC_INSECURE_ISSUERS names http://other.example, which is not in LUX_OIDC_ISSUERS"},
		{"an issuer that is not a URL", map[string]string{"LUX_OIDC_ISSUERS": "login.example.com"},
			`LUX_OIDC_ISSUERS entry "login.example.com" is not an http:// or https:// URL with a host`},
		{"an issuer with another scheme", map[string]string{"LUX_OIDC_ISSUERS": "ftp://login.example.com"},
			`LUX_OIDC_ISSUERS entry "ftp://login.example.com" is not an http:// or https:// URL`},
		{"an issuer listed twice", map[string]string{"LUX_OIDC_ISSUERS": issuer + "," + issuer + "/"},
			"LUX_OIDC_ISSUERS lists " + issuer + " twice"},
		{"an authorizer with its token", map[string]string{
			"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_URL": "https://authz.example.com", "LUX_AUTHORIZER_TOKEN": "t"}, ""},
		{"an authorizer without its token", map[string]string{
			"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_URL": "https://authz.example.com"},
			"LUX_AUTHORIZER_TOKEN is unset while LUX_AUTHORIZER_URL is set"},
		{"a token without an authorizer", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_TOKEN": "t"}, ""},
		{"an http authorizer on loopback", map[string]string{
			"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_URL": "http://127.0.0.1:9200/", "LUX_AUTHORIZER_TOKEN": "t"}, ""},
		{"an http authorizer off loopback", map[string]string{
			"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_URL": "http://authz.internal.example", "LUX_AUTHORIZER_TOKEN": "t"},
			"LUX_AUTHORIZER_URL http://authz.internal.example is http:// on a host other than loopback"},
		{"an authorizer that is not a URL", map[string]string{
			"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_URL": "authz.example.com", "LUX_AUTHORIZER_TOKEN": "t"},
			`LUX_AUTHORIZER_URL is "authz.example.com", not an http:// or https:// URL with a host`},
		{"a timeout", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_TIMEOUT": "750ms"}, ""},
		{"a timeout that is not a duration", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_TIMEOUT": "five"},
			`LUX_AUTHORIZER_TIMEOUT is "five", not a duration such as 5s`},
		{"a zero timeout", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_TIMEOUT": "0s"},
			"LUX_AUTHORIZER_TIMEOUT is 0s, and one decision's deadline is above zero"},
		{"a negative timeout", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_AUTHORIZER_TIMEOUT": "-1s"},
			"LUX_AUTHORIZER_TIMEOUT is -1s, and one decision's deadline is above zero"},
		{"admin subjects", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_ADMIN_SUBJECTS": issuer + "|alice"}, ""},
		{"an admin subject that is a bare sub", map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_ADMIN_SUBJECTS": "alice"},
			`LUX_ADMIN_SUBJECTS entry "alice" is not a rendered subject of the form <issuer>|<sub>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(withKEK(tc.env)))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() = %v, want no problem", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Load() accepted the configuration; want a problem containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Load() = %v, want a problem containing %q", err, tc.want)
			}
		})
	}
}

// TestIdentityProblemsJoinTheOneMessage: the identity problems sort in
// with the listener problems, so an operator reads one line.
func TestIdentityProblemsJoinTheOneMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_PUBLIC_ADDR":        "nope",
		"LUX_AUTHORIZER_URL":     "https://authz.example.com",
		"LUX_AUTHORIZER_TIMEOUT": "soon",
		"LUX_SECRETS_KEK":        kek,
	}))
	if err == nil {
		t.Fatal("Load() accepted four problems")
	}
	got := err.Error()
	order := []string{"LUX_AUTHORIZER_TIMEOUT", "LUX_AUTHORIZER_TOKEN", "LUX_OIDC_ISSUERS", "LUX_PUBLIC_ADDR"}
	last := -1
	for _, name := range order {
		i := strings.Index(got, name)
		if i < 0 {
			t.Errorf("message lacks %s:\n%s", name, got)
		}
		if i < last {
			t.Errorf("%s is out of order:\n%s", name, got)
		}
		last = i
	}
	if strings.Count(got, "; ") != len(order)-1 {
		t.Errorf("want %d problems in one message:\n%s", len(order), got)
	}
}

func TestAuthorizeListItemsFlag(t *testing.T) {
	for _, value := range []string{"", "0", "1", "true"} {
		c := Config{}
		problems := c.loadIdentity(func(name string) string {
			if name == "LUX_AUTHORIZE_LIST_ITEMS" {
				return value
			}
			if name == "LUX_OIDC_ISSUERS" {
				return "https://issuer.example.com"
			}
			return ""
		})
		if c.AuthorizeListItems != (value == "1") || (len(problems) > 0) != (value == "true" || value == "0") {
			t.Fatalf("%q: %v %v", value, c.AuthorizeListItems, problems)
		}
	}
}

// TestAudienceListIsParsed is spec 034's audience list: a comma list of
// distinct names with the first the primary, an empty entry and a
// repeated entry each a problem naming the variable, and an unset value
// the default alone.
func TestAudienceListIsParsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string // the audiences, or nil when the value is a problem
		fail string   // a fragment of the problem
	}{
		{"unset", "", []string{DefaultOIDCAudience}, ""},
		{"blank", "  ", []string{DefaultOIDCAudience}, ""},
		{"one name", "lux", []string{"lux"}, ""},
		{"two names, the first the primary", "lux,api.example.com", []string{"lux", "api.example.com"}, ""},
		{"two names spaced", " api.example.com , lux ", []string{"api.example.com", "lux"}, ""},
		{"a trailing comma", "lux,", nil, `LUX_OIDC_AUDIENCE is "lux,", and one of its entries is empty`},
		{"a leading comma", ",lux", nil, `LUX_OIDC_AUDIENCE is ",lux", and one of its entries is empty`},
		{"two commas", "lux,,api.example.com", nil, "one of its entries is empty"},
		{"a repeated name", "lux,lux", nil, "LUX_OIDC_AUDIENCE lists lux twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_OIDC_AUDIENCE": tc.raw})))
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("Load(%q) = %v, want a problem with %q", tc.raw, err, tc.fail)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q): %v", tc.raw, err)
			}
			if !slices.Equal(c.OIDCAudiences, tc.want) {
				t.Fatalf("Load(%q).OIDCAudiences = %q, want %q", tc.raw, c.OIDCAudiences, tc.want)
			}
		})
	}
}
