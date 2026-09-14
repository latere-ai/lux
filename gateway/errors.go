// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Code is one data plane refusal or failure: a row of spec 004's error
// table, whose status and fixed user sentence spec 011's table owns.
type Code string

// The codes, in the order of spec 004's table, and the two the table
// borrows: store_unavailable from spec 011 for a Key lookup or a catalog
// read the store could not answer, and ClientClosed, which appears in a
// Record and never in a response.
const (
	CodeNotFound            Code = "not_found"
	CodeBodyTooLarge        Code = "body_too_large"
	CodeUnauthenticated     Code = "unauthenticated"
	CodeKeyDisabled         Code = "key_disabled"
	CodeKeyExpired          Code = "key_expired"
	CodeRouteNotAllowed     Code = "route_not_allowed"
	CodeInvalidRequest      Code = "invalid_request"
	CodeModelNotFound       Code = "model_not_found"
	CodeModelNotAllowed     Code = "model_not_allowed"
	CodeProviderUnavailable Code = "provider_unavailable"
	CodeDialectUnsupported  Code = "dialect_unsupported"
	CodeProviderRequired    Code = "provider_required"
	CodeRateLimited         Code = "rate_limited"
	CodeModelUnpriced       Code = "model_unpriced"
	CodeCurrencyMismatch    Code = "currency_mismatch"
	CodeSpendExceeded       Code = "spend_exceeded"
	CodeBudgetExhausted     Code = "budget_exhausted"
	CodeUpstreamRejected    Code = "upstream_rejected"
	CodeUpstreamError       Code = "upstream_error"
	CodeUpstreamTimeout     Code = "upstream_timeout"
	CodeStoreUnavailable    Code = "store_unavailable"

	// ClientClosed is the record's error when the caller disconnected
	// before the response was finished. There is nobody left to answer,
	// so it is never an HTTP answer.
	ClientClosed Code = "client_closed"
)

// codeRow is one code's status and fixed user sentence.
type codeRow struct {
	status  int
	message string
}

// table is spec 011's error table restricted to the codes a door writes.
// The sentence is the one user sentence of the code, never built from an
// underlying error; the developer detail travels apart.
var table = map[Code]codeRow{
	CodeNotFound:            {http.StatusNotFound, "There is no such object."},
	CodeBodyTooLarge:        {http.StatusRequestEntityTooLarge, "The request body is larger than this server accepts."},
	CodeUnauthenticated:     {http.StatusUnauthorized, "This request needs a valid credential."},
	CodeKeyDisabled:         {http.StatusForbidden, "This key is disabled."},
	CodeKeyExpired:          {http.StatusForbidden, "This key has expired."},
	CodeRouteNotAllowed:     {http.StatusForbidden, "This key may not use this route."},
	CodeInvalidRequest:      {http.StatusBadRequest, "The request body is not valid for this request."},
	CodeModelNotFound:       {http.StatusNotFound, "There is no model of that name."},
	CodeModelNotAllowed:     {http.StatusForbidden, "This key may not use that model."},
	CodeProviderUnavailable: {http.StatusServiceUnavailable, "No provider for this model is available right now."},
	CodeDialectUnsupported:  {http.StatusBadRequest, "This door cannot reach the provider that serves this model."},
	CodeProviderRequired:    {http.StatusBadRequest, "Name a provider with the Lux-Provider header."},
	CodeRateLimited:         {http.StatusTooManyRequests, "Too many requests; wait and retry."},
	CodeModelUnpriced:       {http.StatusForbidden, "This model has no price, and this key spends under a limit."},
	CodeCurrencyMismatch:    {http.StatusBadRequest, "The budget and the model are priced in different currencies."},
	CodeSpendExceeded:       {http.StatusTooManyRequests, "This key has spent its limit for the window."},
	CodeBudgetExhausted:     {http.StatusTooManyRequests, "The budget has nothing left for the window."},
	CodeUpstreamRejected:    {http.StatusBadRequest, "The provider rejected this request."},
	CodeUpstreamError:       {http.StatusBadGateway, "The provider returned an error."},
	CodeUpstreamTimeout:     {http.StatusGatewayTimeout, "The provider did not answer in time."},
	CodeStoreUnavailable:    {http.StatusServiceUnavailable, "This server cannot reach its store; retry shortly."},
}

// Codes lists every code a door can answer, in the table's order.
func Codes() []Code {
	return []Code{
		CodeNotFound, CodeBodyTooLarge, CodeUnauthenticated, CodeKeyDisabled,
		CodeKeyExpired, CodeRouteNotAllowed, CodeInvalidRequest, CodeModelNotFound,
		CodeModelNotAllowed, CodeProviderUnavailable, CodeDialectUnsupported,
		CodeProviderRequired, CodeRateLimited, CodeModelUnpriced, CodeCurrencyMismatch,
		CodeSpendExceeded, CodeBudgetExhausted, CodeUpstreamRejected, CodeUpstreamError,
		CodeUpstreamTimeout, CodeStoreUnavailable,
	}
}

// Status is the HTTP status of the code, and 500 for a string that is
// not one, because a code outside the table is a bug in the handler.
func (c Code) Status() int {
	if row, ok := table[c]; ok {
		return row.status
	}
	return http.StatusInternalServerError
}

// Message is the fixed user sentence of the code, and "" for a string
// that is not one.
func (c Code) Message() string { return table[c].message }

