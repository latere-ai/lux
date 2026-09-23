// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

// TestCatalogRules holds the rows of spec 036's configuration table: the
// backstop between five seconds and ten minutes with thirty seconds by
// default, and the grace between zero and an hour with five minutes by
// default, zero turning it off.
func TestCatalogRules(t *testing.T) {
	c, err := Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})))
	if err != nil {
		t.Fatal(err)
	}
	if c.CatalogReload != 30*time.Second || c.KeyCacheGrace != 5*time.Minute {
		t.Fatalf("defaults = %s, %s", c.CatalogReload, c.KeyCacheGrace)
	}
	for _, tc := range []struct {
		name   string
		env    map[string]string
		want   string // a fragment of the problem, or "" for a configuration that loads
		reload time.Duration
		grace  time.Duration
	}{
		{"a backstop", map[string]string{"LUX_CATALOG_RELOAD": "2m"}, "", 2 * time.Minute, 5 * time.Minute},
		{"the backstop floor", map[string]string{"LUX_CATALOG_RELOAD": "5s"}, "", 5 * time.Second, 5 * time.Minute},
		{"the backstop ceiling", map[string]string{"LUX_CATALOG_RELOAD": "10m"}, "", 10 * time.Minute, 5 * time.Minute},
		{"a backstop below the floor", map[string]string{"LUX_CATALOG_RELOAD": "1s"}, "LUX_CATALOG_RELOAD is 1s, not between 5s and 10m0s", 0, 0},
		{"a backstop above the ceiling", map[string]string{"LUX_CATALOG_RELOAD": "11m"}, "LUX_CATALOG_RELOAD is 11m, not between 5s and 10m0s", 0, 0},
		{"a grace of zero", map[string]string{"LUX_KEY_CACHE_GRACE": "0s"}, "", 30 * time.Second, 0},
		{"the grace ceiling", map[string]string{"LUX_KEY_CACHE_GRACE": "1h"}, "", 30 * time.Second, time.Hour},
		{"a grace above the ceiling", map[string]string{"LUX_KEY_CACHE_GRACE": "2h"}, "LUX_KEY_CACHE_GRACE is 2h, not between 0s and 1h0m0s", 0, 0},
		{"a grace below zero", map[string]string{"LUX_KEY_CACHE_GRACE": "-1s"}, "LUX_KEY_CACHE_GRACE is -1s, not between 0s and 1h0m0s", 0, 0},
		{"a grace that is not a duration", map[string]string{"LUX_KEY_CACHE_GRACE": "long"}, `LUX_KEY_CACHE_GRACE is "long", not a duration such as 5m0s`, 0, 0},
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
			if tc.want == "" && (c.CatalogReload != tc.reload || c.KeyCacheGrace != tc.grace) {
				t.Fatalf("CatalogReload, KeyCacheGrace = %s, %s, want %s, %s", c.CatalogReload, c.KeyCacheGrace, tc.reload, tc.grace)
			}
		})
	}
}
