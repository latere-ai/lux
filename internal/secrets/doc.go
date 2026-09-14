// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package secrets is the credential custody of spec 005: envelope
// encryption of a Provider's credential value under the key encryption
// keys of LUX_SECRETS_KEK. A value is sealed under a fresh 32-byte data
// key with AES-256-GCM, the data key is wrapped under the first listed
// key with AES-256-GCM, and both AEADs bind the provider id and the
// credential version as additional data, so a row copied onto another
// Provider or another version fails to open. The four byte strings and
// the version are the store.Sealed row of spec 010; this package holds
// the keys and the store holds the rows, and the import points from
// here toward the store and never back.
//
// Parse reads the variable and names a failing key by position and
// never by value; Seal, Open, and Rewrap are the three operations, and
// Unwrap opens the wrapped data key alone, which is what the start-up
// check and luxd rewrap need without ever touching a value.
package secrets
