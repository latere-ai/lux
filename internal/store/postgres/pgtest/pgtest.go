// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package pgtest gives a test a database of its own on the cluster
// LUX_DB_URL names: created empty when the test starts and dropped when
// it ends, so two packages the toolchain runs at once never see each
// other's rows and a case that asserts an empty store starts from one.
// The database is the postgres tier's dependency, declared by the
// variable and never started here; a test that finds the variable unset
// skips naming it and one way to satisfy it, so a run without a database
// reports what it did not check rather than passing over nothing.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Variable is the one variable the tier reads.
const Variable = "LUX_DB_URL"

// Unset is the skip reason: the variable and one way to satisfy it.
const Unset = Variable + " is unset; the Postgres store's tests need a database: podman run -d --rm -p 5432:5432 -e POSTGRES_PASSWORD=lux --name lux-pg docker.io/library/postgres:17-alpine, then " + Variable + "=postgres://postgres:lux@127.0.0.1:5432/postgres?sslmode=disable"

// Admin is LUX_DB_URL itself, or the skip.
func Admin(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(Variable)
	if admin == "" {
		t.Skip(Unset)
	}
	return admin
}

// URL is the connection URL of a fresh database on the cluster, dropped
// when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	admin := Admin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("pgtest: connecting to the cluster %s names: %v", Variable, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "lux_test_" + hex.EncodeToString(random[:])
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("pgtest: creating database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("pgtest: reconnecting to drop %s: %v", name, err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Logf("pgtest: dropping database %s: %v", name, err)
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("pgtest: %s does not parse: %v", Variable, err)
	}
	u.Path = "/" + name
	return u.String()
}

// Exec runs one statement on the database at dbURL, for a test that
// arranges a state the store would never write, a dirty schema or one
// of another major.
func Exec(t testing.TB, dbURL, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgtest: connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("pgtest: %s: %v", sql, err)
	}
}

// Count answers one integer query on the database at dbURL.
func Count(t testing.TB, dbURL, sql string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("pgtest: connecting: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("pgtest: %s: %v", sql, err)
	}
	return n
}

// Redacted is dbURL with its password replaced, for a log line.
func Redacted(dbURL string) string {
	u, err := url.Parse(dbURL)
	if err != nil {
		return "the database"
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "redacted")
	}
	return fmt.Sprint(u)
}
