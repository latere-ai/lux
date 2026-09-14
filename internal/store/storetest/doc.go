// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package storetest is the conformance suite of spec 010: Run drives
// every method of every collection, Transact, and the tunnel registry
// against a store the caller constructs, so the memory store and, in a
// later phase, the Postgres store are held to one behaviour by one set
// of cases. The memory package runs it as TestStoreConformance; the
// postgres tier runs it as TestPostgresStoreConformance under its build
// tag. Each case is named as spec 010's acceptance table names the test
// that proves the row, so a criterion is found by grepping for its name.
//
// The suite reads the clock, not a fake: a store takes no clock through
// the interface, and Postgres reads its own. The two cases that wait for
// a lapse, the leases and the tunnel registry, use a TTL of tens of
// milliseconds and sleep past it.
package storetest
