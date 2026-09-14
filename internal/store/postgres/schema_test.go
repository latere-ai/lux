// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestSchemaGuards is the decision table of spec 010 over the stored
// version, without a database: a dirty schema and one of another major
// refuse naming the version, one ahead within the major serves with the
// warning naming both, and one at or behind is migrated.
func TestSchemaGuards(t *testing.T) {
	rows := []struct {
		name   string
		schema Schema
		action Action
		names  []string
	}{
		{"dirty", Schema{Version: 1000001, Dirty: true}, Refuse, []string{"dirty", "1000001"}},
		{"another major", Schema{Version: 2000001}, Refuse, []string{"2000001", "major 2", "major 1"}},
		{"ahead within the major", Schema{Version: Highest + 1}, Serve, []string{strconv.FormatInt(Highest+1, 10), strconv.FormatInt(Highest, 10), "ahead"}},
		{"at the highest", Schema{Version: Highest}, Migrate, []string{strconv.FormatInt(Highest, 10)}},
		{"behind", Schema{Version: 1000001}, Migrate, []string{"1000001", strconv.FormatInt(Highest, 10)}},
		{"none", Schema{}, Migrate, []string{"not applied", strconv.FormatInt(Highest, 10)}},
		{"dirty of another major", Schema{Version: 3000001, Dirty: true}, Refuse, []string{"dirty", "3000001"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			action, why := Guard(r.schema)
			if action != r.action {
				t.Fatalf("Guard(%+v) = %s, want %s: %s", r.schema, action, r.action, why)
			}
			for _, n := range r.names {
				if !strings.Contains(why, n) {
					t.Errorf("the sentence does not name %q: %s", n, why)
				}
			}
		})
	}
	for a, want := range map[Action]string{Migrate: "migrate", Serve: "serve", Refuse: "refuse", Action(9): "refuse"} {
		if a.String() != want {
			t.Errorf("Action(%d).String() = %q, want %q", a, a.String(), want)
		}
	}
}

