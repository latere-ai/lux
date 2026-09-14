// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestDecodeLimits: an absent object and an empty one are the zero
// figures, a field the gateway does not know is ignored, and a figure
// below zero, a spend that is not a money string, or a ttl that is not a
// duration is refused, which the caller reads as no decision.
func TestDecodeLimits(t *testing.T) {
	for _, tc := range []struct {
		name, limits string
		want         Limits
		bad          bool
	}{
		{"absent", "", Limits{}, false},
		{"null", "null", Limits{}, false},
		{"empty", "{}", Limits{}, false},
		{"an unknown field", `{"max_key_seats": 3, "max_keys": 2}`, Limits{MaxKeys: 2}, false},
		{"a fraction of money", `{"max_key_spend": "0.5"}`, Limits{Key: manifest.Limits{MaxSpend: 500000}}, false},
		{"a zero ttl", `{"max_key_ttl": "0s"}`, Limits{}, false},
		{"a rate below zero", `{"requests_per_minute": -1}`, Limits{}, true},
		{"a key rate below zero", `{"max_key_requests_per_minute": -5}`, Limits{}, true},
		{"a token rate below zero", `{"max_key_tokens_per_minute": -5}`, Limits{}, true},
		{"max_keys below zero", `{"max_keys": -1}`, Limits{}, true},
		{"a spend as a number", `{"max_key_spend": 50}`, Limits{}, true},
		{"a spend that is not money", `{"max_key_spend": "fifty"}`, Limits{}, true},
		{"a ttl that is not a duration", `{"max_key_ttl": "soon"}`, Limits{}, true},
		{"a ttl below zero", `{"max_key_ttl": "-1h"}`, Limits{}, true},
		{"not an object", `[1]`, Limits{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := authz.Decision{Allow: true}
			if tc.limits != "" {
				d.Limits = json.RawMessage(tc.limits)
			}
			got, err := DecodeLimits(d)
			if tc.bad {
				if err == nil {
					t.Fatalf("DecodeLimits(%s) = %+v, want an error", tc.limits, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DecodeLimits(%s) = %+v, %v; want %+v", tc.limits, got, err, tc.want)
			}
		})
	}
}

// TestWireLimitsRoundTrip: an answer that sets two ceilings renders
// exactly those two wire names, the other four staying absent, and
// decodes back to those two figures with every other figure zero. A
// member set to zero is sent and reads as zero, which is the grant
// absence is.
func TestWireLimitsRoundTrip(t *testing.T) {
	spend := v1.Money(50_000_000)
	rpm := 1200
	raw, err := json.Marshal(WireLimits{MaxKeySpend: &spend, MaxKeyTTL: "720h"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"max_key_spend":"50","max_key_ttl":"720h"}`; got != want {
		t.Errorf("two ceilings render %s, want %s", got, want)
	}
	got, err := DecodeLimits(authz.Decision{Allow: true, Limits: raw})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Limits{Key: manifest.Limits{MaxSpend: spend, MaxTTL: 720 * time.Hour}}); got != want {
		t.Errorf("they decode to %+v, want %+v", got, want)
	}
	zero, err := json.Marshal(WireLimits{RequestsPerMinute: &rpm, MaxKeys: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"requests_per_minute":1200,"max_keys":0}`; string(zero) != want {
		t.Errorf("a member set to zero renders %s, want %s", zero, want)
	}
	back, err := DecodeLimits(authz.Decision{Allow: true, Limits: zero})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Limits{RequestsPerMinute: 1200}); back != want {
		t.Errorf("it decodes to %+v, want %+v", back, want)
	}
}
