// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lux/internal/store/postgres/pgtest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// A serving role without CREATE privileges proves migrations use the direct
// endpoint. Pointing migrations at PoolURL makes this start fail.
func TestPostgresPooledServingRoleDoesNotRunMigrations(t *testing.T) {
	db := pgtest.URL(t)
	role := "lux_serve_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	identifier := pgx.Identifier{role}.Sanitize()
	pgtest.Exec(t, db, `CREATE ROLE `+identifier+` LOGIN PASSWORD 'pool-test'`)
	t.Cleanup(func() {
		pgtest.Exec(t, db, `DROP OWNED BY `+identifier)
		pgtest.Exec(t, db, `DROP ROLE `+identifier)
	})
	pooled, err := url.Parse(db)
	if err != nil {
		t.Fatal(err)
	}
	pooled.User = url.UserPassword(role, "pool-test")
	s, warning, err := Connect(t.Context(), Options{URL: db, PoolURL: pooled.String(), MaxConns: 1})
	if err != nil || warning != "" {
		t.Fatalf("connect: %s %v", warning, err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.pool.Exec(t.Context(), `CREATE TABLE must_not_create(id int)`); err == nil {
		t.Fatal("serving role has migration privileges; test does not prove separation")
	}
	pgtest.Exec(t, db, `GRANT SELECT ON ALL TABLES IN SCHEMA public TO `+identifier)
	schema, err := s.Schema(t.Context())
	if err != nil || schema.Version != Highest {
		t.Fatalf("serving cannot read the migrated schema: %+v %v", schema, err)
	}
	var current string
	if err := s.pool.QueryRow(t.Context(), `SELECT current_user`).Scan(&current); err != nil || current != role {
		t.Fatalf("serving role = %q: %v", current, err)
	}
}

// TestPostgresPooledStoreWritesLabeledObjects writes and reads back a
// labeled Provider and a Model through the serving pool's connection mode.
// A mode that infers parameter types from Go types cannot encode the labels
// map or the JSON columns, so every write with labels failed there.
func TestPostgresPooledStoreWritesLabeledObjects(t *testing.T) {
	db := pgtest.URL(t)
	s, warning, err := Connect(t.Context(), Options{URL: db, PoolURL: db, MaxConns: 1})
	if err != nil || warning != "" {
		t.Fatalf("connect: %s %v", warning, err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now()
	labels := map[string]string{"example.com/tier": "catalog"}
	provider := &v1.Provider{Metadata: v1.ObjectMeta{Name: "pooled", Labels: labels}, Spec: v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://upstream.example/v1"},
		Status: v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, now, nil), Owner: "issuer|owner"}}
	if _, err := s.Objects().Put(t.Context(), provider, 0); err != nil {
		t.Fatalf("put a labeled Provider through the pool: %v", err)
	}
	model := &v1.Model{Metadata: v1.ObjectMeta{Name: "pooled/model", Labels: labels}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: "pooled", Model: "m"}}},
		Status: v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, now, nil), Owner: "issuer|owner"}}
	if _, err := s.Objects().Put(t.Context(), model, 0); err != nil {
		t.Fatalf("put a labeled Model through the pool: %v", err)
	}
	obj, _, err := s.Objects().ByName(t.Context(), v1.KindProvider, "pooled")
	got, ok := obj.(*v1.Provider)
	if err != nil || !ok || got.Metadata.Labels["example.com/tier"] != "catalog" || got.Spec.BaseURL != "https://upstream.example/v1" {
		t.Fatalf("read back %+v: %v", obj, err)
	}
}