// TestMigrationVersions: the embedded names parse to versions of this
// build's major, in one increasing sequence that ends at Highest, and
// a name of another shape or major is refused.
func TestMigrationVersions(t *testing.T) {
	entries, err := fs.ReadDir(migrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, e := range entries {
		v, err := Version(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if !slices.IsSorted(versions) || len(slices.Compact(slices.Clone(versions))) != len(versions) {
		t.Fatalf("the versions are not strictly increasing: %v", versions)
	}
	if versions[0] != Major*perMajor+1 {
		t.Errorf("the first migration of major %d is %d, want %d", Major, versions[0], Major*perMajor+1)
	}
	if versions[len(versions)-1] != Highest {
		t.Errorf("Highest = %d, the last migration is %d", Highest, versions[len(versions)-1])
	}
	if MajorOf(Highest) != Major {
		t.Errorf("MajorOf(%d) = %d, want %d", Highest, MajorOf(Highest), Major)
	}
	for _, bad := range []string{"init.up.sql", "1000001_init.down.sql", "abc_init.up.sql", "2000001_other.up.sql", "1000001_.up.sql", "0_zero.up.sql", "-5_neg.up.sql", "1000001"} {
		if _, err := Version(bad); err == nil {
			t.Errorf("Version(%q) accepted", bad)
		}
	}
	if v, err := Version("migrations/1000007_seven.up.sql"); err != nil || v != 1000007 {
		t.Errorf("Version with a directory = %d, %v", v, err)
	}
}

// statement is one SQL statement of a migration, comments stripped.
var (
	comment    = regexp.MustCompile(`(?m)--.*$`)
	createTbl  = regexp.MustCompile(`(?is)^CREATE TABLE IF NOT EXISTS \w+ \(.*\)$`)
	createIdx  = regexp.MustCompile(`(?is)^CREATE (UNIQUE )?INDEX IF NOT EXISTS \w+ ON \w+ .*$`)
	addColumn  = regexp.MustCompile(`(?is)^ALTER TABLE \w+ ADD COLUMN IF NOT EXISTS \w+ \w+.*$`)
	forbidden  = regexp.MustCompile(`(?i)\b(DROP|RENAME|ALTER COLUMN|SET DATA TYPE|TRUNCATE|DELETE|UPDATE)\b`)
	notNullish = regexp.MustCompile(`(?i)\bNOT NULL\b`)
	defaulted  = regexp.MustCompile(`(?i)\bDEFAULT\b`)
)

// TestMigrationsAreAdditive holds every .up.sql of the current major to
// the grammar of spec 010: a statement creates a table, adds a nullable
// or defaulted column, or adds an index, and never drops, renames, or
// retypes anything; and no .down.sql exists, because the way back is the
// previous binary.
func TestMigrationsAreAdditive(t *testing.T) {
	entries, err := fs.ReadDir(migrations, migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migration is embedded")
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".down.") {
			t.Errorf("%s: a down migration exists; the way back is the previous binary", e.Name())
		}
		data, err := fs.ReadFile(migrations, migrationsDir+"/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "-- SPDX-FileCopyrightText:") {
			t.Errorf("%s carries no licence header", e.Name())
		}
		body := comment.ReplaceAllString(string(data), "")
		statements := 0
		for stmt := range strings.SplitSeq(body, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			statements++
			if forbidden.MatchString(stmt) {
				t.Errorf("%s: a statement drops, renames, retypes, or edits rows, which the additive rule forbids inside a major:\n%s", e.Name(), stmt)
				continue
			}
			switch {
			case createTbl.MatchString(stmt), createIdx.MatchString(stmt):
			case addColumn.MatchString(stmt):
				if notNullish.MatchString(stmt) && !defaulted.MatchString(stmt) {
					t.Errorf("%s: an added column is NOT NULL without a DEFAULT, which the previous binary's rows cannot satisfy:\n%s", e.Name(), stmt)
				}
			default:
				t.Errorf("%s: a statement is none of create table, add column, or create index:\n%s", e.Name(), stmt)
			}
		}
		if statements == 0 {
			t.Errorf("%s holds no statement", e.Name())
		}
	}
}

// TestPoolDefaults: LUX_DB_MAX_CONNS bounds the pool and its default is
// 8, with the other three bounds of spec 010, the connect timeout when
// the URL sets none, and the application name; a URL that sets its own
// connect timeout keeps it. Nothing is dialled.
func TestPoolDefaults(t *testing.T) {
	st, err := Open(t.Context(), Options{URL: "postgres://lux:secret@127.0.0.1:1/lux?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := st.pool.Config()
	if cfg.MaxConns != DefaultMaxConns || st.MaxConns() != DefaultMaxConns {
		t.Errorf("MaxConns = %d, want the default %d", cfg.MaxConns, DefaultMaxConns)
	}
	if cfg.MinConns != minConns || cfg.MaxConnIdleTime != maxConnIdleTime || cfg.MaxConnLifetime != maxConnLifetime {
		t.Errorf("pool bounds = min %d, idle %s, lifetime %s", cfg.MinConns, cfg.MaxConnIdleTime, cfg.MaxConnLifetime)
	}
	if cfg.ConnConfig.ConnectTimeout != connectTimeout {
		t.Errorf("ConnectTimeout = %s, want %s", cfg.ConnConfig.ConnectTimeout, connectTimeout)
	}
	if cfg.ConnConfig.RuntimeParams["application_name"] != applicationName {
		t.Errorf("application_name = %q", cfg.ConnConfig.RuntimeParams["application_name"])
	}
	if st.Endpoint() != "127.0.0.1:1/lux" {
		t.Errorf("Endpoint = %q", st.Endpoint())
	}
	if !strings.Contains(st.Notice(t.Context()), "127.0.0.1:1/lux") || !strings.Contains(st.Notice(t.Context()), "at most 8 connections") || strings.Contains(st.Notice(t.Context()), "secret") {
		t.Errorf("Notice = %q", st.Notice(t.Context()))
	}

	sized, err := Open(t.Context(), Options{URL: "postgresql://lux@127.0.0.1:1/lux?connect_timeout=2&application_name=other", MaxConns: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sized.Close() }()
	if c := sized.pool.Config(); c.MaxConns != 3 || c.ConnConfig.ConnectTimeout != 2*time.Second || c.ConnConfig.RuntimeParams["application_name"] != "other" {
		t.Errorf("a sized pool = max %d, timeout %s, name %q", c.MaxConns, c.ConnConfig.ConnectTimeout, c.ConnConfig.RuntimeParams["application_name"])
	}
	if endpointOf("::not a url") != "the database" {
		t.Errorf("endpointOf a non-URL = %q", endpointOf("::not a url"))
	}
	// A URL the driver refuses is refused without being echoed.
	const password = "s3cret-value"
	if _, err := Open(t.Context(), Options{URL: "postgres://lux:" + password + "@127.0.0.1:1/lux?sslmode=nonsense"}); err == nil || strings.Contains(err.Error(), password) {
		t.Errorf("a bad sslmode: %v", err)
	}
	if _, err := Open(t.Context(), Options{URL: "postgres://lux:" + password + "@127.0.0.1:port/lux"}); err == nil || strings.Contains(err.Error(), password) {
		t.Errorf("a URL that does not parse: %v", err)
	}
	if u, err := migrateURL("postgresql://lux:" + password + "@db.example.com:5432/lux?sslmode=require"); err != nil || u != "pgx5://lux:"+password+"@db.example.com:5432/lux?sslmode=require" {
		t.Errorf("migrateURL = %q, %v", u, err)
	}
	if _, err := migrateURL("postgres://lux:" + password + "@127.0.0.1:port/lux"); err == nil || strings.Contains(err.Error(), password) {
		t.Errorf("migrateURL of a non-URL: %v", err)
	}
}

// TestDatabaseDownAtStartup: a database that does not answer at start
// is one line naming LUX_DB_URL and the endpoint, never the password,
// and the pool is closed behind it.
func TestDatabaseDownAtStartup(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	const password = "s3cret-value"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	st, warning, err := Connect(ctx, Options{URL: "postgres://lux:" + password + "@" + addr + "/lux?sslmode=disable"})
	if err == nil {
		_ = st.Close()
		t.Fatal("Connect to a closed port succeeded")
	}
	if warning != "" {
		t.Errorf("a warning beside the error: %q", warning)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "LUX_DB_URL: the database at "+addr+"/lux did not answer: ") || strings.Contains(msg, password) {
		t.Errorf("error = %q", msg)
	}
	// The other roles read the schema and the readiness on their own,
	// and get the same answer without a URL in it.
	open, err := Open(ctx, Options{URL: "postgres://lux:" + password + "@" + addr + "/lux?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()
	if err := open.Ready(ctx); err == nil || strings.Contains(err.Error(), password) || !strings.Contains(err.Error(), addr) {
		t.Errorf("Ready against a closed port: %v", err)
	}
	if _, err := open.Schema(ctx); err == nil || strings.Contains(err.Error(), password) {
		t.Errorf("Schema against a closed port: %v", err)
	}
	if _, _, err := open.ConnectionLimits(ctx); err == nil || strings.Contains(err.Error(), password) {
		t.Errorf("ConnectionLimits against a closed port: %v", err)
	}
	// An ended context is answered before anything is dialled.
	ended, cancelEnded := context.WithCancel(ctx)
	cancelEnded()
	if _, err := open.Schema(ended); !errors.Is(err, context.Canceled) {
		t.Errorf("Schema on an ended context: %v", err)
	}
	if err := open.Migrate(ended); !errors.Is(err, context.Canceled) {
		t.Errorf("Migrate on an ended context: %v", err)
	}
	if _, _, err := open.ConnectionLimits(ended); !errors.Is(err, context.Canceled) {
		t.Errorf("ConnectionLimits on an ended context: %v", err)
	}
	if _, _, err := Connect(ended, Options{URL: "postgres://lux@" + addr + "/lux"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect on an ended context: %v", err)
	}
	if !strings.Contains(open.Notice(ctx), "schema at version unknown") {
		t.Errorf("Notice without a database = %q", open.Notice(ctx))
	}
	// Close twice is one close.
	if err := open.Close(); err != nil {
		t.Fatal(err)
	}
	if err := open.Ready(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Ready after Close: %v", err)
	}
}

// TestObjectEncoding holds the JSON halves to their rules without a
// database: the observed members and a Key's value leave the control
// half, a Model without a source is declared, the labels and the target
// Providers are columns, a decode rebuilds the object, and the observed
// document carries the non-zero members alone under their status names.
func TestObjectEncoding(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	k := &v1.Key{Metadata: v1.ObjectMeta{Name: "run", Labels: map[string]string{"team": "red"}}, Spec: v1.KeySpec{Models: []string{"gpt"}}}
	k.Spec.SetValue("lux_supplied")
	k.Status = v1.KeyStatus{ID: "key_1", Owner: "alice", Value: "lux_minted", State: v1.KeyExhausted, Prefix: "lux_ab", Usage: &v1.KeyUsage{}, LastUsedAt: at, Warnings: []string{}}
	e, err := encode(k)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"value", "state", "usage", "lastUsedAt"} {
		if _, has := e.status[gone]; has {
			t.Errorf("the control half carries %s", gone)
		}
	}
	if string(e.status["prefix"]) != `"lux_ab"` || e.labels["team"] != "red" || e.kind != v1.KindKey || e.owner != "alice" {
		t.Errorf("encoded = %+v", e)
	}
	status, err := e.stamp("key_1", 3, at, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := decode(v1.KindKey, e.spec, status)
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*v1.Key)
	if _, set := got.Spec.Value(); set || got.Status.Value != "" || got.Status.Version != 3 || !got.Status.CreatedAt.Equal(at) || !got.Status.UpdatedAt.Equal(at.Add(time.Minute)) || got.Metadata.Labels["team"] != "red" || got.Status.State != "" {
		t.Errorf("decoded = %+v", got)
	}

	m := &v1.Model{Metadata: v1.ObjectMeta{Name: "gpt"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: "openai"}, {Provider: "prv_1"}}}, Status: v1.ModelStatus{ID: "mdl_1", Available: new(true)}}
	em, err := encode(m)
	if err != nil {
		t.Fatal(err)
	}
	if em.source != string(v1.SourceDeclared) || string(em.status["source"]) != `"declared"` || !slices.Equal(em.providers, []string{"openai", "prv_1"}) {
		t.Errorf("a Model without a source = %+v", em)
	}
	if _, has := em.status["available"]; has {
		t.Error("the control half carries available")
	}
	setRow(m, "mdl_1", 1, at, at)
	if m.Status.Source != v1.SourceDeclared || m.Status.Version != 1 {
		t.Errorf("setRow = %+v", m.Status)
	}
	setRow(foreign{}, "x", 1, at, at) // a foreign type is left alone

	if _, err := encode(foreign{}); err == nil {
		t.Error("a foreign object encoded")
	}
	if _, err := newObject("Thing"); err == nil {
		t.Error("an unknown kind has an empty value")
	}
	if _, err := decode(v1.KindBudget, []byte(`{"metadata":`), nil); err == nil {
		t.Error("a broken spec decoded")
	}
	if _, err := decode(v1.KindBudget, nil, []byte(`{"version":"x"}`)); err == nil {
		t.Error("a broken status decoded")
	}
	if _, err := decode("Thing", nil, nil); err == nil {
		t.Error("an unknown kind decoded")
	}
	if got := parseTime("garbage", at); !got.Equal(at) {
		t.Errorf("parseTime of garbage = %s", got)
	}

	// The observed document, by kind: the non-zero members alone, under
	// their status names.
	for _, tc := range []struct {
		kind     string
		observed any
		want     map[string]string // member to its JSON, or "" for a document
	}{
		{v1.KindProvider, store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy}}, map[string]string{"health": ""}},
		{v1.KindProvider, &store.ProviderObserved{}, map[string]string{}},
		{v1.KindProvider, (*store.ProviderObserved)(nil), map[string]string{}},
		{v1.KindModel, store.ModelObserved{Targets: []v1.TargetStatus{}}, map[string]string{"targets": "[]"}},
		{v1.KindModel, store.ModelObserved{Available: new(false)}, map[string]string{"available": "false"}},
		{v1.KindKey, store.KeyObserved{State: v1.KeyActive, LastUsedAt: at}, map[string]string{"lastUsedAt": `"2026-09-14T10:00:00Z"`, "state": `"Active"`}},
		{v1.KindBudget, store.BudgetObserved{State: v1.BudgetOpen, Keys: new(2), ResetsAt: at}, map[string]string{"keys": "2", "resetsAt": `"2026-09-14T10:00:00Z"`, "state": `"Open"`}},
	} {
		doc, err := observedJSON(tc.kind, tc.observed)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(doc, &got); err != nil {
			t.Fatalf("%s: %s: %v", tc.kind, doc, err)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s observed = %s, want the members %v", tc.kind, doc, tc.want)
		}
		for member, want := range tc.want {
			raw, has := got[member]
			if !has || (want != "" && string(raw) != want) {
				t.Errorf("%s observed %s = %s, want %s", tc.kind, member, raw, want)
			}
		}
	}
	for _, tc := range []struct {
		kind     string
		observed any
	}{
		{v1.KindProvider, store.ModelObserved{}}, {v1.KindModel, 7}, {v1.KindKey, struct{}{}}, {v1.KindBudget, &store.KeyObserved{}}, {"Thing", nil},
	} {
		if _, err := observedJSON(tc.kind, tc.observed); err == nil {
			t.Errorf("%s accepted %T", tc.kind, tc.observed)
		}
	}
}

// foreign is an Object of a type the store does not know.
type foreign struct{}

func (foreign) Kind() string  { return v1.KindProvider }
func (foreign) ID() string    { return "prv_x" }
func (foreign) Owner() string { return "" }
func (foreign) Name() string  { return "x" }
