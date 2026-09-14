// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// benchPricing prices all four token members, so Cost sums every term.
func benchPricing() *v1.Pricing {
	in, out := v1.Money(3_000_000), v1.Money(15_000_000)
	ci, cw := v1.Money(300_000), v1.Money(3_750_000)
	return &v1.Pricing{Currency: "USD", Per: 1_000_000, Input: &in, Output: &out, CachedInput: &ci, CacheWrite: &cw}
}

// BenchmarkCost measures the per-request costing: the integer sum of the
// priced token members with one half-up rounding, which every settled
// request runs.
func BenchmarkCost(b *testing.B) {
	b.ReportAllocs()
	p := benchPricing()
	t := Tokens{Input: 1200, Output: 800, CachedInput: 300, CacheWrite: 64}
	var sink v1.Money
	for range b.N {
		sink, _ = Cost(t, p)
	}
	_ = sink
}

// BenchmarkWindow measures the window bounds arithmetic: the start a counter
// key carries and the reset a refusal names, aligned to the epoch, which
// every spend check runs.
func BenchmarkWindow(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		Window("1h", at, created)
	}
}

// BenchmarkCounterKey measures rendering a spend counter's key, the window
// arithmetic plus the string the store is asked with on every reservation.
func BenchmarkCounterKey(b *testing.B) {
	b.ReportAllocs()
	var sink string
	for range b.N {
		sink = CounterKey(ScopeKeySpend, "key_01J9ZK2P7Q8R9S0T1U2V3W4X5Y", "1h", at, created)
	}
	_ = sink
}
