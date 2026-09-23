// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"errors"
	"net/http"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lux/gateway"
)

// Code is one refusal this package raises before a stream is
// committed: a row of spec 011's error table, whose status and fixed
// user sentence are repeated here because the forward route is not
// under internal/api and writes its own envelope. internal/api's test
// holds each sentence to the table's.
type Code string

// The codes.
const (
	CodeNotFound            Code = "not_found"
	CodeUnauthenticated     Code = "unauthenticated"
	CodeInvalidRequest      Code = "invalid_request"
	CodeProviderUnavailable Code = "provider_unavailable"
	CodeStoreUnavailable    Code = "store_unavailable"
)

// rows are the codes' statuses and sentences.
var rows = map[Code]struct {
	status  int
	message string
}{
	CodeNotFound:            {http.StatusNotFound, "There is no such object."},
	CodeUnauthenticated:     {http.StatusUnauthorized, "This request needs a valid credential."},
	CodeInvalidRequest:      {http.StatusBadRequest, "The request body is not valid for this request."},
	CodeProviderUnavailable: {http.StatusServiceUnavailable, "No provider for this model is available right now."},
	CodeStoreUnavailable:    {http.StatusServiceUnavailable, "This server cannot reach its store; retry shortly."},
}

// Codes lists every code this package raises.
func Codes() []Code {
	return []Code{CodeNotFound, CodeUnauthenticated, CodeInvalidRequest, CodeProviderUnavailable, CodeStoreUnavailable}
}

// Status is the HTTP status of the code.
func (c Code) Status() int { return rows[c].status }

// Message is the fixed user sentence of the code.
func (c Code) Message() string { return rows[c].message }

// Error is one refusal: the code and the developer's detail apart, so
// the sentence a caller reads is the table's and never the detail.
type Error struct {
	Code   Code
	Detail string
}

// Error renders the developer's line.
func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}

// refuse builds an Error.
func refuse(code Code, detail string) *Error { return &Error{Code: code, Detail: detail} }

// writeError answers the forward route with spec 011's envelope: the
// code, its sentence, and details carrying the request id and the
// detail.
func writeError(w http.ResponseWriter, id string, e *Error) {
	details := map[string]any{"request_id": id}
	if e.Detail != "" {
		details["detail"] = e.Detail
	}
	w.Header().Set(gateway.HeaderError, string(e.Code))
	httpjson.WriteError(w, e.Code.Status(), httpjson.Error{Code: string(e.Code), Message: e.Code.Message(), Details: details})
}

// The failures the carrier transport raises before any byte reaches a
// runtime. The door maps each to provider_unavailable, records the
// attempt as failed, and moves to the next target; the developer's
// detail names the session and the replica.
var (
	// ErrNoSession is a tunneled Provider with no live session in the
	// registry, or one the registry names on a replica that does not
	// hold it.
	ErrNoSession = errors.New("tunnel: the Provider has no live session")
	// ErrNoCarrier is a request that found no parked carrier before its
	// own deadline.
	ErrNoCarrier = errors.New("tunnel: no parked carrier before the request's deadline")
	// ErrSessionClosed is a session that ended while a request waited on
	// it or streamed through it.
	ErrSessionClosed = errors.New("tunnel: the session closed")
	// ErrNoForward is a session held by another replica on a replica
	// without LUX_TUNNEL_FORWARD_ADDR, which cannot forward.
	ErrNoForward = errors.New("tunnel: the session is held by another replica and LUX_TUNNEL_FORWARD_ADDR is unset")
)
