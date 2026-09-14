// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"strings"
)

// Code is one refusal of Decode or Resolve. The data plane's codes are the
// gateway's and are never raised here.
type Code string

// The codes, in the order of spec 003's error table.
const (
	CodeUnsupportedMediaType  Code = "unsupported_media_type"
	CodeMalformedBody         Code = "malformed_body"
	CodeMultiDocument         Code = "multi_document"
	CodeUnsupportedVersion    Code = "unsupported_version"
	CodeUnsupportedKind       Code = "unsupported_kind"
	CodeUnknownField          Code = "unknown_field"
	CodeMissingField          Code = "missing_field"
	CodeInvalidField          Code = "invalid_field"
	CodeReservedPrefix        Code = "reserved_prefix"
	CodeExclusiveFields       Code = "exclusive_fields"
	CodeDuplicateTarget       Code = "duplicate_target"
	CodeNotFound              Code = "not_found"
	CodeImmutableField        Code = "immutable_field"
	CodeCeilingExceeded       Code = "ceiling_exceeded"
	CodeAuthorizerUnavailable Code = "authorizer_unavailable"
)

// messages are the fixed user sentences of spec 011's error table, one
// per code. The API writes the same sentence for the same code, so a
// caller reads one sentence whichever surface refused.
var messages = map[Code]string{
	CodeUnsupportedMediaType:  "Send the manifest as JSON or YAML.",
	CodeMalformedBody:         "The request body is not valid JSON or YAML.",
	CodeMultiDocument:         "Send one manifest per request.",
	CodeUnsupportedVersion:    "This server serves lux.latere.ai/v1beta1.",
	CodeUnsupportedKind:       "This server does not serve that kind.",
	CodeUnknownField:          "The manifest has a field this schema does not know.",
	CodeMissingField:          "A required field is missing.",
	CodeInvalidField:          "A field has a value it cannot take.",
	CodeReservedPrefix:        "That name is reserved for the gateway.",
	CodeExclusiveFields:       "Two fields that cannot be set together are set.",
	CodeDuplicateTarget:       "Two targets name the same provider and model.",
	CodeNotFound:              "There is no such object.",
	CodeImmutableField:        "This field cannot be changed after the object is created.",
	CodeCeilingExceeded:       "The value is above what you may ask for.",
	CodeAuthorizerUnavailable: "The permission service is unavailable; retry shortly.",
}

// Codes lists every code this package raises, in the table's order.
func Codes() []Code {
	return []Code{
		CodeUnsupportedMediaType, CodeMalformedBody, CodeMultiDocument,
		CodeUnsupportedVersion, CodeUnsupportedKind, CodeUnknownField,
		CodeMissingField, CodeInvalidField, CodeReservedPrefix,
		CodeExclusiveFields, CodeDuplicateTarget, CodeNotFound,
		CodeImmutableField, CodeCeilingExceeded, CodeAuthorizerUnavailable,
	}
}

// Message is the fixed user sentence of the code, and "" for a string
// that is not one of the codes.
func (c Code) Message() string { return messages[c] }

// Error is one refusal: one code, the JSON paths of the fields it names,
// the fixed user sentence of the code in Message, and the developer's
// detail apart, so the two registers never share a field. A path is
// dotted for a struct field, [n] for a list entry, and ["k"] for a map
// key: spec.targets[1].model, metadata.labels["tier"].
type Error struct {
	Code    Code
	Paths   []string
	Message string
	Detail  string
}

// Error renders the developer's line: the code, the paths, the detail.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
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

// refuse builds an Error with the code's fixed sentence.
func refuse(code Code, detail string, paths ...string) *Error {
	return &Error{Code: code, Paths: paths, Message: code.Message(), Detail: detail}
}

// The answers a Lookup gives for a reference it cannot resolve. Resolve
// turns them into the codes not_found and authorizer_unavailable at the
// field's path; a Lookup may also return an *Error carrying either code.
var (
	ErrNotFound              = errors.New("not found")
	ErrAuthorizerUnavailable = errors.New("authorizer unavailable")
)
