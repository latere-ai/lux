// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package store is the contract of spec 010: the Store interface every
// consumer takes, its collections, the row shapes that are the store's
// own (Sealed, Event, Tunnel, and the observed halves of status), the
// errors the interface names, the opaque cursor, and Instrument, which
// counts every operation. The implementations live beneath it: memory,
// the file mode over memory, and, in a later phase, Postgres. The
// package imports manifest/v1 for the kinds, latere.ai/x/pkg/metrics
// for the counter, and the standard library, and nothing of the
// implementations, so a consumer that takes the interface pulls in no
// driver.
package store
