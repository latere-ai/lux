// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// Store is what the serve role constructs and every consumer takes as an
// interface. The implementations differ in durability and in how many
// replicas may share them, never in behavior. The store mints no id:
// every id is the caller's, the prefixed ULID of spec 001, and is the
// primary key of its row.
//
// Usage is the aggregate side of spec 009, whose parameters are the
// metering package's types, so a name is defined by one spec alone.
type Store interface {
	Objects() Objects
	Keys() Keys
	KeyFences() KeyFences
	Credentials() Credentials
	Counters() Counters
	Leases() Leases
	Journal() Journal
	Usage() Usage
	// Tunnels is the tunnel registry of spec 013.
	Tunnels() Tunnels
	// Transact runs fn against a Store whose writes commit together or
	// not at all. An apply is one Transact: the object, its journal row,
	// and a Key's hash or a Provider's credential; a Provider delete is
	// the Provider, its discovered Models, its credential, and the
	// journal rows. The memory store holds its write lock for the call,
	// so fn writes through tx and never through the outer Store;
	// Postgres runs one transaction. A Transact inside fn is ErrNested.
	Transact(ctx context.Context, fn func(tx Store) error) error
	// Ready is the readiness check named store: nil while the store
	// answers, the failure otherwise.
	Ready(ctx context.Context) error
	Close() error
}

// Objects holds desired state: the resolved manifest of every kind.
// Version is optimistic concurrency: a caller reads an object with its
// version, resolves, and writes with that version; a row that moved in
// between is ErrVersionConflict, which the API returns as 409. The
// version starts at 1, advances by one on every Put, and is rendered as
// status.version and as the ETag.
type Objects interface {
	// Put writes metadata, spec, and the control plane's members of
	// status, and never the observed members; ifVersion 0 creates. The
	// object carries its id, owner, warnings, and the kind's own control
	// members; Put writes back into it the row's id, version, createdAt,
	// and updatedAt, so the caller renders what was stored without a
	// second read. createdAt is the caller's on a create when set and
	// the clock otherwise, and the row's on an update; updatedAt is the
	// caller's when set and the clock otherwise. A Model whose source is
	// empty is stored as declared.
	//
	// A create over a live row of the same kind and name is
	// ErrNameTaken, except a declared Model over a discovered one, which
	// is replaced in place: the id kept, source declared, owner the
	// actor's, version advanced, so a declared Model shadows a
	// discovered one without a delete. A create over an id already used,
	// live or deleted, is ErrVersionConflict. An update of an id with no
	// live row is ErrNotFound; of a live row at another version,
	// ErrVersionConflict.
	Put(ctx context.Context, obj v1.Object, ifVersion int64) (version int64, err error)
	// Get and ByName return the object with both halves of status merged
	// and its version; a deleted row is ErrNotFound.
	Get(ctx context.Context, kind, id string) (v1.Object, int64, error)
	ByName(ctx context.Context, kind, name string) (v1.Object, int64, error)
	// List returns live objects of one kind ordered by name ascending;
	// next is the cursor of the last row returned while rows remain,
	// and "" at the end. A Limit of 0 or less is every row.
	List(ctx context.Context, kind string, f Filter, p Page) (objs []v1.Object, next string, err error)
	// Delete marks the row deleted, so its name is free at once and its
	// id never is; the row is removed by Prune once its events are
	// acknowledged, so Journal.ByObject can still name it while they
	// deliver. A row that is not live is ErrNotFound.
	Delete(ctx context.Context, kind, id string) error
	// PutStatus writes the observed members of status and nothing else.
	// observed is the kind's struct below, ProviderObserved for a
	// Provider and so on; a zero member leaves the stored member as it
	// was, so the health and discovery jobs each write their own members
	// without reading the other's. It takes no version and never
	// conflicts: every observed member has one writer, the lease holder
	// of its job, and Put never writes one. A row that is not live is
	// ErrNotFound.
	PutStatus(ctx context.Context, kind, id string, observed any) error
	// Prune removes deleted rows whose deletion is at or before before
	// and whose journal rows are all acknowledged or dropped.
	Prune(ctx context.Context, before time.Time) (n int, err error)
}

