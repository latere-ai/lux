// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestServingPoolKeepsMigrationsDirect(t *testing.T) {
	s, err := Open(t.Context(), Options{URL: "postgres://migrator:secret@direct.example:5432/lux", PoolURL: "postgres://server:secret@pool.example:6432/lux-pool", MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cfg := s.pool.Config()
	if cfg.ConnConfig.Host != "pool.example" || cfg.ConnConfig.Port != 6432 || cfg.ConnConfig.Database != "lux-pool" {
		t.Fatal("serving did not select the pool URL")
	}
	if !strings.Contains(s.migrateURL, "direct.example:5432/lux") || strings.Contains(s.migrateURL, "pool.example") {
		t.Fatal("migration selected pooled endpoint")
	}
	if cfg.MinConns != 0 || cfg.MaxConns != 2 {
		t.Fatal("pool bounds ignored")
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec || cfg.ConnConfig.StatementCacheCapacity != 0 || cfg.ConnConfig.DescriptionCacheCapacity != 0 {
		t.Fatal("pooler mode retains prepared statement caches")
	}
	if s.Endpoint() != "pool.example:6432/lux-pool" {
		t.Fatalf("endpoint %q", s.Endpoint())
	}
	if _, err := Open(t.Context(), Options{URL: "postgres://direct/lux", PoolURL: "postgres://secret@%zz/lux"}); err == nil || strings.Contains(err.Error(), "secret@") {
		t.Fatalf("bad pool URL: %v", err)
	}
}
