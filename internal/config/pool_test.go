// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestPooledDatabaseConfiguration(t *testing.T) {
	const direct = "postgres://migrator:secret@db.example:5432/lux"
	const pool = "postgres://server:secret@pool.example:6432/lux-pool"
	base := map[string]string{"LUX_OIDC_ISSUERS": issuer, "LUX_SECRETS_KEK": kek, "LUX_DB_URL": direct, "LUX_DB_POOL_URL": pool}
	cfg, err := Load(env(base))
	if err != nil || cfg.DBURL != direct || cfg.DBPoolURL != pool {
		t.Fatalf("configuration: %+v %v", cfg, err)
	}
	rewrap, err := LoadRewrap(env(base))
	if err != nil || rewrap.DBPoolURL != pool {
		t.Fatalf("rewrap: %v", err)
	}
	for _, bad := range []string{"mysql://user:secret@pool.example/lux", "postgres://user:secret@%zz/lux"} {
		base["LUX_DB_POOL_URL"] = bad
		if _, err := Load(env(base)); err == nil || !strings.Contains(err.Error(), "LUX_DB_POOL_URL") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad URL: %v", err)
		}
		if _, err := LoadRewrap(env(base)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("bad rewrap URL: %v", err)
		}
	}
	base["LUX_DB_POOL_URL"] = pool
	delete(base, "LUX_DB_URL")
	if _, err := Load(env(base)); err == nil || !strings.Contains(err.Error(), "requires LUX_DB_URL") {
		t.Fatalf("pool without migration URL: %v", err)
	}
}