// Filter selects without reading every row. Labels is equality over
// every pair; Source and Provider are what the discovery job needs to
// find the Models of one Provider, and Provider is what provider_in_use
// and the API's ?provider= read.
type Filter struct {
	Owner  string
	Labels map[string]string
	// Source is declared or discovered, and selects Models alone.
	Source string
	// Provider is a prv_ id; a Model matches when a target names that
	// id, or names by name the live Provider that has it.
	Provider string
	IDs      []string
	// Budget is a bud_ id; a Key matches when it draws on that Budget,
	// through spec.budget or spec.budgets (spec 037), so a Budget's Keys
	// are read without reading every Key.
	Budget string
}

// Page is the API's limit and cursor, clamped by the API before it gets
// here. The cursor is opaque to a caller and is checked by the store:
// base64url of "<kind>|<8 hex of SHA-256 over the filter's canonical
// JSON>|<last name>"; a cursor whose kind or filter differs from the
// request's is ErrInvalidCursor. EncodeCursor and DecodeCursor are the
// one encoding every implementation uses.
type Page struct {
	Limit  int
	Cursor string
}

// The observed halves of status, one struct per kind, in the members of
// spec 010's table. PutStatus takes the kind's struct by value or by
// pointer; a nil pointer, an empty string, or a zero time leaves the
// stored member as it was, and a non-nil empty Targets clears the list.

// ProviderObserved is what the health, discovery, and tunnel jobs write.
type ProviderObserved struct {
	Health     *v1.HealthStatus
	Discovered *v1.DiscoveredStatus
	Tunnel     *v1.TunnelStatus
}

// ModelObserved is what discovery and the health job write on a Model.
type ModelObserved struct {
	Available *bool
	Targets   []v1.TargetStatus
}

// KeyObserved is what the data plane reports about a Key.
type KeyObserved struct {
	State      v1.KeyState
	Usage      *v1.KeyUsage
	LastUsedAt time.Time
}

// BudgetObserved is what the data plane reports about a Budget.
type BudgetObserved struct {
	State     v1.BudgetState
	Spent     *v1.Money
	Remaining *v1.Money
	ResetsAt  time.Time
	Keys      *int
}

// Keys is the door's lookup: one hash to one key id, and nothing else.
// The value itself is never stored in any form but this hash, the
// SHA-256 of the value's exact bytes as 64 lower-case hex characters,
// whether the value was minted or supplied; Put refuses any other
// string with a plain error, because a hash of another shape is a
// caller's mistake and never a row. Put on a key id that already has a
// hash replaces it, which is a rotation, and the old hash is ErrNotFound
// at once; a hash registered to another key id is ErrHashTaken, and the
// API calls Put inside the Transact that writes the Key, so a create is
// atomic. Delete of a key id with no hash is ErrNotFound.
type Keys interface {
	Put(ctx context.Context, keyID, hash string) error
	ByHash(ctx context.Context, hash string) (keyID string, err error)
	Delete(ctx context.Context, keyID string) error
}

// Sealed is the row shape of a credential: four byte slices and a
// version, and nothing that could open them. It is declared here rather
// than in the secrets package so the import points from the package
// that holds the key encryption keys toward the package that holds the
// rows, and never back.
type Sealed struct {
	Version      int    // status.credential.version: counts values, not wraps
	WrappedKey   []byte // 48 bytes
	WrappedNonce []byte // 12 bytes
	Ciphertext   []byte // the value plus 16 bytes
	Nonce        []byte // 12 bytes
}

