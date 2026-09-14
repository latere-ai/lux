// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package check

import (
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
)

// TestPostgresCheckRows is the store, migrations, and db conns rows over
// a real database: an empty one answers SELECT 1 and reports the schema
// as not applied with the rows over the objects unchecked, applies
// nothing itself, then reads the applied schema and the cluster's
// connection limits, and fails the db conns row when one replica's pool
// alone would exceed what the cluster leaves.
func TestPostgresCheckRows(t *testing.T) {
	s := newStack(t)
	dbURL := pgtest.URL(t)
	m := s.serverEnv()
	m["LUX_DB_URL"] = dbURL
	highest := strconv.FormatInt(postgres.Highest, 10)

	fresh := byName(Lines(t.Context(), Options{Getenv: env(m)}))
	if fresh["store"].State != OK || !strings.Contains(fresh["store"].Detail, "answered SELECT 1") {
		t.Errorf("store over an empty database: %s", fresh["store"])
	}
	if fresh["migrations"].State != OK || !strings.Contains(fresh["migrations"].Detail, "not applied yet") || !strings.Contains(fresh["migrations"].Detail, highest) {
		t.Errorf("migrations over an empty database: %s", fresh["migrations"])
	}
	if fresh["providers"].State != Warn || !strings.Contains(fresh["providers"].Detail, "not checked; the schema is not applied yet") {
		t.Errorf("providers over an empty database: %s", fresh["providers"])
	}
	if fresh["db conns"].State != OK || !strings.Contains(fresh["db conns"].Detail, "max_connections is") {
		t.Errorf("db conns: %s", fresh["db conns"])
	}
	if n := pgtest.Count(t, dbURL, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`); n != 0 {
		t.Fatalf("check applied %d table(s); it applies nothing", n)
	}

	st, _, err := postgres.Connect(t.Context(), postgres.Options{URL: dbURL})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	applied := byName(Lines(t.Context(), Options{Getenv: env(m)}))
	if applied["migrations"].State != OK || !strings.Contains(applied["migrations"].Detail, "at version "+highest) {
		t.Errorf("migrations over the applied schema: %s", applied["migrations"])
	}
	if applied["providers"].State != OK || !strings.Contains(applied["providers"].Detail, "none declared") {
		t.Errorf("providers over the applied schema: %s", applied["providers"])
	}
	if applied["credentials"].State != OK {
		t.Errorf("credentials over the applied schema: %s", applied["credentials"])
	}

	m["LUX_DB_MAX_CONNS"] = "100"
	crowded := byName(Lines(t.Context(), Options{Getenv: env(m)}))
	if crowded["db conns"].State != Fail || !strings.Contains(crowded["db conns"].Detail, "one replica alone exceeds it") {
		t.Errorf("db conns with a pool of 100: %s", crowded["db conns"])
	}
	delete(m, "LUX_DB_MAX_CONNS")

	pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET dirty = true`)
	dirty := byName(Lines(t.Context(), Options{Getenv: env(m)}))
	if dirty["migrations"].State != Fail || !strings.Contains(dirty["migrations"].Detail, "dirty") {
		t.Errorf("migrations over a dirty schema: %s", dirty["migrations"])
	}
	if dirty["providers"].State != Warn || !strings.Contains(dirty["providers"].Detail, "would refuse the schema") {
		t.Errorf("providers over a dirty schema: %s", dirty["providers"])
	}
	for _, lines := range []map[string]Line{fresh, applied, crowded, dirty} {
		for _, l := range lines {
			if strings.Contains(l.Detail, "lux@") || strings.Contains(l.Detail, "postgres://") {
				t.Errorf("%s carries the URL", l)
			}
		}
	}
}
