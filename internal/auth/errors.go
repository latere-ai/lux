// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import "latere.ai/x/lux/manifest"

// Code is one refusal of this package: the caller presented no credential
// this plane accepts, was denied the action it asked, or no decision
// could be had. The HTTP status of each is spec 011's.
type Code string

// The codes.
const (
	CodeUnauthenticated       Code = "unauthenticated"
	CodeForbidden             Code = "forbidden"
	CodeAuthorizerUnavailable Code = Code(manifest.CodeAuthorizerUnavailable)
)

// messages are the fixed user sentences of spec 011's error table, one
// per code; authorizer_unavailable is the same sentence manifest writes,
// so a caller reads one whichever surface refused.
var messages = map[Code]string{
	CodeUnauthenticated:       "This request needs a valid credential.",
	CodeForbidden:             "You do not have permission to do this.",
	CodeAuthorizerUnavailable: manifest.CodeAuthorizerUnavailable.Message(),
}

// Codes lists every code this package raises.
func Codes() []Code {
	return []Code{CodeUnauthenticated, CodeForbidden, CodeAuthorizerUnavailable}
}

// Message is the fixed user sentence of the code, and "" for a string
// that is not one of the codes.
func (c Code) Message() string { return messages[c] }

// Error is one refusal: one code, the fixed user sentence of the code in
// Message, and the developer's detail apart, so an authorizer's reason or
// a verifier's finding never reaches the user's sentence.
type Error struct {
	Code    Code
	Message string
	Detail  string
}

// Error renders the developer's line: the code and the detail.
func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}

// refuse builds an Error with the code's fixed sentence.
func refuse(code Code, detail string) *Error {
	return &Error{Code: code, Message: code.Message(), Detail: detail}
}