// Credentials is ciphertext in and ciphertext out. The store holds no
// key encryption key and has no method that returns a plaintext; the
// secrets package seals and opens. There is one way to write a value
// and one way to re-wrap its key, and they touch different columns.
type Credentials interface {
	// Put writes a new value: every column.
	Put(ctx context.Context, providerID string, s Sealed) error
	// Rewrap writes the two wrap columns of the row at ifVersion and
	// nothing else; a row whose version moved is ErrVersionConflict, so
	// a re-wrap never covers a value applied during its run.
	Rewrap(ctx context.Context, providerID string, ifVersion int, wrappedKey, wrappedNonce []byte) error
	Get(ctx context.Context, providerID string) (Sealed, error)
	Delete(ctx context.Context, providerID string) error
	// List returns every provider id with a row, ascending.
	List(ctx context.Context) (providerIDs []string, err error)
}

// Counters is the spend arithmetic of spec 009: one key names one window
// of one object. Add is atomic and returns the new total, so a flush is
// one round trip that both writes a delta and refreshes the replica's
// view. expiresAt is written when the key is first added and left alone
// after, because a window's end is a function of its key; a zero
// expiresAt is a window that never resets and is never pruned. Read
// answers the keys that have a row and leaves the others out.
type Counters interface {
	Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (total int64, err error)
	Read(ctx context.Context, keys []string) (map[string]int64, error)
	// Prune removes rows whose expiresAt is set and at or before before.
	Prune(ctx context.Context, before time.Time) (n int, err error)
}

// Leases name the jobs that must run on one replica at a time: discovery,
// health, journal, and usage, each with a TTL of LeaseTTL renewed at a
// third of it. Acquire by the holder that already holds the lease
// renews it and answers true; by another holder it answers true only
// once the row lapsed. Release by a holder that does not hold the lease
// changes nothing.
type Leases interface {
	Acquire(ctx context.Context, name, holder string, ttl time.Duration) (held bool, err error)
	Release(ctx context.Context, name, holder string) error
}

// The lease names and their TTL.
const (
	LeaseDiscovery = "discovery"
	LeaseHealth    = "health"
	LeaseJournal   = "journal"
	LeaseUsage     = "usage"
	LeaseTTL       = 15 * time.Second
)

// Event is the journal row of one event of spec 012. Payload is the body
// exactly as it is delivered, so a retry and a replay send the same
// bytes and the same signature input.
type Event struct {
	ID            string // evt_ and a ULID, the caller's
	GSeq          int64  // store-wide, monotonic, set by Append
	ObjectID      string // the object the event names, never empty
	Seq           int64  // per object, monotonic, set by Append
	Type          string
	At            time.Time // the clock when zero
	Payload       []byte
	Attempts      int
	NextAttemptAt time.Time // At when zero
	AckedAt       time.Time // zero while pending
}

// Journal is the durable side of the events of spec 012.
type Journal interface {
	// Append writes the row and returns its per-object sequence. An
	// event with no id or no object id, or with an id already in the
	// journal, is a plain error.
	Append(ctx context.Context, e Event) (seq int64, err error)
	// Pending returns the unacknowledged rows that are due, at most one
	// per object, each the oldest of its object, oldest first; an object
	// whose oldest row is not yet due holds its later rows back.
	Pending(ctx context.Context, limit int) ([]Event, error)
	// Acknowledge, Defer, and Drop act on one row by id; a row that is
	// not in the journal is ErrNotFound. Drop removes the row.
	Acknowledge(ctx context.Context, id string) error
	Defer(ctx context.Context, id string, attempts int, next time.Time) error
	Drop(ctx context.Context, id string) error
	// ByObject pages one object's rows by Seq ascending; the cursor is
	// the store's, from EncodeCursor over the kind journal and a Filter
	// whose IDs name the object.
	ByObject(ctx context.Context, objectID string, p Page) ([]Event, string, error)
	// Since reads the journal in global order, which is what a replica
	// tails to invalidate its Key cache before the cache window lapses
	// and what the discovery lease holder tails for a Provider change.
	// Event.GSeq is the store-wide sequence; Event.Seq is the per-object
	// one delivery orders by. A limit of 0 or less is every row.
	Since(ctx context.Context, afterGSeq int64, limit int) ([]Event, error)
	// Prune removes acknowledged rows whose At is at or before before.
	Prune(ctx context.Context, before time.Time) (n int, err error)
}

// JournalKind is the kind a ByObject cursor is encoded under.
const JournalKind = "journal"

