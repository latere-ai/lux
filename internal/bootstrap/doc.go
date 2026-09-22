// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package bootstrap applies a directory of manifests into the store once
// at start, LUX_BOOTSTRAP_DIR of spec 035. Every document is decoded,
// validated, and resolved with the calls PUT /v1/{kind}s/{name} makes,
// its credential read from the environment the way the file mode reads
// one, and written in kind order, Providers, Budgets, Models, Keys, with
// the store calls the API's apply makes: the object, a Key's hash or a
// Provider's sealed credential, and one journal row, in one Transact per
// object.
//
// It writes to the store directly and asks no authorizer, because there
// is no caller to decide about: the directory is the operator's own
// desired state, and every object it creates is owned by the subject the
// caller names, the first entry of LUX_ADMIN_SUBJECTS.
//
// An apply is idempotent. Every object is read by name first: a Provider,
// Model, or Budget whose stored metadata, spec, and credential equal the
// document's is left at its version; one that differs is updated; a Key
// that exists is left as it is whatever the document says, because a
// Key's value is write-once (spec 007). Check is the same run without
// the writes, the dry run luxd check reports.
package bootstrap
