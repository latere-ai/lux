// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/postgres/pgtest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The spec's own Postgres rows, each against a database of its own:
// what a restart keeps, the four schema guards on a real
// schema_migrations table and the migration from the previous schema,
// every index earning its keep, the pool's ceiling, two stores sharing
// one database, and a database that goes away while the store serves.

const subject = "https://login.example.com|alice"

// connect opens a migrated store on dbURL and closes it with the test.
func connect(t *testing.T, dbURL string, maxConns int) *Store {
	t.Helper()
	st, warning, err := Connect(t.Context(), Options{URL: dbURL, MaxConns: maxConns})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if warning != "" {
		t.Fatalf("Connect warned: %s", warning)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func provider(name string) *v1.Provider {
	return &v1.Provider{
		Metadata: v1.ObjectMeta{Name: name, Labels: map[string]string{"tier": "gold"}},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1"},
		Status:   v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, time.Now(), nil), Owner: subject, Warnings: []string{}},
	}
}

func model(name string, targets ...string) *v1.Model {
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: name},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, time.Now(), nil), Owner: subject, Source: v1.SourceDeclared, Warnings: []string{}},
	}
	for _, p := range targets {
		m.Spec.Targets = append(m.Spec.Targets, v1.Target{Provider: p, Model: name})
	}
	return m
}