// Usage is the aggregate side of spec 009. Records are not rows in any
// mode: the durable record set is the archive of spec 012, and
// AppendRecord feeds the process's own bounded ring, metering.RecordsPerKey
// per Key, which Records answers and GET /v1/requests serves with source
// memory when no archive is configured, on Postgres as on memory. The
// aggregates are rows: one per hour per dimension tuple, summed on
// conflict, which is what the usage API reads.
type Usage interface {
	// AddRows upserts hourly aggregate rows: a row whose key exists has
	// its sums added and its labels replaced, one that does not is
	// inserted. The Recorder's flush calls it with the hour's deltas.
	AddRows(ctx context.Context, rows []metering.Aggregate) error
	// QueryRows answers the response rows of q: the hourly rows inside
	// the range and the monthly rows whose month overlaps it (spec 038),
	// that pass the filters, grouped by q.By and summed into q.Interval,
	// as metering.Group does, so no row sums two currencies. q is
	// validated by the caller.
	QueryRows(ctx context.Context, q metering.Query) ([]metering.Row, error)
	// Hourly pages the hourly rows whose bucket is before before, in
	// metering.AggregateKey order after after, the zero key starting at
	// the first, at most limit rows. The roll-up of spec 038 reads
	// through it.
	Hourly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error)
	// Monthly pages the monthly rows the same way.
	Monthly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error)
	// Fold deletes each move's hourly row and adds the sums that row
	// held into the move's monthly row, summed on conflict with its
	// labels replaced, and deletes the monthly rows expired names, in one
	// transaction. A move whose hourly row is gone adds nothing, so a
	// pass run twice, or by two replicas at once, counts every row once.
	// It answers the hourly rows folded and the monthly rows deleted.
	Fold(ctx context.Context, moves []metering.Move, expired []metering.AggregateKey) (folded, deleted int, err error)
	// RedactOwner moves the sums of every hourly and monthly row whose
	// owner is owner onto the row with the same other dimensions and no
	// owner, and empties the owner of every ring record carrying it, in
	// one transaction, and answers the rows rewritten.
	RedactOwner(ctx context.Context, owner string) (int, error)
	// AppendRecord adds r to its Key's ring, dropping the oldest past
	// metering.RecordsPerKey.
	AppendRecord(ctx context.Context, r metering.Record) error
	// Records pages the rings' records that match q, newest first by At
	// then id; the cursor is EncodeRecordCursor's, and one from another
	// query is ErrInvalidCursor. A Limit of 0 or less is every row.
	Records(ctx context.Context, q metering.RecordQuery, p Page) ([]metering.Record, string, error)
}

// RecordsKind is the kind a Records cursor is encoded under.
const RecordsKind = "records"

// Tunnels is the registry of live tunnel sessions of spec 013, one row
// per tunneled Provider, so every replica reads the same answer.
type Tunnels interface {
	// Register writes the row, replacing any session that was there. The
	// newest session wins: an agent that reconnects after a break is
	// serving again at once rather than after the old row lapses.
	Register(ctx context.Context, t Tunnel, ttl time.Duration) error
	// Heartbeat renews the row when the session still holds it, and
	// answers false when another session replaced it or the row is
	// gone, which is how the replica holding a superseded session learns
	// to close it.
	Heartbeat(ctx context.Context, providerID, session string, ttl time.Duration) (held bool, err error)
	// Get is the live row, and ErrNotFound when there is none or it has
	// lapsed.
	Get(ctx context.Context, providerID string) (Tunnel, error)
	// Unregister removes the row when the session holds it, and changes
	// nothing when another session does.
	Unregister(ctx context.Context, providerID, session string) error
}

// Tunnel is one registry row.
type Tunnel struct {
	ProviderID  string
	Session     string    // tun_ and a ULID
	Replica     string    // the holder's forward address
	Subject     string    // the rendered subject that connected
	Agent       string    // the agent's User-Agent
	ConnectedAt time.Time // the clock when zero at Register
	ExpiresAt   time.Time // set by Register and Heartbeat
}
