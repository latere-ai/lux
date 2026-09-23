// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
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

// BenchmarkCatalogLookups measures what one request resolves from the
// catalog, the Model by name, its Provider, and the Provider's
// credential opened, read from the store as before spec 036 and from the
// snapshot. On the memory store both are in process, so the difference
// is the store's own locking and copying; the Postgres half,
// BenchmarkPostgresCatalogLookups, is the round trips the snapshot
// removes.
func BenchmarkCatalogLookups(b *testing.B) {
	benchCatalogLookups(b, memory.New())
}

// benchCatalogLookups stores one Provider with a sealed credential and
// one Model on st and times the two ways of resolving them.
func benchCatalogLookups(b *testing.B, st store.Store) {
	ctx := context.Background()
	keys, err := secrets.Parse("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "oai"},
		Spec: v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", Timeout: "10s",
			Credential: &v1.Credential{Header: "Authorization", Scheme: "Bearer"}},
		Status: v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, now, nil), Owner: "bench", Warnings: []string{},
			Credential: &v1.CredentialStatus{Set: true, Version: 1}},
	}
	if _, err := st.Objects().Put(ctx, p, 0); err != nil {
		b.Fatal(err)
	}
	row, err := keys.Seal(p.Status.ID, 1, []byte("sk-bench"))
	if err != nil {
		b.Fatal(err)
	}
	if err := st.Credentials().Put(ctx, p.Status.ID, row); err != nil {
		b.Fatal(err)
	}
	w := 100
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: "bench"},
		Spec:     v1.ModelSpec{Targets: []v1.Target{{Provider: "oai", Model: "x", Weight: &w}}, Fallback: v1.FallbackOnError},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, now, nil), Owner: "bench", Source: v1.SourceDeclared, Warnings: []string{}},
	}
	if _, err := st.Objects().Put(ctx, m, 0); err != nil {
		b.Fatal(err)
	}
	type catalog interface {
		gateway.Catalog
		gateway.CredentialSource
	}
	resolve := func(b *testing.B, c catalog) {
		for b.Loop() {
			got, err := c.Model(ctx, "bench")
			if err != nil || got == nil {
				b.Fatal(got, err)
			}
			prov, err := c.Provider(ctx, got.Spec.Targets[0].Provider)
			if err != nil || prov == nil {
				b.Fatal(prov, err)
			}
			if v, err := c.Credential(ctx, prov.Status.ID); err != nil || len(v) == 0 {
				b.Fatal(v, err)
			}
		}
	}
	b.Run("store", func(b *testing.B) {
		resolve(b, struct {
			*Catalog
			*StoreCredentials
		}{&Catalog{Objects: st.Objects()}, &StoreCredentials{Credentials: st.Credentials(), Keys: keys}})
	})
	b.Run("snapshot", func(b *testing.B) {
		snap := NewCatalogSnapshot(CatalogSnapshotOptions{Store: st, Keys: keys})
		if err := snap.Load(ctx, triggerStart); err != nil {
			b.Fatal(err)
		}
		resolve(b, snap)
	})
}
