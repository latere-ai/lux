// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// BenchmarkResolve measures the resolve every surface runs on the control
// plane path, one sub-benchmark per kind: the six stages of spec 003 over
// a decoded object, structural validation through consistency, against a
// fixed in-memory Lookup so nothing dials. The input is decoded once and
// resolved in the loop; Resolve does not change its input.
func BenchmarkResolve(b *testing.B) {
	prov, err := Decode([]byte(head(v1.KindProvider, "openai")+minProvider), MediaYAML, Hint{})
	if err != nil {
		b.Fatal(err)
	}
	o := Options{
		Actor: Actor{Subject: "https://login.example.com|alice"},
		Lookup: &stubLookup{
			providers: map[string]*v1.Provider{"openai": prov.(*v1.Provider)},
			budgets:   map[string]*v1.Budget{},
			models:    []string{"gpt-5"},
		},
		Defaults: Defaults{RequestsPerMinute: 30, TokensPerMinute: 4000, Timeout: 90 * time.Second},
		Now:      func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) },
		NewName:  func() string { return "generated" },
	}
	for _, c := range []struct {
		kind, body string
	}{
		{v1.KindProvider, head(v1.KindProvider, "p") + minProvider},
		{v1.KindModel, head(v1.KindModel, "m") + minModel},
		{v1.KindKey, head(v1.KindKey, "k") + minKey},
		{v1.KindBudget, head(v1.KindBudget, "bud") + minBudget},
	} {
		obj, err := Decode([]byte(c.body), MediaYAML, Hint{})
		if err != nil {
			b.Fatalf("%s: %v", c.kind, err)
		}
		b.Run(c.kind, func(b *testing.B) {
			b.ReportAllocs()
			ctx := context.Background()
			for range b.N {
				if _, err := Resolve(ctx, obj, o); err != nil {
					b.Fatalf("%s: %v", c.kind, err)
				}
			}
		})
	}
}
