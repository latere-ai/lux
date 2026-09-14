// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package memory is the default store of spec 010 and what every unit
// test runs against: maps under one lock, a write lock held for the
// length of a Transact, and nothing that survives the process. Desired
// state, the counters, the journal, and the leases live with the process
// and are gone at its end; the serve role says so at start in one line
// naming the three consequences, no recovery, every window starting
// empty, and this process being assumed the only replica, because two
// in-memory replicas cannot detect each other. The file mode of
// internal/store/filemode is this store loaded from a directory.
//
// Every value the store holds is its own copy: an object is cloned
// through its JSON form on the way in, which also drops the write-only
// values the encoders skip, and cloned again on the way out, so a caller
// that keeps writing into what it passed or received changes nothing
// here. Rows are values in maps and are replaced, never edited in place,
// which is what lets Transact take a snapshot by copying the maps and
// put it back when fn fails.
package memory
