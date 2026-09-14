// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

// TestKeyRules holds every rule of spec 007's configuration table: the
// cache window between one second and ten minutes with ten seconds by
// default, and the two default rates as whole numbers of zero or more
// with zero, no limit, by default.
func TestKeyRules(t *testing.T) {
	c, err := Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})))
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyCache != DefaultKeyCache || c.DefaultRequestsPerMinute != 0 || c.DefaultTokensPerMinute != 0 {
		t.Fatalf("defaults = %s, %d, %d", c.KeyCache, c.DefaultRequestsPerMinute, c.DefaultTokensPerMinute)
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string // a fragment of the problem, or "" for a configuration that loads
	}{
		{"a cache window", map[string]string{"LUX_KEY_CACHE": "30s"}, ""},
		{"the cache floor", map[string]string{"LUX_KEY_CACHE": "1s"}, ""},
		{"the cache ceiling", map[string]string{"LUX_KEY_CACHE": "10m"}, ""},
		{"a cache window below the floor", map[string]string{"LUX_KEY_CACHE": "500ms"}, "LUX_KEY_CACHE is 500ms, not between 1s and 10m0s"},
		{"a cache window above the ceiling", map[string]string{"LUX_KEY_CACHE": "11m"}, "LUX_KEY_CACHE is 11m, not between 1s and 10m0s"},
		{"a cache window that is not a duration", map[string]string{"LUX_KEY_CACHE": "soon"}, `LUX_KEY_CACHE is "soon", not a duration such as 10s`},
		{"a request rate", map[string]string{"LUX_DEFAULT_REQUESTS_PER_MINUTE": "600"}, ""},
		{"a request rate of zero", map[string]string{"LUX_DEFAULT_REQUESTS_PER_MINUTE": "0"}, ""},
		{"a request rate below zero", map[string]string{"LUX_DEFAULT_REQUESTS_PER_MINUTE": "-1"}, "LUX_DEFAULT_REQUESTS_PER_MINUTE is -1, below 0, and 0 is no limit"},
		{"a request rate that is not a number", map[string]string{"LUX_DEFAULT_REQUESTS_PER_MINUTE": "many"}, `LUX_DEFAULT_REQUESTS_PER_MINUTE is "many", not a whole number`},
		{"a token rate", map[string]string{"LUX_DEFAULT_TOKENS_PER_MINUTE": " 100000 "}, ""},
		{"a token rate that is a fraction", map[string]string{"LUX_DEFAULT_TOKENS_PER_MINUTE": "1.5"}, `LUX_DEFAULT_TOKENS_PER_MINUTE is "1.5", not a whole number`},
		{"a token rate below zero", map[string]string{"LUX_DEFAULT_TOKENS_PER_MINUTE": "-100"}, "LUX_DEFAULT_TOKENS_PER_MINUTE is -100, below 0"},
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
			if tc.want != "" {
				return
			}
			switch tc.name {
			case "a cache window":
				if c.KeyCache != 30*time.Second {
					t.Fatalf("KeyCache = %s", c.KeyCache)
				}
			case "a request rate":
				if c.DefaultRequestsPerMinute != 600 {
					t.Fatalf("DefaultRequestsPerMinute = %d", c.DefaultRequestsPerMinute)
				}
			case "a token rate":
				if c.DefaultTokensPerMinute != 100000 {
					t.Fatalf("DefaultTokensPerMinute = %d", c.DefaultTokensPerMinute)
				}
			}
		})
	}
}

// TestKeyProblemsJoinTheOneMessage: the key problems sort in with the
// rest, so an operator reads one line.
func TestKeyProblemsJoinTheOneMessage(t *testing.T) {
	_, err := Load(env(withKEK(map[string]string{
		"LUX_OIDC_ISSUERS":                issuer,
		"LUX_KEY_CACHE":                   "0s",
		"LUX_DEFAULT_TOKENS_PER_MINUTE":   "-1",
		"LUX_DEFAULT_REQUESTS_PER_MINUTE": "x",
		"LUX_HEALTH_INTERVAL":             "1s",
	})))
	if err == nil {
		t.Fatal("Load() accepted four problems")
	}
	got := err.Error()
	order := []string{"LUX_DEFAULT_REQUESTS_PER_MINUTE", "LUX_DEFAULT_TOKENS_PER_MINUTE", "LUX_HEALTH_INTERVAL", "LUX_KEY_CACHE"}
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
