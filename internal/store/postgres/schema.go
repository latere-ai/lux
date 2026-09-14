// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	// The migrator's database driver: pgxmigrate imports none and asks
	// the caller to blank-import the one whose scheme its URL carries,
	// which is pgx5:// for this one.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"latere.ai/x/pkg/pgxmigrate"
)

// Migrations are embedded as <version>_<name>.up.sql, with no .down.sql:
// the way back is the previous binary. <version> is the schema major
// followed by a six-digit ordinal, so the first migration of v1 is
// 1000001_init.up.sql, and the major is read back from a version by
// integer division.
//
//go:embed migrations/*.up.sql
var migrations embed.FS

// migrationsDir is the directory inside the embedded files.
const migrationsDir = "migrations"

// Major is the schema major this build reads and writes. A schema of
// another major is one this binary cannot read, and docs/upgrades is the
// way across.
const Major int64 = 1

// perMajor is the width of one major's ordinals: version / perMajor is
// the major.
const perMajor int64 = 1_000_000

// Highest is the highest version among the embedded migrations, read
// from their names at init.
var Highest = highest()

// highest reads the embedded directory and returns the highest version;
// a name that does not parse is a build defect and panics.
func highest() int64 {
	entries, err := fs.ReadDir(migrations, migrationsDir)
	if err != nil {
		panic("postgres: the embedded migrations do not list: " + err.Error())
	}
	var top int64
	for _, e := range entries {
		v, err := Version(e.Name())
		if err != nil {
			panic("postgres: " + err.Error())
		}
		top = max(top, v)
	}
	if top == 0 {
		panic("postgres: no embedded migration")
	}
	return top
}

// Version reads the version of a migration file name,
// <version>_<name>.up.sql, and refuses a name of another shape or a
// version of another major than this build's.
func Version(name string) (int64, error) {
	base := path.Base(name)
	digits, rest, ok := strings.Cut(base, "_")
	if !ok || !strings.HasSuffix(rest, ".up.sql") || rest == ".up.sql" {
		return 0, fmt.Errorf("migration %q is not <version>_<name>.up.sql", base)
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("migration %q has no numeric version", base)
	}
	if MajorOf(v) != Major {
		return 0, fmt.Errorf("migration %q is of major %d, and this build embeds major %d", base, MajorOf(v), Major)
	}
	return v, nil
}

// MajorOf is the schema major a stored version belongs to.
func MajorOf(version int64) int64 { return version / perMajor }

// Schema is what golang-migrate's schema_migrations table says: the
// stored version, 0 when the table does not exist yet, and whether a
// migration was left half applied.
type Schema struct {
	Version int64
	Dirty   bool
}

// Action is what a start does about the stored schema.
type Action int

// The four rows of the schema guard.
const (
	// Migrate applies what is missing and serves: the stored version is
	// at or below the binary's highest.
	Migrate Action = iota
	// Serve serves without applying anything: the stored version is
	// ahead within the binary's major, which the additive rule makes
	// readable, and the start logs one WARN naming both versions. This
	// is what makes a rollout undo inside a major a rollback and not an
	// outage.
	Serve
	// Refuse refuses to start: the schema is dirty, for an operator to
	// repair, or of another major, which this binary cannot read.
	Refuse
)

// String names the action.
func (a Action) String() string {
	switch a {
	case Migrate:
		return "migrate"
	case Serve:
		return "serve"
	default:
		return "refuse"
	}
}

// Guard holds a stored schema to the three rules of spec 010 and returns
// what to do with one sentence saying why, naming the stored and the
// embedded highest version.
func Guard(s Schema) (Action, string) {
	switch {
	case s.Dirty:
		return Refuse, fmt.Sprintf("the schema is dirty at version %d: a migration was left half applied and is an operator's to repair; nothing is retried over a dirty schema because a retry can only report the dirt", s.Version)
	case s.Version != 0 && MajorOf(s.Version) != Major:
		return Refuse, fmt.Sprintf("the schema is at version %d, of major %d, and this build reads major %d (highest %d); a schema of another major is one this binary cannot read, and docs/upgrades says the way across", s.Version, MajorOf(s.Version), Major, Highest)
	case s.Version > Highest:
		return Serve, fmt.Sprintf("the schema is at version %d, ahead of this build's highest %d within major %d; the additive rule makes it readable, so this build serves it without applying anything", s.Version, Highest, Major)
	case s.Version == Highest:
		return Migrate, fmt.Sprintf("the schema is at version %d, this build's highest", s.Version)
	case s.Version == 0:
		return Migrate, fmt.Sprintf("the schema is not applied yet; this build applies every migration through %d", Highest)
	default:
		return Migrate, fmt.Sprintf("the schema is at version %d and this build's highest is %d; the missing migrations are applied at start", s.Version, Highest)
	}
}

// Schema reads golang-migrate's table. A database without the table has
// no schema and reads as version 0.
func (s *Store) Schema(ctx context.Context) (Schema, error) {
	if err := ctx.Err(); err != nil {
		return Schema{}, err
	}
	var out Schema
	err := s.q().QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&out.Version, &out.Dirty)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return out, nil
	case errors.As(err, &pgErr) && pgErr.Code == undefinedTable:
		return Schema{}, nil
	case errors.Is(err, errNoRows):
		// The table exists and is empty, which golang-migrate leaves
		// behind when nothing was ever applied.
		return Schema{}, nil
	default:
		return Schema{}, fmt.Errorf("reading the schema version: %w", err)
	}
}

// Migrate applies the embedded migrations that are missing, through
// pgxmigrate over the URL with its scheme rewritten to pgx5://. The
// migrator opens a database/sql pool of its own for the length of the
// call and closes it, so a start briefly holds LUX_DB_MAX_CONNS plus one
// connection; it retries the open for ten seconds, so a rolling deploy
// whose outgoing replica still holds its pool does not fail the incoming
// one.
func (s *Store) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pgxmigrate.Up(s.migrateURL, migrations, migrationsDir); err != nil {
		return fmt.Errorf("applying the migrations: %w", err)
	}
	return nil
}

// migrateURL rewrites a postgres:// or postgresql:// URL to the pgx5://
// scheme the migrator's driver registers, keeping everything else.
func migrateURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("LUX_DB_URL does not parse as a URL")
	}
	u.Scheme = "pgx5"
	return u.String(), nil
}
