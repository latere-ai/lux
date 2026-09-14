// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

// TestMeteringRules holds the one rule of spec 009's configuration
// table: the flush interval between a tenth of a second and a minute,
// one second by default, and a problem that joins the one message.
func TestMeteringRules(t *testing.T) {
	c, err := Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})))
	if err != nil {
		t.Fatal(err)
	}
	if c.MeteringFlush != DefaultMeteringFlush || DefaultMeteringFlush != time.Second {
		t.Fatalf("default = %s", c.MeteringFlush)
	}
	for _, tc := range []struct {
		name  string
		value string
		want  string // a fragment of the problem, or "" for a value that loads
		flush time.Duration
	}{
		{"an interval", "5s", "", 5 * time.Second},
		{"the floor", "100ms", "", 100 * time.Millisecond},
		{"the ceiling", "1m", "", time.Minute},
		{"blank is the default", "  ", "", time.Second},
		{"below the floor", "50ms", "LUX_METERING_FLUSH is 50ms, not between 100ms and 1m0s", 0},
		{"above the ceiling", "61s", "LUX_METERING_FLUSH is 61s, not between 100ms and 1m0s", 0},
		{"not a duration", "often", `LUX_METERING_FLUSH is "often", not a duration such as 1s`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer})
			maps.Copy(m, map[string]string{"LUX_METERING_FLUSH": tc.value})
			c, err := Load(env(m))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() = %v, want no problem", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Load() accepted %q; want a problem containing %q", tc.value, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Load() = %v, want a problem containing %q", err, tc.want)
			case tc.want == "" && c.MeteringFlush != tc.flush:
				t.Fatalf("MeteringFlush = %s, want %s", c.MeteringFlush, tc.flush)
			}
		})
	}
	// The problem sorts in with the rest, in one line.
	_, err = Load(env(withKEK(map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_METERING_FLUSH": "0s", "LUX_KEY_CACHE": "0s"})))
	if err == nil {
		t.Fatal("Load() accepted two problems")
	}
	got := err.Error()
	if strings.Index(got, "LUX_KEY_CACHE") > strings.Index(got, "LUX_METERING_FLUSH") || strings.Count(got, "; ") != 1 || strings.Count(got, "\n") != 0 {
		t.Fatalf("want two sorted problems in one line:\n%s", got)
	}
}
