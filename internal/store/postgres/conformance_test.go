// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres_test

import (
	"testing"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
	"latere.ai/x/lux/internal/store/storetest"
)

// open connects a store to a fresh database, migrated, and closes it
// with the test.
func open(t *testing.T) *postgres.Store {
	t.Helper()
	st, warning, err := postgres.Connect(t.Context(), postgres.Options{URL: pgtest.URL(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if warning != "" {
		t.Fatalf("a fresh database warned: %s", warning)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestPostgresStoreConformance is spec 010's suite over the Postgres
// store: every case of storetest.Run, each against a database of its
// own, so the store that runs against memory in the untagged run runs
// against Postgres here.
func TestPostgresStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return open(t) })
}