// failure is one refusal or failure on its way to the caller: the code,
// the developer detail, and the Retry-After of a window refusal.
type failure struct {
	code       Code
	detail     string
	retryAfter time.Duration
}

func fail(code Code, detail string) *failure { return &failure{code: code, detail: detail} }

// Error renders the developer's line.
func (f *failure) Error() string {
	if f.detail == "" {
		return string(f.code)
	}
	return string(f.code) + ": " + f.detail
}

// The response headers of a door.
const (
	HeaderRequestID   = "Lux-Request-Id"
	HeaderError       = "Lux-Error"
	HeaderErrorDetail = "Lux-Error-Detail"
	HeaderLoss        = "Lux-Loss"
	HeaderEstimated   = "Lux-Estimated"
	HeaderProvider    = "Lux-Provider"
	HeaderLabels      = "Lux-Labels"
)

// maxDetailBytes bounds Lux-Error-Detail: one line, so an upstream body
// cannot break the response framing.
const maxDetailBytes = 1024

// detailHeader renders a developer detail as one header line of at most
// maxDetailBytes: bytes outside printable ASCII are percent-encoded and
// anything past the limit is cut on an escape boundary.
func detailHeader(detail string) string {
	var b strings.Builder
	for i := 0; i < len(detail); i++ {
		c := detail[i]
		var piece string
		if c < 0x20 || c > 0x7e {
			piece = "%" + strings.ToUpper(strconv.FormatUint(uint64(c)|0x100, 16)[1:])
		} else {
			piece = string(c)
		}
		if b.Len()+len(piece) > maxDetailBytes {
			break
		}
		b.WriteString(piece)
	}
	return b.String()
}

// googleStatus is the google.rpc.Code name a Gemini error carries beside
// the HTTP status, because that member is an enum a client may switch on.
func googleStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		return "INTERNAL"
	}
}

// openaiError is the OpenAI error shape, as a body and as a stream frame.
type openaiError struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Code    string  `json:"code"`
		Param   *string `json:"param"`
	} `json:"error"`
}

// anthropicError is the Anthropic error shape, as a body and as the data
// of an event: error frame on the /anthropic and /lux doors.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// geminiError is Google's error shape with the lux code in
// details[0].reason as Google's own ErrorInfo carries one.
type geminiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		} `json:"details"`
	} `json:"error"`
}

// envelope renders the failure in the door's own error shape. The lux
// door's is latere.ai/x/pkg/httpjson's, as on /v1; the developer detail
// is in details.detail there and in Lux-Error-Detail on every door,
// never in message. An empty door, a path under no door, renders the lux
// shape.
func envelope(door v1.Dialect, id string, f *failure) []byte {
	code, message := f.code, f.code.Message()
	var body any
	switch door {
	case v1.DialectOpenAI:
		var e openaiError
		e.Error.Message, e.Error.Type, e.Error.Code = message, string(code), string(code)
		body = e
	case v1.DialectAnthropic:
		var e anthropicError
		e.Type, e.Error.Type, e.Error.Message, e.RequestID = "error", string(code), message, id
		body = e
	case v1.DialectGemini:
		var e geminiError
		e.Error.Code, e.Error.Message, e.Error.Status = code.Status(), message, googleStatus(code.Status())
		e.Error.Details = make([]struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		}, 1)
		e.Error.Details[0].Type = "type.googleapis.com/google.rpc.ErrorInfo"
		e.Error.Details[0].Reason = string(code)
		e.Error.Details[0].Domain = "lux"
		body = e
	case v1.DialectLux, "":
		details := map[string]any{"request_id": id}
		if f.detail != "" {
			details["detail"] = f.detail
		}
		body = httpjson.ErrorEnvelope{Error: httpjson.Error{Code: string(code), Message: message, Details: details}}
	}
	out, err := json.Marshal(body)
	if err != nil {
		// Every shape above is a struct of strings and integers, which
		// always marshals; the branch is unreachable and kept for the
		// signature.
		return []byte(`{"error":{"code":"` + string(code) + `"}}`)
	}
	return append(out, '\n')
}

// writeFailure answers the caller with the failure in the door's shape:
// the code in Lux-Error, the detail in Lux-Error-Detail, Retry-After on a
// window refusal, the status of the code.
func writeFailure(w http.ResponseWriter, door v1.Dialect, id string, f *failure) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(HeaderError, string(f.code))
	if f.detail != "" {
		h.Set(HeaderErrorDetail, detailHeader(f.detail))
	}
	if f.retryAfter > 0 {
		secs := int64((f.retryAfter + time.Second - 1) / time.Second)
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	body := envelope(door, id, f)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(f.code.Status())
	_, _ = w.Write(body)
}

// streamErrorFrame is the one error frame that ends a stream which failed
// after its first byte, in the door's dialect: the OpenAI shape as a data
// frame and no [DONE] on /openai, event: error with the door's envelope
// on /anthropic and /lux, and nothing on /gemini, which the gateway
// never decodes and so never writes into.
func streamErrorFrame(door v1.Dialect, id string, f *failure) []byte {
	switch door {
	case v1.DialectOpenAI:
		body := envelope(door, id, f)
		return append(append([]byte("data: "), strings.TrimSuffix(string(body), "\n")...), "\n\n"...)
	case v1.DialectAnthropic, v1.DialectLux:
		body := envelope(door, id, f)
		return append(append([]byte("event: error\ndata: "), strings.TrimSuffix(string(body), "\n")...), "\n\n"...)
	case v1.DialectGemini, "":
		return nil
	}
	return nil
}
