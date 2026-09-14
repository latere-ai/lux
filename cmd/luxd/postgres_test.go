// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package main

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
)

// otherKEK is a second 32-byte key, the one a rotation moves away from.
const otherKEK = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="

// TestPostgresServeSelectsTheStore: with LUX_DB_URL serve migrates the
// database, prints the Postgres start-up line naming the endpoint and
// the schema version and never the password, answers readiness with the
// store joined to it, and a second start over the applied schema serves
// again; a schema ahead of the build prints one WARN line and serves.
func TestPostgresServeSelectsTheStore(t *testing.T) {
	dbURL := pgtest.URL(t)
	srv := startServe(t, map[string]string{"LUX_DB_URL": dbURL, "LUX_DB_MAX_CONNS": "3"})
	line := ""
	for l := range strings.SplitSeq(srv.out.String(), "\n") {
		if strings.Contains(l, "state in Postgres at ") {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, "at most 3 connections") || !strings.Contains(line, "schema at version "+strconv.FormatInt(postgres.Highest, 10)) || strings.Contains(srv.out.String(), "lux@") {
		t.Fatalf("the start-up line:\n%s", srv.out.String())
	}
	if status, body := get(t, srv.internalURL+"/readyz"); status != 200 {
		t.Fatalf("/readyz = %d %s", status, body)
	}
	if code := srv.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	again := startServe(t, map[string]string{"LUX_DB_URL": dbURL})
	if strings.Contains(again.out.String(), "WARN") {
		t.Fatalf("a second start over the applied schema warned:\n%s", again.out.String())
	}
	if code := again.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}

	pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET version = $1`, postgres.Highest+1)
	ahead := startServe(t, map[string]string{"LUX_DB_URL": dbURL})
	if !strings.Contains(ahead.out.String(), "luxd: WARN the schema is at version "+strconv.FormatInt(postgres.Highest+1, 10)) {
		t.Fatalf("no WARN line over a schema ahead:\n%s", ahead.out.String())
	}
	if code := ahead.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}

	pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET version = $1, dirty = true`, postgres.Highest)
	var errOut bytes.Buffer
	if code := run(t.Context(), nil, env(serveEnv(t, map[string]string{"LUX_DB_URL": dbURL})), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "dirty") {
		t.Fatalf("a dirty schema: exit %d, stderr %q", code, errOut.String())
	}
}

// TestPostgresRewrapRole is spec 005's role over its store: rows sealed
// under the old key are re-wrapped under the new first key and the run
// prints its summary, a second run is current, a row no key opens is
// named by provider id with exit 1, and a schema not at this build's
// version is refused before any row is read.
func TestPostgresRewrapRole(t *testing.T) {
	ctx := t.Context()
	dbURL := pgtest.URL(t)
	st, _, err := postgres.Connect(ctx, postgres.Options{URL: dbURL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	old, err := secrets.Parse(otherKEK)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"prv_a", "prv_b"} {
		row, err := old.Seal(id, 1, []byte("sk-canary-"+id))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Credentials().Put(ctx, id, row); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	vars := env(map[string]string{"LUX_SECRETS_KEK": kek + "," + otherKEK, "LUX_DB_URL": dbURL})
	if code := run(ctx, []string{"rewrap"}, vars, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errOut.String())
	}
	if got := out.String(); got != "luxd: rewrap: 2 re-wrapped, 0 already current, 0 unopenable\n" || errOut.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q", got, errOut.String())
	}
	fresh, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.Credentials().Get(ctx, "prv_a")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := fresh.Open("prv_a", row); err != nil || string(v) != "sk-canary-prv_a" {
		t.Fatalf("the row does not open under the new key alone: %q, %v", v, err)
	}
	out.Reset()
	if code := run(ctx, []string{"rewrap"}, vars, &out, &errOut); code != 0 || !strings.Contains(out.String(), "0 re-wrapped, 2 already current") {
		t.Fatalf("second run: exit %d, %s", code, out.String())
	}
	// A row under a key nobody lists is named and fails the run.
	lost, err := secrets.Parse("CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQk=")
	if err != nil {
		t.Fatal(err)
	}
	unopenable, err := lost.Seal("prv_lost", 1, []byte("sk-canary-lost"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Credentials().Put(ctx, "prv_lost", unopenable); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run(ctx, []string{"rewrap"}, vars, &out, &errOut); code != 1 || !strings.Contains(out.String(), "1 unopenable") || !strings.HasPrefix(errOut.String(), "rewrap: provider prv_lost: ") || strings.Contains(errOut.String(), "sk-canary") {
		t.Fatalf("an unopenable row: exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	// A schema behind this build is refused before any row is read.
	pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET version = 1000001`)
	errOut.Reset()
	if code := run(ctx, []string{"rewrap"}, vars, io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "the schema is at version 1000001 and this build's is "+strconv.FormatInt(postgres.Highest, 10)) {
		t.Fatalf("a schema behind: exit %d, stderr %q", code, errOut.String())
	}
	pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET version = $1, dirty = true`, postgres.Highest)
	errOut.Reset()
	if code := run(ctx, []string{"rewrap"}, vars, io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "dirty") {
		t.Fatalf("a dirty schema: exit %d, stderr %q", code, errOut.String())
	}
}
