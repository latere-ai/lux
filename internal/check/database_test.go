// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/store/postgres"
)

// fakeDB stands in for the Postgres store's readiness, schema, and
// connection limits, so the three rows are driven through every state
// without a database.
type fakeDB struct {
	ready         error
	schema        postgres.Schema
	schemaErr     error
	max, reserved int
	limitsErr     error
}

func (f *fakeDB) Ready(context.Context) error                     { return f.ready }
func (f *fakeDB) Schema(context.Context) (postgres.Schema, error) { return f.schema, f.schemaErr }
func (f *fakeDB) ConnectionLimits(context.Context) (int, int, error) {
	return f.max, f.reserved, f.limitsErr
}
func (f *fakeDB) Endpoint() string { return "db.example.com:5432/lux" }

// healthy is a cluster at this build's schema with room for the replicas.
func healthy() *fakeDB {
	return &fakeDB{schema: postgres.Schema{Version: postgres.Highest}, max: 100, reserved: 3}
}

// TestCheckReadsTheDatabase is spec 010's three rows of luxd check over
// the Postgres store, each state of the database once: the store row
// answers SELECT 1 or fails; the migrations row is ok at or behind this
// build's version naming both, warn ahead within the major, fail when
// dirty or of another major, and not checked when the store did not
// answer; the db conns row compares the replicas' connections with what
// the cluster leaves after its reserved slots, warning when the replicas
// together exceed it and failing when one alone does; and the rows over
// the objects are not checked while the schema is not there to read.
func TestCheckReadsTheDatabase(t *testing.T) {
	s := newStack(t)
	highest := strconv.FormatInt(postgres.Highest, 10)
	cases := []struct {
		name    string
		db      *fakeDB
		env     map[string]string
		states  map[string]State
		details map[string]string
	}{
		{
			name: "at this build's version", db: healthy(),
			states:  map[string]State{"store": OK, "migrations": OK, "db conns": OK, "providers": OK, "credentials": OK},
			details: map[string]string{"store": "the Postgres store at db.example.com:5432/lux answered SELECT 1 in", "migrations": highest, "db conns": "max_connections is 100 with 3 reserved for superusers, leaving 97"},
		},
		{
			name: "behind", db: &fakeDB{schema: postgres.Schema{Version: 1000001}, max: 100, reserved: 3},
			states:  map[string]State{"migrations": OK, "providers": OK},
			details: map[string]string{"migrations": "1000001"},
		},
		{
			name: "ahead within the major", db: &fakeDB{schema: postgres.Schema{Version: postgres.Highest + 1}, max: 100, reserved: 3},
			states:  map[string]State{"migrations": Warn, "providers": OK},
			details: map[string]string{"migrations": strconv.FormatInt(postgres.Highest+1, 10)},
		},
		{
			name: "dirty", db: &fakeDB{schema: postgres.Schema{Version: postgres.Highest, Dirty: true}, max: 100, reserved: 3},
			states:  map[string]State{"store": OK, "migrations": Fail, "providers": Warn, "credentials": Warn, "dialects": Warn, "public url": Warn},
			details: map[string]string{"migrations": "dirty", "providers": "would refuse the schema"},
		},
		{
			name: "another major", db: &fakeDB{schema: postgres.Schema{Version: 2000001}, max: 100, reserved: 3},
			states:  map[string]State{"migrations": Fail, "providers": Warn},
			details: map[string]string{"migrations": "major 2"},
		},
		{
			name: "not applied yet", db: &fakeDB{max: 100, reserved: 3},
			states:  map[string]State{"store": OK, "migrations": OK, "db conns": OK, "providers": Warn, "credentials": Warn},
			details: map[string]string{"migrations": "not applied yet", "providers": "not checked; the schema is not applied yet"},
		},
		{
			name: "the database does not answer", db: &fakeDB{ready: errors.New("postgres: the database at db.example.com:5432/lux did not answer: dial refused"), schemaErr: errors.New("dial refused")},
			states:  map[string]State{"store": Fail, "migrations": Warn, "db conns": Warn, "providers": Warn, "credentials": Warn},
			details: map[string]string{"store": "did not answer", "migrations": "not checked; the store did not answer", "db conns": "not checked; the store did not answer"},
		},
		{
			name: "the replicas together exceed the cluster", db: &fakeDB{schema: postgres.Schema{Version: postgres.Highest}, max: 20, reserved: 3},
			states:  map[string]State{"db conns": Warn},
			details: map[string]string{"db conns": "leaving 17; the replicas together exceed it"},
		},
		{
			name: "one replica alone exceeds the cluster", db: &fakeDB{schema: postgres.Schema{Version: postgres.Highest}, max: 20, reserved: 3},
			env:     map[string]string{"LUX_DB_MAX_CONNS": "20"},
			states:  map[string]State{"db conns": Fail},
			details: map[string]string{"db conns": "each replica opens up to 21 connections", "db conns ": "one replica alone exceeds it"},
		},
		{
			name: "the limits cannot be read", db: &fakeDB{schema: postgres.Schema{Version: postgres.Highest}, limitsErr: errors.New("reading the cluster's connection limits: permission denied")},
			states:  map[string]State{"db conns": Fail},
			details: map[string]string{"db conns": "permission denied"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := s.serverEnv()
			m["LUX_DB_URL"] = "postgres://lux:hunter2@db.example.com:5432/lux?sslmode=require"
			for k, v := range tc.env {
				m[k] = v
			}
			st := memory.New()
			o := Options{Getenv: env(m), open: func(context.Context, config.Config, config.Getenv) (opened, error) {
				return opened{st: st, db: tc.db}, nil
			}}
			lines := byName(Lines(t.Context(), o))
			for name, want := range tc.states {
				if lines[name].State != want {
					t.Errorf("%s: want %s, got %s", name, want, lines[name])
				}
			}
			for name, want := range tc.details {
				l := lines[strings.TrimSpace(name)]
				if !strings.Contains(l.Detail, want) {
					t.Errorf("%s does not say %q: %s", strings.TrimSpace(name), want, l)
				}
			}
			for _, l := range lines {
				if strings.Contains(l.Detail, "hunter2") {
					t.Errorf("%s carries the password", l)
				}
			}
		})
	}
}
