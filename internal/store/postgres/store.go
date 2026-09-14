// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
)

// The pool's shape. A managed cluster caps its connections in the low
// tens and a replica set multiplies whatever one process opens, so a
// pool sized for one process is an outage at three; the default is what
// one process can defensibly hold and an installation that needs more
// raises LUX_DB_MAX_CONNS against a cluster it has measured.
const (
	DefaultMaxConns = 8
	minConns        = 1
	maxConnIdleTime = 60 * time.Second
	maxConnLifetime = 30 * time.Minute
	connectTimeout  = 5 * time.Second
	// applicationName is what the pool's connections report in
	// pg_stat_activity, so an operator tells the gateway's from others'.
	applicationName = "luxd"
	// readyBudget bounds Ready's SELECT 1.
	readyBudget = time.Second
)

// Options configure Open.
type Options struct {
	// URL is LUX_DB_URL, a postgres:// or postgresql:// URL, its sslmode
	// included and honoured as written. It is never echoed.
	URL string
	// MaxConns is LUX_DB_MAX_CONNS; zero is DefaultMaxConns.
	MaxConns int
	// Now is the clock the store stamps rows with and measures every
	// expiry against; nil is time.Now.
	Now func() time.Time
}

// Store is the Postgres store: one pgxpool.Pool for the process, every
// collection over it, and the process's own ring of records, which are
// not rows in any mode.
type Store struct {
	pool *pgxpool.Pool
	// tx is set on the Store a Transact hands out; every method then
	// runs on the transaction and a Transact on it is ErrNested.
	tx pgx.Tx
	// ring holds the records of Usage().AppendRecord, spec 009's bounded
	// ring per Key, which the memory store already implements.
	ring       *memory.Store
	now        func() time.Time
	closed     *atomic.Bool
	closeOnce  *sync.Once
	endpoint   string
	migrateURL string
	maxConns   int
}

// Open parses the URL, builds the pool, and returns the store without
// touching the database: the first query opens the first connection.
// The caller decides about the schema; Connect is the serve role's
// sequence and check's is Schema alone.
func Open(_ context.Context, o Options) (*Store, error) {
	if o.MaxConns <= 0 {
		o.MaxConns = DefaultMaxConns
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	cfg, err := poolConfig(o)
	if err != nil {
		return nil, err
	}
	mURL, err := migrateURL(o.URL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("LUX_DB_URL: opening the pool: %w", err)
	}
	return &Store{
		pool: pool, ring: memory.New(memory.WithClock(o.Now)), now: o.Now,
		closed: new(atomic.Bool), closeOnce: new(sync.Once),
		endpoint: endpointOf(o.URL), migrateURL: mURL, maxConns: o.MaxConns,
	}, nil
}

// poolConfig is the pool's configuration from the options: the URL as
// the driver reads it, with the four bounds of spec 010 and a connect
// timeout when the URL sets none. The parser's error is not echoed,
// because it may carry the password.
func poolConfig(o Options) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(o.URL)
	if err != nil {
		return nil, errors.New("LUX_DB_URL does not parse as a connection URL; the value is not echoed because it may carry a password")
	}
	cfg.MaxConns = int32(o.MaxConns) //nolint:gosec // bounded by config to at most 100
	cfg.MinConns = minConns
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.MaxConnLifetime = maxConnLifetime
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = connectTimeout
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = applicationName
	}
	return cfg, nil
}

// endpointOf is the part of the URL a line may print: the host, the
// port, and the database, never the userinfo or the query.
func endpointOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the database"
	}
	return u.Host + u.Path
}

// Connect is the serve role's start: Open, read the schema, hold it to
// the guard, apply what is missing, and answer the store ready to serve.
// A database that does not answer is the error, naming the endpoint and
// never the URL. warning is the one WARN line the start prints when the
// schema is ahead within the major, and "" otherwise.
func Connect(ctx context.Context, o Options) (s *Store, warning string, err error) {
	opened, err := Open(ctx, o)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if err != nil {
			_ = opened.Close()
		}
	}()
	s = opened
	sch, err := s.Schema(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("LUX_DB_URL: the database at %s did not answer: %w", s.endpoint, err)
	}
	action, why := Guard(sch)
	switch action {
	case Refuse:
		return nil, "", fmt.Errorf("LUX_DB_URL: %s", why)
	case Serve:
		warning = why
	default:
		if err := s.Migrate(ctx); err != nil {
			return nil, "", fmt.Errorf("LUX_DB_URL: %w", err)
		}
	}
	return s, warning, nil
}

