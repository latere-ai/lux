// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import "errors"

// The errors the interface names. A consumer tells them apart with
// errors.Is; an implementation may wrap them with a developer detail,
// which is how the file mode's ErrReadOnly names its directory. The
// first five are the ones the API maps to a code; ErrInvalidCursor is
// invalid_field at cursor there; ErrNested is a programming error a
// test catches.
var (
	// ErrNotFound is a row that does not exist or is deleted.
	ErrNotFound = errors.New("store: not found")
	// ErrVersionConflict is a write at a version the row has moved past,
	// or a create over an id that is already used.
	ErrVersionConflict = errors.New("store: version conflict")
	// ErrNameTaken is a create over a live row of the same kind and name.
	ErrNameTaken = errors.New("store: name taken")
	// ErrHashTaken is a Key hash registered to another key id.
	ErrHashTaken = errors.New("store: hash taken")
	// ErrReadOnly is a control-plane write in the file mode.
	ErrReadOnly = errors.New("store: read only")
	// ErrInvalidCursor is a cursor that is not this store's, or is from
	// another kind or filter than the request's.
	ErrInvalidCursor = errors.New("store: invalid cursor")
	// ErrNested is a Transact inside a Transact.
	ErrNested = errors.New("store: nested transaction")
)

// Errors lists the errors the interface names, in the order above, so a
// test can hold every implementation to raising these and no other for
// the conditions the contract states.
func Errors() []error {
	return []error{ErrNotFound, ErrVersionConflict, ErrNameTaken, ErrHashTaken, ErrReadOnly, ErrInvalidCursor, ErrNested}
}
