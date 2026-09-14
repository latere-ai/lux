// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package events is the event stream of spec 012: the record every
// writer journals, the type table a sink keys on, the Lux-Signature a
// sink verifies, the client that posts one event to the operator's sink,
// and the worker that delivers the journal at least once and in order
// per object.
//
// A writer, the API of spec 011 or a job of internal/serve, calls Append
// inside the transaction that commits the fact the event reports, so the
// row commits with the object it names. The Worker on the replica holding
// the journal lease reads Journal.Pending, which answers at most one due
// row per object, posts each with a fresh signature, acknowledges a 2xx,
// and defers anything else on the backoff of DeliveryPolicy until
// GiveUpAfter, when the row is dropped with an ERROR line. A row
// journalled before the sink was first set is acknowledged unsent, so an
// installation that names a sink later is not replayed its history.
package events
