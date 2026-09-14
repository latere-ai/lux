// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package e2e

import (
	"fmt"
	"os"
	"testing"
)

// The postgres tier of spec 015: the same tree of cases with LUX_DB_URL
// set, plus the cases that only exist with a shared store. It is
// selected by the postgres tag and the TestPostgres prefix:
//
//	LUX_DB_URL=postgres://... go test -tags=postgres -run '^TestPostgres' -v ./...
//
// The database is a dependency of the tier, declared by the variable and
// never started here.

// unsetDBURL is the one line an unset LUX_DB_URL fails with: the variable
// and one way to satisfy it.
const unsetDBURL = "LUX_DB_URL is unset and the postgres tier needs a database: docker run --rm -p 5432:5432 -e POSTGRES_PASSWORD=lux postgres:17-alpine, then LUX_DB_URL=postgres://postgres:lux@127.0.0.1:5432/postgres?sslmode=disable"

// TestMain refuses to run without LUX_DB_URL: a tier that silently skips
// is a tier nobody notices is not running.
func TestMain(m *testing.M) {
	if os.Getenv("LUX_DB_URL") == "" {
		_, _ = fmt.Fprintln(os.Stderr, unsetDBURL)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestPostgresTwoReplicas runs two luxd processes against one database
// and proves the shared lease, the shared spend counter, the
// journal-driven Key cache invalidation, and the resumed event. The
// Postgres store is spec 010's sixth phase and not in this build: luxd
// refuses LUX_DB_URL at start, so the case waits on it and says so.
func TestPostgresTwoReplicas(t *testing.T) {
	t.Skip("the Postgres store is spec 010's phase 6 and not in this build; luxd refuses LUX_DB_URL until it lands")
}
