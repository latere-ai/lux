// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestPublicURLIsRequiredInEveryMode is spec 011's row for the one
// variable every response's URLs are built from: unset is a problem
// naming it in server mode and in file mode alike, a value is held to
// an absolute http:// or https:// URL with a host and nothing after the
// path, and the trailing slash is dropped so a path joins with one.
func TestPublicURLIsRequiredInEveryMode(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"server mode": withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_PUBLIC_URL": ""}),
		"file mode":   {"LUX_MANIFEST_DIR": t.TempDir(), "LUX_PUBLIC_URL": " "},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(m))
			if err == nil || !strings.Contains(err.Error(), "LUX_PUBLIC_URL is unset") {
				t.Fatalf("Load() = %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name, raw, want string // want is the problem's fragment, or the rendered URL
	}{
		{"an https URL", "https://lux.example.com", "https://lux.example.com"},
		{"a trailing slash", "https://lux.example.com/", "https://lux.example.com"},
		{"a path", "https://gateway.example.com/lux/", "https://gateway.example.com/lux"},
		{"an http URL on loopback", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"no scheme", "lux.example.com", `LUX_PUBLIC_URL is "lux.example.com", not an http:// or https:// URL with a host`},
		{"another scheme", "ftp://lux.example.com", `LUX_PUBLIC_URL is "ftp://lux.example.com", not an http:// or https:// URL with a host`},
		{"a query", "https://lux.example.com/?x=1", "and the address carries no query, fragment, or user information"},
		{"a fragment", "https://lux.example.com/#top", "and the address carries no query, fragment, or user information"},
		{"user information", "https://alice@lux.example.com", "and the address carries no query, fragment, or user information"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_PUBLIC_URL": tc.raw})))
			if strings.HasPrefix(tc.want, "http") {
				if err != nil {
					t.Fatal(err)
				}
				if got := c.PublicURL.String(); got != tc.want {
					t.Fatalf("PublicURL = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestAPIRules holds every rule of spec 011's configuration table and
// the two rows of spec 004 the API's Resolve and the doors read: the
// two rates as whole numbers with 0 for no limit, the proxies as CIDR
// ranges, the two byte counts as plain numbers or with a binary suffix
// within their bounds, and the upstream timeout as a duration between
// a second and an hour.
func TestAPIRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"a subject rate", map[string]string{"LUX_REQUESTS_PER_MINUTE": "1200"}, ""},
		{"a subject rate of zero", map[string]string{"LUX_REQUESTS_PER_MINUTE": "0"}, ""},
		{"a subject rate below zero", map[string]string{"LUX_REQUESTS_PER_MINUTE": "-5"}, "LUX_REQUESTS_PER_MINUTE is -5, below 0, and 0 is no limit"},
		{"a subject rate that is not a number", map[string]string{"LUX_REQUESTS_PER_MINUTE": "lots"}, `LUX_REQUESTS_PER_MINUTE is "lots", not a whole number`},
		{"an address rate", map[string]string{"LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "10"}, ""},
		{"an address rate that is not a number", map[string]string{"LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "1.5"}, `LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE is "1.5", not a whole number`},
		{"trusted proxies", map[string]string{"LUX_TRUSTED_PROXIES": "10.0.0.0/8,192.168.1.0/24, fd00::/8"}, ""},
		{"a bare address as a proxy", map[string]string{"LUX_TRUSTED_PROXIES": "10.0.0.1"}, `LUX_TRUSTED_PROXIES entry "10.0.0.1" is not a CIDR range such as 10.0.0.0/8`},
		{"a word as a proxy", map[string]string{"LUX_TRUSTED_PROXIES": "10.0.0.0/8,proxy"}, `LUX_TRUSTED_PROXIES entry "proxy" is not a CIDR range such as 10.0.0.0/8`},
		{"a manifest size", map[string]string{"LUX_MAX_MANIFEST_BYTES": "131072"}, ""},
		{"a manifest size with a suffix", map[string]string{"LUX_MAX_MANIFEST_BYTES": "256Ki"}, ""},
		{"the manifest floor", map[string]string{"LUX_MAX_MANIFEST_BYTES": "1Ki"}, ""},
		{"the manifest ceiling", map[string]string{"LUX_MAX_MANIFEST_BYTES": "16Mi"}, ""},
		{"a manifest size below the floor", map[string]string{"LUX_MAX_MANIFEST_BYTES": "512"}, "LUX_MAX_MANIFEST_BYTES is 512, not between 1Ki and 16Mi"},
		{"a manifest size above the ceiling", map[string]string{"LUX_MAX_MANIFEST_BYTES": "17Mi"}, "LUX_MAX_MANIFEST_BYTES is 17Mi, not between 1Ki and 16Mi"},
		{"a manifest size that is not a count", map[string]string{"LUX_MAX_MANIFEST_BYTES": "big"}, `LUX_MAX_MANIFEST_BYTES is "big", not a byte count such as 64Ki`},
		{"a manifest size with a decimal", map[string]string{"LUX_MAX_MANIFEST_BYTES": "1.5Mi"}, `LUX_MAX_MANIFEST_BYTES is "1.5Mi", not a byte count such as 64Ki`},
		{"a manifest size that overflows", map[string]string{"LUX_MAX_MANIFEST_BYTES": "99999999999Gi"}, `LUX_MAX_MANIFEST_BYTES is "99999999999Gi", not a byte count such as 64Ki`},
		{"a body size", map[string]string{"LUX_MAX_BODY_BYTES": "128Mi"}, ""},
		{"the body ceiling", map[string]string{"LUX_MAX_BODY_BYTES": "1Gi"}, ""},
		{"a body size below the floor", map[string]string{"LUX_MAX_BODY_BYTES": "1Ki"}, "LUX_MAX_BODY_BYTES is 1Ki, not between 4Ki and 1Gi"},
		{"a body size above the ceiling", map[string]string{"LUX_MAX_BODY_BYTES": "2Gi"}, "LUX_MAX_BODY_BYTES is 2Gi, not between 4Ki and 1Gi"},
		{"a body size that is not a count", map[string]string{"LUX_MAX_BODY_BYTES": "-1"}, `LUX_MAX_BODY_BYTES is "-1", not a byte count such as 64Mi`},
		{"an upstream timeout", map[string]string{"LUX_UPSTREAM_TIMEOUT": "30s"}, ""},
		{"an upstream timeout below the floor", map[string]string{"LUX_UPSTREAM_TIMEOUT": "500ms"}, "LUX_UPSTREAM_TIMEOUT is 500ms, not between 1s and 1h0m0s"},
		{"an upstream timeout above the ceiling", map[string]string{"LUX_UPSTREAM_TIMEOUT": "2h"}, "LUX_UPSTREAM_TIMEOUT is 2h, not between 1s and 1h0m0s"},
		{"an upstream timeout that is not a duration", map[string]string{"LUX_UPSTREAM_TIMEOUT": "soon"}, `LUX_UPSTREAM_TIMEOUT is "soon", not a duration such as 10m0s`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})
			maps.Copy(m, tc.env)
			c, err := Load(env(m))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Load() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if c.PublicURL != nil {
				t.Fatal("a refused configuration was returned")
			}
		})
	}
}

// TestAPIValuesAreRead: each variable's value lands in its field with
// the units applied, and a blank one is its default.
func TestAPIValuesAreRead(t *testing.T) {
	c, err := Load(env(withKEK(map[string]string{
		"LUX_OIDC_ISSUERS": issuer, "LUX_REQUESTS_PER_MINUTE": " 30 ", "LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "5",
		"LUX_TRUSTED_PROXIES": "10.1.2.3/8", "LUX_MAX_MANIFEST_BYTES": "2Ki", "LUX_MAX_BODY_BYTES": "1Gi", "LUX_UPSTREAM_TIMEOUT": "1h",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestsPerMinute != 30 || c.UnauthenticatedRequestsPerMinute != 5 || c.MaxManifestBytes != 2048 || c.MaxBodyBytes != 1<<30 || c.UpstreamTimeout != time.Hour {
		t.Fatalf("values = %+v", c)
	}
	if len(c.TrustedProxies) != 1 || c.TrustedProxies[0] != netip.MustParsePrefix("10.0.0.0/8") {
		t.Fatalf("TrustedProxies = %v, want the range masked to its prefix", c.TrustedProxies)
	}
	c, err = Load(env(withKEK(map[string]string{
		"LUX_OIDC_ISSUERS": issuer, "LUX_REQUESTS_PER_MINUTE": " ", "LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE": "",
		"LUX_TRUSTED_PROXIES": " , ", "LUX_MAX_MANIFEST_BYTES": "\t", "LUX_MAX_BODY_BYTES": " ", "LUX_UPSTREAM_TIMEOUT": "",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestsPerMinute != DefaultRequestsPerMinute || c.UnauthenticatedRequestsPerMinute != DefaultUnauthenticatedRequestsPerMinute || c.TrustedProxies != nil ||
		c.MaxManifestBytes != DefaultMaxManifestBytes || c.MaxBodyBytes != DefaultMaxBodyBytes || c.UpstreamTimeout != DefaultUpstreamTimeout {
		t.Fatalf("blank API values did not fall back to defaults: %+v", c)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512", 1 << 10: "1Ki", 65536: "64Ki", 3 << 20: "3Mi", 1 << 30: "1Gi", 1536: "1536"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestBasePathRules is spec 034's LUX_BASE_PATH at load: empty is the
// root; a set value begins with a slash, ends without one, is clean,
// carries no query, fragment, or escape, and equals the path of
// LUX_PUBLIC_URL, each problem naming the variables.
func TestBasePathRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   string
		public string
		want   string // the loaded base path
		fail   string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"unset", "", "https://lux.example.com", "", ""},
		{"unset beside a public URL with a path", "", "https://api.example.com/v1/models", "", ""},
		{"a prefix equal to the public path", "/v1/models", "https://api.example.com/v1/models", "/v1/models", ""},
		{"a prefix equal to a public path with a trailing slash", "/v1/models", "https://api.example.com/v1/models/", "/v1/models", ""},
		{"no leading slash", "v1/models", "https://api.example.com/v1/models", "", `LUX_BASE_PATH is "v1/models", not a clean path`},
		{"a trailing slash", "/v1/models/", "https://api.example.com/v1/models", "", `LUX_BASE_PATH is "/v1/models/", not a clean path`},
		{"the root spelled as a slash", "/", "https://api.example.com", "", `LUX_BASE_PATH is "/", not a clean path`},
		{"an empty segment", "/v1//models", "https://api.example.com/v1//models", "", "not a clean path"},
		{"a dot segment", "/v1/../models", "https://api.example.com/models", "", "not a clean path"},
		{"a query", "/v1/models?x=1", "https://api.example.com/v1/models", "", "no query, fragment, or escape sequence"},
		{"an escape", "/v1/mod%65ls", "https://api.example.com/v1/models", "", "no query, fragment, or escape sequence"},
		{"a public URL without the prefix", "/v1/models", "https://api.example.com", "", `LUX_BASE_PATH is /v1/models and the path of LUX_PUBLIC_URL is ""`},
		{"a public URL with another prefix", "/v1/models", "https://api.example.com/models", "", `the path of LUX_PUBLIC_URL is "/models"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_PUBLIC_URL": tc.public, "LUX_BASE_PATH": tc.base})))
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("Load() = %v, want a problem with %q", err, tc.fail)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if c.BasePath != tc.want {
				t.Fatalf("BasePath = %q, want %q", c.BasePath, tc.want)
			}
		})
	}
}
