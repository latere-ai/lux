// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package filemode is the file mode of spec 010: the memory store loaded
// from a directory of manifests, with the control plane's writes
// refused. Every *.yaml and *.json file under LUX_MANIFEST_DIR is one
// object, decoded by manifest.Decode and resolved by manifest.Resolve
// under the file mode's Options, in kind order, Provider, Budget, Model,
// Key, because a Model names a Provider and a Key names a Model and a
// Budget. A Provider's credential and a Key's value come from the
// process environment through valueFrom.env, read once at start and at
// each re-read, and are held in memory: nothing is sealed, because there
// is nowhere to store it.
//
// Ids are minted per start and kept by kind and name across a re-read,
// so an id is stable for the life of the process and differs between
// starts and between replicas; a client of the mode addresses by name.
// Reload re-reads the directory into a new snapshot and swaps it in one
// Transact of the memory store, so a reader sees the old directory or
// the new one and never half of each; a directory that stops resolving
// leaves the running snapshot in place and returns the file and the
// path inside it.
//
// The store is read-only for callers and not for the gateway: Put and
// Delete of a declared object, Keys.Put and Keys.Delete, and every
// Credentials method are ErrReadOnly with the directory in the detail,
// while discovered Models, PutStatus, the counters, the leases, the
// journal, and the tunnel registry are admitted, because the jobs and
// the data plane write them.
package filemode
