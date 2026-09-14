// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

// TestServeRequiresTheKEKOutsideFileMode is spec 005's start-up rule at
// the configuration: no key and no manifest directory is a problem
// naming the variable, the file mode lifts it, and a key that fails to
// decode is named by position and never by value.
func TestServeRequiresTheKEKOutsideFileMode(t *testing.T) {
	_, err := Load(env(map[string]string{"LUX_OIDC_ISSUERS": issuer}))
	if err == nil || !strings.Contains(err.Error(), "LUX_SECRETS_KEK is unset") {
		t.Fatalf("Load() without a key: %v", err)
	}
	if _, err := Load(env(map[string]string{"LUX_MANIFEST_DIR": t.TempDir()})); err != nil {
		t.Fatalf("Load() in the file mode: %v", err)
	}
	short := "CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQ=="
	_, err = Load(env(map[string]string{"LUX_MANIFEST_DIR": t.TempDir(), "LUX_SECRETS_KEK": kek + "," + short}))
	if err == nil || !strings.Contains(err.Error(), "LUX_SECRETS_KEK key 2 of 2 decodes to 31 bytes, not 32") {
		t.Fatalf("Load() with a short key in the file mode: %v", err)
	}
	if strings.Contains(err.Error(), short) {
		t.Fatalf("the problem names the key: %v", err)
	}
}

// TestProviderRules holds every rule of spec 005's configuration table,
// one row per problem, each message naming its variable.
func TestProviderRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"two keys", map[string]string{"LUX_SECRETS_KEK": kek + "," + strings.ReplaceAll(kek, "AQ", "Ag")}, ""},
		{"nine keys", map[string]string{"LUX_SECRETS_KEK": strings.Repeat(kek+",", 9)}, "LUX_SECRETS_KEK lists 9 keys, at most 8"},
		{"a key that is not base64", map[string]string{"LUX_SECRETS_KEK": kek + ",not-a-key"}, "LUX_SECRETS_KEK key 2 of 2 is not standard base64 with padding"},
		{"private upstreams allowed", map[string]string{"LUX_UPSTREAM_ALLOW_PRIVATE": "1"}, ""},
		{"private upstreams by another word", map[string]string{"LUX_UPSTREAM_ALLOW_PRIVATE": "yes"}, `LUX_UPSTREAM_ALLOW_PRIVATE is "yes", and 1 is the one value that sets it`},
		{"private upstreams by true", map[string]string{"LUX_UPSTREAM_ALLOW_PRIVATE": "true"}, `LUX_UPSTREAM_ALLOW_PRIVATE is "true", and 1 is the one value that sets it`},
		{"a discovery interval", map[string]string{"LUX_DISCOVERY_INTERVAL": "2h"}, ""},
		{"the discovery floor", map[string]string{"LUX_DISCOVERY_INTERVAL": "1m"}, ""},
		{"the discovery ceiling", map[string]string{"LUX_DISCOVERY_INTERVAL": "24h"}, ""},
		{"a discovery interval below the floor", map[string]string{"LUX_DISCOVERY_INTERVAL": "59s"}, "LUX_DISCOVERY_INTERVAL is 59s, not between 1m0s and 24h0m0s"},
		{"a discovery interval above the ceiling", map[string]string{"LUX_DISCOVERY_INTERVAL": "25h"}, "LUX_DISCOVERY_INTERVAL is 25h, not between 1m0s and 24h0m0s"},
		{"a discovery interval that is not a duration", map[string]string{"LUX_DISCOVERY_INTERVAL": "hourly"}, `LUX_DISCOVERY_INTERVAL is "hourly", not a duration such as 1h0m0s`},
		{"a health interval", map[string]string{"LUX_HEALTH_INTERVAL": "10s"}, ""},
		{"the health floor", map[string]string{"LUX_HEALTH_INTERVAL": "5s"}, ""},
		{"the health ceiling", map[string]string{"LUX_HEALTH_INTERVAL": "10m"}, ""},
		{"a health interval below the floor", map[string]string{"LUX_HEALTH_INTERVAL": "4s"}, "LUX_HEALTH_INTERVAL is 4s, not between 5s and 10m0s"},
		{"a health interval above the ceiling", map[string]string{"LUX_HEALTH_INTERVAL": "11m"}, "LUX_HEALTH_INTERVAL is 11m, not between 5s and 10m0s"},
		{"a health interval that is not a duration", map[string]string{"LUX_HEALTH_INTERVAL": "often"}, `LUX_HEALTH_INTERVAL is "often", not a duration such as 30s`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})
			maps.Copy(m, tc.env)
			c, err := Load(env(m))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() = %v, want no problem", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Load() accepted the configuration; want a problem containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Load() = %v, want a problem containing %q", err, tc.want)
			}
			if tc.want == "" && tc.env["LUX_DISCOVERY_INTERVAL"] == "2h" && c.DiscoveryInterval != 2*time.Hour {
				t.Fatalf("DiscoveryInterval = %s", c.DiscoveryInterval)
			}
		})
	}
}

