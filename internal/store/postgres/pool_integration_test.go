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
