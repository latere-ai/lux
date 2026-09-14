// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxclient

import (
	"encoding/json"
	"strconv"
	"strings"

	"latere.ai/x/pkg/httpjson"
)

// Error is a refusal the server answered: the envelope of spec 011
// decoded into its parts, each kept apart so the command prints the
// sentence to a person and the rest under -v. RetryAfter is the header's
// value in seconds, 0 when the response carried none.
type Error struct {
	Status     int
	Code       string
	Message    string
	Paths      []string
	Detail     string
	RequestID  string
	RetryAfter int
}

// Error renders the developer's line: the code, the paths, the detail.
func (e *Error) Error() string {
	var b strings.Builder
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

// TransportError is a failure before any response: a dial, a TLS
// handshake, a proxy that refused, or the deadline to the first byte.
type TransportError struct {
	Method string
	URL    string
	Err    error
}

// Error renders the developer's line.
func (e *TransportError) Error() string { return e.Method + " " + e.URL + ": " + e.Err.Error() }

// Unwrap exposes the transport's own error.
func (e *TransportError) Unwrap() error { return e.Err }

// UnreadableError is a response the client cannot read as the API's: a
// status outside 2xx whose body is not the error envelope, which a proxy
// or a wrong URL produces, or a 2xx list whose body is not a list.
type UnreadableError struct {
	Method    string
	URL       string
	Status    int
	RequestID string
	Body      []byte
}

// Error renders the developer's line with an excerpt of the body.
func (e *UnreadableError) Error() string {
	excerpt := string(e.Body)
	if len(excerpt) > 200 {
		excerpt = excerpt[:200] + "..."
	}
	return e.Method + " " + e.URL + ": status " + strconv.Itoa(e.Status) + " with a body this client does not read: " + strconv.Quote(excerpt)
}

// decodeError reads a refusal's envelope into an *Error, or reports
// false for a body that is not one.
func decodeError(status int, body []byte, requestID string, retryAfter string) (*Error, bool) {
	var env httpjson.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		return nil, false
	}
	e := &Error{Status: status, Code: env.Error.Code, Message: env.Error.Message, RequestID: requestID}
	if id, ok := env.Error.Details["request_id"].(string); ok && id != "" {
		e.RequestID = id
	}
	if d, ok := env.Error.Details["detail"].(string); ok {
		e.Detail = d
	}
	if raw, ok := env.Error.Details["paths"].([]any); ok {
		for _, p := range raw {
			if s, ok := p.(string); ok {
				e.Paths = append(e.Paths, s)
			}
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && n > 0 {
		e.RetryAfter = n
	}
	return e, true
}