// TestProviderProblemsJoinTheOneMessage: the provider problems sort in
// with the rest, so an operator reads one line.
func TestProviderProblemsJoinTheOneMessage(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LUX_OIDC_ISSUERS":       issuer,
		"LUX_PUBLIC_ADDR":        "nope",
		"LUX_HEALTH_INTERVAL":    "1s",
		"LUX_DISCOVERY_INTERVAL": "1s",
	}))
	if err == nil {
		t.Fatal("Load() accepted four problems")
	}
	got := err.Error()
	order := []string{"LUX_DISCOVERY_INTERVAL", "LUX_HEALTH_INTERVAL", "LUX_PUBLIC_ADDR", "LUX_SECRETS_KEK"}
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
	if strings.Count(got, "; ") != len(order)-1 || strings.Count(got, "\n") != 0 {
		t.Errorf("want %d problems in one line:\n%s", len(order), got)
	}
}

// TestLoadRewrap: the rewrap role reads the keys and the database and
// nothing else, both required, and echoes neither.
func TestLoadRewrap(t *testing.T) {
	const url = "postgres://lux:s3cret-password@db.example.com/lux"
	r, err := LoadRewrap(env(map[string]string{"LUX_SECRETS_KEK": kek, "LUX_DB_URL": url, "LUX_PUBLIC_ADDR": "nope"}))
	if err != nil || r.DBURL != url || r.SecretsKEK == nil || r.SecretsKEK.Len() != 1 {
		t.Fatalf("LoadRewrap() = %+v, %v", r, err)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want []string
	}{
		"nothing set":    {nil, []string{"LUX_DB_URL is unset, and rewrap re-wraps the credential rows of a database", "LUX_SECRETS_KEK is unset, and rewrap re-wraps every stored data key"}},
		"no database":    {map[string]string{"LUX_SECRETS_KEK": kek}, []string{"LUX_DB_URL is unset"}},
		"a bad database": {map[string]string{"LUX_SECRETS_KEK": kek, "LUX_DB_URL": "mysql://db.example.com/lux"}, []string{`LUX_DB_URL has scheme "mysql", not postgres`}},
		"a bad key":      {map[string]string{"LUX_SECRETS_KEK": "not-a-key", "LUX_DB_URL": url}, []string{"LUX_SECRETS_KEK key 1 of 1 is not standard base64"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadRewrap(env(tc.env))
			if err == nil {
				t.Fatal("LoadRewrap() accepted the configuration")
			}
			got := err.Error()
			if !strings.HasPrefix(got, "configuration: ") || strings.Contains(got, "s3cret-password") || strings.Count(got, "; ") != len(tc.want)-1 {
				t.Fatalf("LoadRewrap() = %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("message lacks %q:\n%s", w, got)
				}
			}
		})
	}
}
