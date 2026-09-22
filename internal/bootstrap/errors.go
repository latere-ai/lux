// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"errors"
	"strings"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
)

// The codes a refusal carries beside the manifest package's own: spec
// 011's names for the store's errors and for the failures of the
// credential path, as the API maps them, so one condition reads as one
// code whether it met /v1 or the bootstrap directory.
const (
	codeNotFound         = string(manifest.CodeNotFound)
	codeMissingField     = string(manifest.CodeMissingField)
	codeInvalidField     = string(manifest.CodeInvalidField)
	codeAlreadyExists    = "already_exists"
	codeConflict         = "conflict"
	codeKeyFenced        = "key_fenced"
	codeFenceConflict    = "fence_conflict"
	codeReadOnly         = "read_only"
	codeInternal         = "internal"
	codeStoreUnavailable = "store_unavailable"
)

// hashTakenDetail is the API's developer sentence for a value another
// Key already holds, which names no Key.
const hashTakenDetail = "a Key with this value already exists"

// Error is one refusal of the directory: the file it is in, the object
// it names when the document decoded far enough to say, spec 011's code,
// the JSON paths of the fields at fault, and the developer's detail. A
// refusal ends the run, so it is also the start-up failure's message.
type Error struct {
	// File is the directory joined with the file's path under it, the
	// path an operator opens.
	File string
	// Kind and Name are the object's, empty when the document did not
	// carry them in a shape that could be read.
	Kind, Name string
	// Code is the code /v1 answers the same condition with.
	Code   string
	Paths  []string
	Detail string
	err    error
}

// Error renders "<file>: <Kind> <name>: <code> at <paths>: <detail>",
// leaving out the parts that are empty.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Kind != "" {
		b.WriteString(": ")
		b.WriteString(e.Kind)
		if e.Name != "" {
			b.WriteString(" ")
			b.WriteString(e.Name)
		}
	}
	b.WriteString(": ")
	b.WriteString(e.Code)
	if len(e.Paths) > 0 {
		b.WriteString(" at ")
		b.WriteString(strings.Join(e.Paths, ", "))
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Unwrap is the error the refusal was made from, so a caller can still
// tell a store's failure apart with errors.Is.
func (e *Error) Unwrap() error { return e.err }

// refuse builds the refusal of one document with a code of its own.
func (d *document) refuse(code, detail string, paths ...string) *Error {
	return &Error{File: d.path, Kind: d.kind, Name: d.name, Code: code, Paths: paths, Detail: detail}
}

// fail names err at the document the way the API's mapError maps it: a
// manifest.Error keeps its code, paths, and detail; a store error takes
// the code of its name, ErrHashTaken invalid_field at the field the
// Key's value arrived in; anything else, a Lookup's pass-through of the
// store's failure included, is store_unavailable.
func (d *document) fail(err error, valuePath string) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	e := &Error{File: d.path, Kind: d.kind, Name: d.name, Detail: err.Error(), err: err}
	if me, ok := errors.AsType[*manifest.Error](err); ok {
		e.Code, e.Paths, e.Detail = string(me.Code), me.Paths, me.Detail
		return e
	}
	switch {
	case errors.Is(err, store.ErrKeyFenced):
		e.Code = codeKeyFenced
	case errors.Is(err, store.ErrFenceConflict):
		e.Code = codeFenceConflict
	case errors.Is(err, store.ErrNotFound):
		e.Code = codeNotFound
	case errors.Is(err, store.ErrVersionConflict):
		e.Code = codeConflict
	case errors.Is(err, store.ErrNameTaken):
		e.Code = codeAlreadyExists
	case errors.Is(err, store.ErrHashTaken):
		e.Code, e.Paths, e.Detail = codeInvalidField, []string{valuePath}, hashTakenDetail
	case errors.Is(err, store.ErrReadOnly):
		e.Code = codeReadOnly
	default:
		e.Code = codeStoreUnavailable
	}
	return e
}