// hash is a 64-character lower-case hex string built from a seed.
func hash(seed byte) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hex[(int(seed)+i)%16]
	}
	return string(b)
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPostgresRestartKeepsState: after the store is closed and opened
// again on the same database, every object, key hash, credential, open
// spend window, pending event, lease, tunnel row, and aggregate is what
// it was, and the version keeps counting from where it stopped.
func TestPostgresRestartKeepsState(t *testing.T) {
	ctx := t.Context()
	dbURL := pgtest.URL(t)
	first := connect(t, dbURL, 0)
	p := provider("openai")
	if _, err := first.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Objects().Put(ctx, p, 1); err != nil {
		t.Fatal(err)
	}
	if err := first.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy}}); err != nil {
		t.Fatal(err)
	}
	sealed := store.Sealed{Version: 1, WrappedKey: []byte("wk"), WrappedNonce: []byte("wn"), Ciphertext: []byte("ct"), Nonce: []byte("n")}
	if err := first.Credentials().Put(ctx, p.Status.ID, sealed); err != nil {
		t.Fatal(err)
	}
	if err := first.Keys().Put(ctx, "key_1", hash(1)); err != nil {
		t.Fatal(err)
	}
	window := time.Now().Add(time.Hour)
	if _, err := first.Counters().Add(ctx, "key:key_1:spend:1", 700, window); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Counters().Add(ctx, "key:key_1:spend:none", 900, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: p.Status.ID, Type: "provider.created", Payload: []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if held, err := first.Leases().Acquire(ctx, store.LeaseJournal, "replica-a", time.Hour); err != nil || !held {
		t.Fatalf("Acquire = %v, %v", held, err)
	}
	if err := first.Tunnels().Register(ctx, store.Tunnel{ProviderID: p.Status.ID, Session: "tun_1"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	hour := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	if err := first.Usage().AddRows(ctx, []metering.Aggregate{{Bucket: hour, KeyID: "key_1", Owner: subject, Status: metering.StatusOK, Currency: "USD", Sums: metering.Sums{Requests: 3, Cost: 50}}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := connect(t, dbURL, 0)
	got, version, err := second.Objects().ByName(ctx, v1.KindProvider, "openai")
	if err != nil || version != 2 {
		t.Fatalf("ByName after the restart = %v, %d", err, version)
	}
	if gp := got.(*v1.Provider); gp.Status.ID != p.Status.ID || gp.Status.Health == nil || gp.Status.Health.State != v1.HealthHealthy || gp.Metadata.Labels["tier"] != "gold" {
		t.Fatalf("the Provider after the restart = %+v", gp.Status)
	}
	if _, err := second.Objects().Put(ctx, p, 2); err != nil {
		t.Fatalf("Put at the kept version: %v", err)
	}
	if row, err := second.Credentials().Get(ctx, p.Status.ID); err != nil || string(row.Ciphertext) != "ct" || row.Version != 1 {
		t.Fatalf("the credential after the restart = %+v, %v", row, err)
	}
	if id, err := second.Keys().ByHash(ctx, hash(1)); err != nil || id != "key_1" {
		t.Fatalf("the hash after the restart = %q, %v", id, err)
	}
	counters, err := second.Counters().Read(ctx, []string{"key:key_1:spend:1", "key:key_1:spend:none"})
	if err != nil || counters["key:key_1:spend:1"] != 700 || counters["key:key_1:spend:none"] != 900 {
		t.Fatalf("the counters after the restart = %v, %v", counters, err)
	}
	if n, err := second.Counters().Prune(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("the open window was pruned: %d, %v", n, err)
	}
	pending, err := second.Journal().Pending(ctx, 0)
	if err != nil || len(pending) != 1 || pending[0].ID != "evt_1" || string(pending[0].Payload) != `{"a":1}` || pending[0].GSeq != 1 {
		t.Fatalf("the pending event after the restart = %+v, %v", pending, err)
	}
	if held, err := second.Leases().Acquire(ctx, store.LeaseJournal, "replica-b", time.Hour); err != nil || held {
		t.Fatalf("the lease was lost across the restart: %v, %v", held, err)
	}
	if row, err := second.Tunnels().Get(ctx, p.Status.ID); err != nil || row.Session != "tun_1" {
		t.Fatalf("the tunnel row after the restart = %+v, %v", row, err)
	}
	rows, err := second.Usage().QueryRows(ctx, metering.Query{From: hour, To: hour.Add(time.Hour)})
	if err != nil || len(rows) != 1 || rows[0].Requests != 3 || rows[0].Cost != 50 {
		t.Fatalf("the aggregates after the restart = %+v, %v", rows, err)
	}
	if _, err := second.Journal().Append(ctx, store.Event{ID: "evt_2", ObjectID: p.Status.ID, Type: "provider.updated"}); err != nil {
		t.Fatal(err)
	}
	if all, err := second.Journal().Since(ctx, 0, 0); err != nil || len(all) != 2 || all[1].GSeq != 2 || all[1].Seq != 2 {
		t.Fatalf("the sequence after the restart = %+v, %v", all, err)
	}
	if !strings.Contains(second.Notice(ctx), "schema at version "+strconv.FormatInt(Highest, 10)) {
		t.Errorf("Notice = %q", second.Notice(ctx))
	}
}

// TestPostgresSchemaGuards is the table of spec 010 over a real
// schema_migrations table: an empty database and one at the previous
// schema are migrated to the highest version; a schema ahead within the
// major starts with one warning naming both and serves; a dirty schema
// and one of another major refuse to start naming the version.
func TestPostgresSchemaGuards(t *testing.T) {
	ctx := t.Context()
	first, err := fs.ReadFile(migrations, migrationsDir+"/1000001_init.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("from an empty database", func(t *testing.T) {
		dbURL := pgtest.URL(t)
		st := connect(t, dbURL, 0)
		if sch, err := st.Schema(ctx); err != nil || sch.Version != Highest || sch.Dirty {
			t.Fatalf("Schema after Connect = %+v, %v", sch, err)
		}
		// A second start over the applied schema applies nothing and serves.
		again := connect(t, dbURL, 0)
		if err := again.Ready(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("from the previous schema", func(t *testing.T) {
		dbURL := pgtest.URL(t)
		mURL, err := migrateURL(dbURL)
		if err != nil {
			t.Fatal(err)
		}
		if err := pgxmigrate.Up(mURL, fstest.MapFS{"1000001_init.up.sql": {Data: first}}, "."); err != nil {
			t.Fatalf("applying the first migration alone: %v", err)
		}
		before, err := Open(ctx, Options{URL: dbURL})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = before.Close() }()
		if sch, err := before.Schema(ctx); err != nil || sch.Version != 1000001 {
			t.Fatalf("Schema at the previous version = %+v, %v", sch, err)
		}
		if action, _ := Guard(Schema{Version: 1000001}); action != Migrate {
			t.Fatalf("Guard of the previous version = %s", action)
		}
		st := connect(t, dbURL, 0)
		if sch, err := st.Schema(ctx); err != nil || sch.Version != Highest {
			t.Fatalf("Schema after the migration from the previous version = %+v, %v", sch, err)
		}
		hour := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
		if err := st.Usage().AddRows(ctx, []metering.Aggregate{{Bucket: hour, KeyID: "key_1", Sums: metering.Sums{Requests: 1}}}); err != nil {
			t.Fatalf("the table the later migration adds: %v", err)
		}
	})
	t.Run("ahead within the major", func(t *testing.T) {
		dbURL := pgtest.URL(t)
		connect(t, dbURL, 0)
		pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET version = $1`, Highest+1)
		st, warning, err := Connect(ctx, Options{URL: dbURL})
		if err != nil {
			t.Fatalf("Connect over a schema ahead: %v", err)
		}
		defer func() { _ = st.Close() }()
		for _, want := range []string{strconv.FormatInt(Highest+1, 10), strconv.FormatInt(Highest, 10), "ahead"} {
			if !strings.Contains(warning, want) {
				t.Errorf("the warning does not name %q: %s", want, warning)
			}
		}
		if _, err := st.Objects().Put(ctx, provider("openai"), 0); err != nil {
			t.Fatalf("serving over a schema ahead: %v", err)
		}
		if sch, err := st.Schema(ctx); err != nil || sch.Version != Highest+1 {
			t.Fatalf("the schema was touched: %+v, %v", sch, err)
		}
	})
	t.Run("dirty", func(t *testing.T) {
		dbURL := pgtest.URL(t)
		connect(t, dbURL, 0)
		pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET dirty = true`)
		st, _, err := Connect(ctx, Options{URL: dbURL})
		if err == nil {
			_ = st.Close()
			t.Fatal("Connect over a dirty schema succeeded")
		}
		if !strings.Contains(err.Error(), "dirty") || !strings.Contains(err.Error(), strconv.FormatInt(Highest, 10)) || !strings.HasPrefix(err.Error(), "LUX_DB_URL: ") {
			t.Errorf("error = %v", err)
		}
		open, err := Open(ctx, Options{URL: dbURL})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = open.Close() }()
		if sch, err := open.Schema(ctx); err != nil || !sch.Dirty {
			t.Fatalf("Schema = %+v, %v", sch, err)
		}
	})
	t.Run("another major", func(t *testing.T) {
		dbURL := pgtest.URL(t)
		connect(t, dbURL, 0)
		pgtest.Exec(t, dbURL, `UPDATE schema_migrations SET version = 2000001`)
		st, _, err := Connect(ctx, Options{URL: dbURL})
		if err == nil {
			_ = st.Close()
			t.Fatal("Connect over another major succeeded")
		}
		if !strings.Contains(err.Error(), "2000001") || !strings.Contains(err.Error(), "major 2") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("an empty migrations table", func(t *testing.T) {
		dbURL := pgtest.URL(t)
		pgtest.Exec(t, dbURL, `CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`)
		open, err := Open(ctx, Options{URL: dbURL})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = open.Close() }()
		if sch, err := open.Schema(ctx); err != nil || sch.Version != 0 {
			t.Fatalf("Schema over an empty table = %+v, %v", sch, err)
		}
	})
}

// TestPostgresQueriesUseIndexes: with sequential scans turned off for
// the database, so the planner picks an index whenever one serves, every
// operation of every collection runs once and every index of the schema
// has been scanned by the end, which is what proves each list, lookup,
// count, and upsert has the index the table says and that no index is
// dead weight.
func TestPostgresQueriesUseIndexes(t *testing.T) {
	ctx := t.Context()
	dbURL := pgtest.URL(t)
	name := strings.TrimPrefix(mustURL(t, dbURL).Path, "/")
	pgtest.Exec(t, dbURL, `ALTER DATABASE `+quote(name)+` SET enable_seqscan = off`)
	// One connection, so every statement's statistics are flushed by the
	// statement that reads them.
	st := connect(t, dbURL, 1)
	// A few thousand live Models of another owner, label, and Provider,
	// so a filter has rows to leave out and the planner's estimates make
	// the selective index the cheaper path, as they do in an installation.
	pgtest.Exec(t, dbURL, `
		INSERT INTO objects (kind, id, name, owner, source, version, spec, status, labels, providers, created_at, updated_at)
		SELECT 'Model', 'mdl_bulk' || n, 'bulk-' || n, 'https://login.example.com|bulk', 'declared', 1,
			'{"metadata":{"name":"bulk"},"spec":{}}', '{}', '{"tier":"bulk"}', '{bulk}', now(), now()
		FROM generate_series(1, 3000) AS n`)
	pgtest.Exec(t, dbURL, `ANALYZE objects`)
	pgtest.Exec(t, dbURL, `SELECT pg_stat_reset()`)

	p := provider("openai")
	m := model("gpt-5", "openai")
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err := st.Objects().Put(ctx, p, 0)
	must("Put provider", err)
	_, err = st.Objects().Put(ctx, m, 0)
	must("Put model", err)
	_, err = st.Objects().Put(ctx, p, 1)
	must("Put update", err)
	_, _, err = st.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	must("Get", err)
	_, _, err = st.Objects().ByName(ctx, v1.KindProvider, "openai")
	must("ByName", err)
	for _, f := range []store.Filter{{}, {Owner: subject}, {Source: "declared"}, {Labels: map[string]string{"tier": "gold"}}, {Provider: p.Status.ID}, {IDs: []string{m.Status.ID}}} {
		_, _, err = st.Objects().List(ctx, v1.KindModel, f, store.Page{Limit: 10})
		must("List", err)
	}
	must("PutStatus", st.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Available: new(true)}))
	must("Delete", st.Objects().Delete(ctx, v1.KindModel, m.Status.ID))
	_, err = st.Objects().Prune(ctx, time.Now().Add(time.Hour))
	must("Prune", err)
	must("Keys.Put", st.Keys().Put(ctx, "key_1", hash(1)))
	_, err = st.Keys().ByHash(ctx, hash(1))
	must("ByHash", err)
	must("Keys.Delete", st.Keys().Delete(ctx, "key_1"))
	must("Credentials.Put", st.Credentials().Put(ctx, p.Status.ID, store.Sealed{Version: 1}))
	must("Rewrap", st.Credentials().Rewrap(ctx, p.Status.ID, 1, []byte("k"), []byte("n")))
	_, err = st.Credentials().Get(ctx, p.Status.ID)
	must("Credentials.Get", err)
	_, err = st.Credentials().List(ctx)
	must("Credentials.List", err)
	_, err = st.Counters().Add(ctx, "key:key_1:spend:1", 1, time.Now().Add(-time.Second))
	must("Counters.Add", err)
	_, err = st.Counters().Read(ctx, []string{"key:key_1:spend:1"})
	must("Counters.Read", err)
	_, err = st.Counters().Prune(ctx, time.Now())
	must("Counters.Prune", err)
	_, err = st.Leases().Acquire(ctx, store.LeaseHealth, "a", time.Minute)
	must("Acquire", err)
	must("Release", st.Leases().Release(ctx, store.LeaseHealth, "a"))
	for _, e := range []store.Event{{ID: "evt_1", ObjectID: p.Status.ID}, {ID: "evt_2", ObjectID: p.Status.ID}, {ID: "evt_3", ObjectID: "key_9"}} {
		_, err = st.Journal().Append(ctx, e)
		must("Append", err)
	}
	_, err = st.Journal().Pending(ctx, 10)
	must("Pending", err)
	_, _, err = st.Journal().ByObject(ctx, p.Status.ID, store.Page{Limit: 1})
	must("ByObject", err)
	_, err = st.Journal().Since(ctx, 1, 10)
	must("Since", err)
	must("Acknowledge", st.Journal().Acknowledge(ctx, "evt_1"))
	must("Defer", st.Journal().Defer(ctx, "evt_2", 1, time.Now()))
	must("Drop", st.Journal().Drop(ctx, "evt_3"))
	_, err = st.Journal().Prune(ctx, time.Now().Add(-time.Hour))
	must("Journal.Prune", err)
	must("Register", st.Tunnels().Register(ctx, store.Tunnel{ProviderID: p.Status.ID, Session: "tun_1"}, time.Minute))
	_, err = st.Tunnels().Heartbeat(ctx, p.Status.ID, "tun_1", time.Minute)
	must("Heartbeat", err)
	_, err = st.Tunnels().Get(ctx, p.Status.ID)
	must("Tunnels.Get", err)
	must("Unregister", st.Tunnels().Unregister(ctx, p.Status.ID, "tun_1"))
	hour := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	rows := []metering.Aggregate{{Bucket: hour, KeyID: "key_1", Sums: metering.Sums{Requests: 1}}}
	must("AddRows", st.Usage().AddRows(ctx, rows))
	must("AddRows again", st.Usage().AddRows(ctx, rows))
	_, err = st.Usage().QueryRows(ctx, metering.Query{From: hour, To: hour.Add(time.Hour), Keys: []string{"key_1"}})
	must("QueryRows", err)

	// The statistics of every index, flushed by the read.
	var unused []string
	deadline := time.Now().Add(10 * time.Second)
	for {
		unused = nil
		conn := st.pool
		_, _ = conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`)
		// golang-migrate's table is its own, read whole and never by key.
		rows, err := conn.Query(ctx, `SELECT indexrelname, idx_scan FROM pg_stat_user_indexes WHERE relname <> 'schema_migrations' ORDER BY indexrelname`)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var index string
			var scans int64
			if err := rows.Scan(&index, &scans); err != nil {
				t.Fatal(err)
			}
			n++
			if scans == 0 {
				unused = append(unused, index)
			}
		}
		rows.Close()
		if n < 19 {
			t.Fatalf("%d indexes, want the schema's 19 or more", n)
		}
		if len(unused) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexes no operation scanned: %v", unused)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func quote(ident string) string { return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"` }

// TestPostgresPoolIsBounded: a pool of three holds at most three
// connections under forty concurrent callers, sampled from outside the
// pool, and after Connect the migrator's own connection is gone.
func TestPostgresPoolIsBounded(t *testing.T) {
	ctx := t.Context()
	dbURL := pgtest.URL(t)
	st := connect(t, dbURL, 3)
	if st.pool.Config().MaxConns != 3 || st.MaxConns() != 3 {
		t.Fatalf("MaxConns = %d", st.pool.Config().MaxConns)
	}
	name := strings.TrimPrefix(mustURL(t, dbURL).Path, "/")
	const count = `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND application_name = $2`
	eventually(t, "the migrator releasing its connection", 5*time.Second, func() bool {
		return pgtest.Count(t, dbURL, count, name, applicationName) <= 3
	})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := st.Counters().Add(ctx, "key:k:requests:0", 1, time.Time{}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	most := 0
	for range 20 {
		time.Sleep(25 * time.Millisecond)
		most = max(most, pgtest.Count(t, dbURL, count, name, applicationName))
	}
	close(stop)
	wg.Wait()
	if most == 0 || most > 3 {
		t.Fatalf("the pool held %d connections at most, want between 1 and 3", most)
	}
}

// TestPostgresReplicasShareOneStore: two stores over one database are
// one installation: an object one writes the other reads at once, a
// thousand counter adds split between them sum on both, one lease has
// one holder, and the journal and the tunnel registry are one.
func TestPostgresReplicasShareOneStore(t *testing.T) {
	ctx := t.Context()
	dbURL := pgtest.URL(t)
	a, b := connect(t, dbURL, 4), connect(t, dbURL, 4)
	p := provider("openai")
	if _, err := a.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	got, version, err := b.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	if err != nil || version != 1 || got.Name() != "openai" {
		t.Fatalf("b reads a's object = %v, %d, %v", got, version, err)
	}
	if _, err := b.Objects().Put(ctx, p, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Objects().Put(ctx, p, 1); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("a's stale write = %v", err)
	}

	var wg sync.WaitGroup
	for _, st := range []*Store{a, b} {
		for range 500 {
			wg.Go(func() {
				if _, err := st.Counters().Add(ctx, "key:key_1:spend:1", 1, time.Now().Add(time.Hour)); err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	for name, st := range map[string]*Store{"a": a, "b": b} {
		read, err := st.Counters().Read(ctx, []string{"key:key_1:spend:1"})
		if err != nil || read["key:key_1:spend:1"] != 1000 {
			t.Fatalf("%s reads the counter as %v, %v", name, read, err)
		}
	}

	if held, err := a.Leases().Acquire(ctx, store.LeaseDiscovery, "a", time.Hour); err != nil || !held {
		t.Fatalf("a acquires: %v, %v", held, err)
	}
	if held, err := b.Leases().Acquire(ctx, store.LeaseDiscovery, "b", time.Hour); err != nil || held {
		t.Fatalf("b acquires a's lease: %v, %v", held, err)
	}
	if _, err := a.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: p.Status.ID, Type: "provider.created"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Journal().Append(ctx, store.Event{ID: "evt_2", ObjectID: p.Status.ID, Type: "provider.updated"}); err != nil {
		t.Fatal(err)
	}
	all, err := a.Journal().Since(ctx, 0, 0)
	if err != nil || len(all) != 2 || all[0].GSeq != 1 || all[1].GSeq != 2 || all[1].Seq != 2 {
		t.Fatalf("the journal from both = %+v, %v", all, err)
	}
	if err := a.Tunnels().Register(ctx, store.Tunnel{ProviderID: p.Status.ID, Session: "tun_a", Replica: "a"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if row, err := b.Tunnels().Get(ctx, p.Status.ID); err != nil || row.Replica != "a" {
		t.Fatalf("b reads the registry = %+v, %v", row, err)
	}
	if held, err := b.Tunnels().Heartbeat(ctx, p.Status.ID, "tun_a", time.Hour); err != nil || !held {
		t.Fatalf("b renews a's session = %v, %v", held, err)
	}
	if err := a.Credentials().Put(ctx, p.Status.ID, store.Sealed{Version: 1, Ciphertext: []byte("ct")}); err != nil {
		t.Fatal(err)
	}
	if row, err := b.Credentials().Get(ctx, p.Status.ID); err != nil || string(row.Ciphertext) != "ct" {
		t.Fatalf("b reads a's credential = %+v, %v", row, err)
	}
}

// TestPostgresTransactIsolation: a Transact's writes are its own until
// it commits, a panic inside fn rolls everything back, and a write the
// contract refuses inside fn leaves the transaction usable, as it does
// on the memory store.
func TestPostgresTransactIsolation(t *testing.T) {
	ctx := t.Context()
	st := connect(t, pgtest.URL(t), 0)
	p := provider("openai")
	err := st.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
			return err
		}
		if _, _, err := tx.Objects().Get(ctx, v1.KindProvider, p.Status.ID); err != nil {
			return err
		}
		if _, _, err := st.Objects().Get(ctx, v1.KindProvider, p.Status.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an uncommitted write is visible outside: %v", err)
		}
		if err := tx.Ready(ctx); err != nil {
			return err
		}
		// A refused write does not poison the transaction.
		if _, err := tx.Objects().Put(ctx, provider("openai"), 0); !errors.Is(err, store.ErrNameTaken) {
			t.Errorf("the second create = %v", err)
		}
		if err := tx.Keys().Put(ctx, "key_1", hash(1)); err != nil {
			return err
		}
		if err := tx.Keys().Put(ctx, "key_2", hash(1)); !errors.Is(err, store.ErrHashTaken) {
			t.Errorf("the taken hash = %v", err)
		}
		_, err := tx.Objects().Put(ctx, provider("azure"), 0)
		return err
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}
	for _, name := range []string{"openai", "azure"} {
		if _, _, err := st.Objects().ByName(ctx, v1.KindProvider, name); err != nil {
			t.Fatalf("%s after the commit: %v", name, err)
		}
	}
	if id, err := st.Keys().ByHash(ctx, hash(1)); err != nil || id != "key_1" {
		t.Fatalf("the hash after the commit = %q, %v", id, err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not propagate")
			}
		}()
		_ = st.Transact(ctx, func(tx store.Store) error {
			if _, err := tx.Objects().Put(ctx, provider("gemini"), 0); err != nil {
				return err
			}
			panic("the apply blew up")
		})
	}()
	if _, _, err := st.Objects().ByName(ctx, v1.KindProvider, "gemini"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the object of a panicking Transact = %v", err)
	}
	// The ring is the process's and is not part of a transaction.
	at := time.Now()
	if err := st.Transact(ctx, func(tx store.Store) error {
		return tx.Usage().AppendRecord(ctx, metering.Record{ID: "req_1", At: at, Key: metering.KeyRef{ID: "key_1"}})
	}); err != nil {
		t.Fatal(err)
	}
	recs, _, err := st.Usage().Records(ctx, metering.RecordQuery{Query: metering.Query{From: at.Add(-time.Minute), To: at.Add(time.Minute)}}, store.Page{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("Records = %v, %v", recs, err)
	}
}

// proxy carries TCP between a listener of its own and the database, so
// a test can take the database away from a serving store and give it
// back.
type proxy struct {
	upstream string
	addr     string
	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func startProxy(t *testing.T, upstream string) *proxy {
	t.Helper()
	p := &proxy{upstream: upstream, conns: map[net.Conn]struct{}{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.stop)
	return p
}

func (p *proxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	p.wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.upstream)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns[c], p.conns[up] = struct{}{}, struct{}{}
			p.mu.Unlock()
			p.wg.Go(func() { _, _ = io.Copy(up, c); _ = up.Close() })
			p.wg.Go(func() { _, _ = io.Copy(c, up); _ = c.Close() })
		}
	})
}

// stop closes the listener and every carried connection.
func (p *proxy) stop() {
	p.mu.Lock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	p.wg.Wait()
}

// resume listens again at the same address.
func (p *proxy) resume(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		t.Fatal(err)
	}
	p.serve(ln)
}

// TestPostgresDatabaseGoesAwayWhileServing: a store whose database stops
// answering reports it through Ready inside the readiness budget and
// answers every call with the store's error rather than a contract
// error, and serves again once the database is back, without a restart.
func TestPostgresDatabaseGoesAwayWhileServing(t *testing.T) {
	ctx := t.Context()
	dbURL := pgtest.URL(t)
	u := mustURL(t, dbURL)
	px := startProxy(t, u.Host)
	u.Host = px.addr
	st := connect(t, u.String(), 2)
	p := provider("openai")
	if _, err := st.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	px.stop()
	started := time.Now()
	err := st.Ready(ctx)
	if err == nil {
		t.Fatal("Ready with the database gone")
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("Ready took %s, over the budget", took)
	}
	if !strings.Contains(err.Error(), "did not answer") || strings.Contains(err.Error(), u.User.String()) {
		t.Errorf("Ready = %v", err)
	}
	_, _, err = st.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	if err == nil {
		t.Fatal("Get with the database gone")
	}
	for _, named := range store.Errors() {
		if errors.Is(err, named) {
			t.Errorf("a failing store answered the contract error %v", named)
		}
	}
	if _, err := st.Counters().Add(ctx, "k", 1, time.Time{}); err == nil {
		t.Fatal("Add with the database gone")
	}

	px.resume(t)
	eventually(t, "the store serving again", 15*time.Second, func() bool { return st.Ready(ctx) == nil })
	got, _, err := st.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	if err != nil || got.Name() != "openai" {
		t.Fatalf("Get after the database came back = %v, %v", got, err)
	}
}
