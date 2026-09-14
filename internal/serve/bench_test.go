// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// BenchmarkLimiterReserveSettle measures stage 7 on one replica in its pure
// in-memory path: the Key's two rate buckets, the pricing and spend
// projection, admitting the reservation, and settling it with the measured
// count. The counters hold their deltas locally and are never flushed here,
// so no store I/O is on the timed path, which is what a request pays before
// and after the provider call. The rates and the spend amount are large so
// no request is refused and the buckets do not drain over the run; the clock
// is fixed so the window is one.
func BenchmarkLimiterReserveSettle(b *testing.B) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(LimiterOptions{
		Store: memory.New(),
		Now:   func() time.Time { return now },
	})

	k := &v1.Key{}
	k.Metadata.Name = "bench"
	k.Status.ID = "key_" + strings.Repeat("B", 26)
	k.Status.CreatedAt = now.Add(-time.Hour)
	reqRate, tokRate := 1<<40, 1<<40
	amount := v1.Money(1 << 62)
	k.Spec.Limits.RequestsPerMinute = &reqRate
	k.Spec.Limits.TokensPerMinute = &tokRate
	k.Spec.Limits.Spend = &v1.Spend{Amount: &amount, Currency: "USD", Window: "1h"}

	m := priced("bench", "0.01", "0.03", "USD")
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		lease, err := l.Reserve(ctx, gateway.Reservation{Key: k, Model: m, InputTokens: 100, OutputTokens: 200})
		if err != nil {
			b.Fatal(err)
		}
		lease.Settle(ctx, gateway.Tokens{Input: 100, Output: 150})
	}
}