// Notice is the start-up line's substance: where the state is and what
// that gives, with the pool's ceiling and the schema's version.
func (s *Store) Notice(ctx context.Context) string {
	sch, err := s.Schema(ctx)
	version := "unknown"
	if err == nil {
		version = strconv.FormatInt(sch.Version, 10)
	}
	return fmt.Sprintf("state in Postgres at %s: desired state, spend windows, leases, and the journal are shared by every replica and survive a restart; rate windows are per replica; at most %d connections from this process; schema at version %s", s.endpoint, s.maxConns, version)
}

// Endpoint is the host, the port, and the database the store reaches,
// the part of LUX_DB_URL a line may print.
func (s *Store) Endpoint() string { return s.endpoint }

// MaxConns is the pool's ceiling.
func (s *Store) MaxConns() int { return s.maxConns }

// ConnectionLimits reads the cluster's max_connections and
// superuser_reserved_connections, which luxd check's db conns row
// compares with what the replicas open.
func (s *Store) ConnectionLimits(ctx context.Context) (maxConnections, reserved int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	err = s.q().QueryRow(ctx, `SELECT current_setting('max_connections')::int, current_setting('superuser_reserved_connections')::int`).Scan(&maxConnections, &reserved)
	if err != nil {
		return 0, 0, fmt.Errorf("reading the cluster's connection limits: %w", err)
	}
	return maxConnections, reserved, nil
}

// querier is what a statement runs on: the pool outside a Transact, the
// transaction inside one.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) q() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.pool
}

// read runs fn on the pool, or on the transaction inside a Transact. A
// context that has ended is answered with its error before anything is
// read.
func (s *Store) read(ctx context.Context, fn func(q querier) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(s.q())
}

// write runs fn in a transaction of its own outside a Transact, and in a
// savepoint inside one, so a write the contract refuses, a taken name or
// a stale version, leaves the enclosing transaction usable, as it does
// on the memory store.
func (s *Store) write(ctx context.Context, fn func(q pgx.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	var tx pgx.Tx
	if s.tx != nil {
		tx, err = s.tx.Begin(ctx)
	} else {
		tx, err = s.pool.Begin(ctx)
	}
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(ctx)
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

// Objects implements store.Store.
func (s *Store) Objects() store.Objects { return objects{s} }

// Keys implements store.Store.
func (s *Store) Keys() store.Keys { return keys{s} }

// Credentials implements store.Store.
func (s *Store) Credentials() store.Credentials { return credentials{s} }

// Counters implements store.Store.
func (s *Store) Counters() store.Counters { return counters{s} }

// Leases implements store.Store.
func (s *Store) Leases() store.Leases { return leases{s} }

// Journal implements store.Store.
func (s *Store) Journal() store.Journal { return journal{s} }

// Tunnels implements store.Store.
func (s *Store) Tunnels() store.Tunnels { return tunnels{s} }

// Usage implements store.Store.
func (s *Store) Usage() store.Usage { return usage{s} }

// Transact implements store.Store: one transaction for fn, committed
// when fn returns nil and rolled back otherwise, a panic included. The
// Store fn is handed runs every method on that transaction; a Transact
// on it is ErrNested.
func (s *Store) Transact(ctx context.Context, fn func(tx store.Store) error) (err error) {
	if s.tx != nil {
		return store.ErrNested
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(ctx)
			panic(r)
		}
	}()
	inner := *s
	inner.tx = tx
	if err := fn(&inner); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	return nil
}

// Ready implements store.Store: SELECT 1 inside a one second budget,
// which is the readiness check named store.
func (s *Store) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errors.New("postgres: the store is closed")
	}
	ctx, cancel := context.WithTimeout(ctx, readyBudget)
	defer cancel()
	var one int
	if err := s.q().QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("postgres: the database at %s did not answer: %w", s.endpoint, err)
	}
	return nil
}

// Close implements store.Store: the pool is closed once, and Ready says
// so after.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.pool.Close()
	})
	return nil
}

// The error codes the store reads.
const (
	uniqueViolation = "23505"
	undefinedTable  = "42P01"
)

// errNoRows is pgx's no-row answer, named once.
var errNoRows = pgx.ErrNoRows

// violates reports whether err is a unique violation on the named
// constraint or index.
func violates(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == constraint
}
