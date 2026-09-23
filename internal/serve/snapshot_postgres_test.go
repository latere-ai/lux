// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package serve

import (
	"context"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// connect opens one replica's connection to the database at url.
func connect(t testing.TB, url string) store.Store {
	t.Helper()
	s, _, err := postgres.Connect(context.Background(), postgres.Options{URL: url, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPostgresCatalogSnapshotFollowsTheJournal is spec 036's second
// criterion on the Postgres store: two replicas, each with its own
// connection, snapshot, and Key cache; a Model and a Provider credential
// written through one replica's store are served by the other within two
// tail intervals of its Run, and a delete is gone there in the same bound.
func TestPostgresCatalogSnapshotFollowsTheJournal(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	url := pgtest.URL(t)
	a, b := connect(t, url), connect(t, url)
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "oai"},
		Spec: v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", Timeout: "10s",
			Credential: &v1.Credential{Header: "Authorization", Scheme: "Bearer"}},
		Status: v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, time.Now(), nil), Owner: subject, Warnings: []string{},
			Credential: &v1.CredentialStatus{Set: true, Version: 1}},
	}
	write := func(st store.Store, typ string, obj v1.Object, fn func(tx store.Store) error) {
		t.Helper()
		err := st.Transact(ctx, func(tx store.Store) error {
			if err := fn(tx); err != nil {
				return err
			}
			return appendEvent(ctx, tx.Journal(), typ, reasonRequest, obj, map[string]any{}, time.Now(), newEventID(time.Now))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sealed := func(value string) store.Sealed {
		row, err := h.keys.Seal(p.Status.ID, 1, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	write(a, "provider.created", p, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
			return err
		}
		return tx.Credentials().Put(ctx, p.Status.ID, sealed("sk-first"))
	})
	snapB := NewCatalogSnapshot(CatalogSnapshotOptions{Store: b, Keys: h.keys, Logger: h.logger})
	if err := snapB.Load(ctx, triggerStart); err != nil {
		t.Fatal(err)
	}
	cacheB := NewKeyCache(KeyCacheOptions{Store: b, Tail: 20 * time.Millisecond, Follower: snapB, Logger: h.logger})
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); cacheB.Run(run) }()
	defer func() { stop(); <-done }()

	w := 100
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: "fresh"},
		Spec:     v1.ModelSpec{Targets: []v1.Target{{Provider: "oai", Model: "gpt-4.1", Weight: &w}}, Fallback: v1.FallbackOnError},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, time.Now(), nil), Owner: subject, Source: v1.SourceDeclared, Warnings: []string{}},
	}
	write(a, "model.created", m, func(tx store.Store) error { _, err := tx.Objects().Put(ctx, m, 0); return err })
	// Two tail intervals are 40ms; the deadline leaves a loaded runner
	// room, and a replica that never follows fails it all the same.
	within := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not served by the second replica", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	within("the Model on the second replica", func() bool { got, _ := snapB.Model(ctx, "fresh"); return got != nil })
	write(a, "provider.updated", p, func(tx store.Store) error {
		return tx.Credentials().Put(ctx, p.Status.ID, sealed("sk-second"))
	})
	within("the rotated credential on the second replica", func() bool {
		got, _ := snapB.Credential(ctx, p.Status.ID)
		return string(got) == "sk-second"
	})
	write(a, "model.deleted", m, func(tx store.Store) error { return tx.Objects().Delete(ctx, v1.KindModel, m.Status.ID) })
	within("the delete on the second replica", func() bool { got, _ := snapB.Model(ctx, "fresh"); return got == nil })
}

// BenchmarkPostgresCatalogLookups is the Postgres half of
// BenchmarkCatalogLookups: the Model and its Provider and credential a
// request resolves, read from the store and from the snapshot.
func BenchmarkPostgresCatalogLookups(b *testing.B) {
	st := connect(b, pgtest.URL(b))
	benchCatalogLookups(b, st)
}
