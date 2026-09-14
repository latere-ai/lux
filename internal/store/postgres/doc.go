// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package postgres is the Postgres store of spec 010, selected by
// LUX_DB_URL: desired state, the key hashes, the sealed credentials, the
// spend windows, the leases, the journal, the tunnel registry, and the
// usage aggregates as rows, shared by every replica and surviving a
// restart, behind the same store.Store interface the memory store
// serves. The driver is github.com/jackc/pgx/v5 over one pgxpool.Pool
// for the process, sized by LUX_DB_MAX_CONNS; the migrations are
// embedded under migrations/ and applied at the start of luxd serve
// through latere.ai/x/pkg/pgxmigrate, after the schema guards of the
// spec have read golang-migrate's schema_migrations table. No package
// outside this one imports the driver or the migrator.
//
// Every object is stored as it marshals, its metadata and spec in one
// jsonb column and the control plane's status members in another, so a
// row is what a caller applied and nothing else; the observed members
// the jobs write through PutStatus live in a third column under the
// same member names, which is what lets a read merge the two halves
// with one jsonb concatenation and lets a zero member leave the stored
// one alone. A credential row is ciphertext in and ciphertext out, and
// the store holds no key that could open it. A journal payload is
// bytea, because a delivery is signed over the exact bytes and jsonb
// would re-spell them. The global sequence of the journal is assigned
// under a transaction-scoped advisory lock rather than by a sequence,
// so rows become visible in the order a tailing replica reads them by.
//
// The records of spec 009 are not rows in any mode: AppendRecord and
// Records are the memory store's bounded ring per Key, held by this
// process, on Postgres as on memory.
//
// The process's clock is the store's clock, as it is the memory
// store's: every timestamp a row carries is the caller's or this
// process's, and every expiry, of a lease, a tunnel row, or a due
// delivery, is compared with a now the process passes in rather than
// the database's own. The two clocks are not one thing, a database in a
// container or another zone can sit minutes from the gateway's, and a
// contract whose callers hand in Go times would read them against the
// wrong one; replicas are kept together by the clock discipline the
// installation already owes its certificates and its tokens.
package postgres
