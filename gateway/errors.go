// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/bridge"

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

// failureOf is the failure in the bridge's vocabulary: the code as the
// machine-readable member of every shape, the code's fixed sentence, the
// developer detail, which only the lux shape carries in its body, the
// request id, the status, and the domain of the Google ErrorInfo.
func failureOf(id string, f *failure) bridge.Failure {
	return bridge.Failure{
		Code:      string(f.code),
		Message:   f.code.Message(),
		Detail:    f.detail,
		RequestID: id,
		Status:    f.code.Status(),
		Domain:    "lux",
	}
}

// envelope renders the failure in the door's own error shape. The lux
// door's is latere.ai/x/pkg/httpjson's, as on /v1; the developer detail
// is in details.detail there and in Lux-Error-Detail on every door,
// never in message. An empty door, a path under no door, renders the lux
// shape.
func envelope(door v1.Dialect, id string, f *failure) []byte {
	return bridge.Envelope(wireOf(door), failureOf(id, f))
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

// WriteRefusal answers a request refused before the handler ran, in the
// shape of the door path names and in the lux shape for a path under no
// door: the code's status and fixed sentence, Lux-Error, the developer
// detail in Lux-Error-Detail and, on the lux shape, in details.detail,
// and Retry-After when retryAfter is above zero. It is the one way spec
// 011's per-address bucket, which runs in front of the doors, refuses
// in a door's own dialect; id is the request id the caller minted.
func WriteRefusal(w http.ResponseWriter, path, id string, code Code, detail string, retryAfter time.Duration) {
	d, _ := door(path)
	writeFailure(w, d, id, &failure{code: code, detail: detail, retryAfter: retryAfter})
}

// streamErrorFrame is the one error frame that ends a stream which failed
// after its first byte, in the door's dialect: the OpenAI shape as a data
// frame and no [DONE] on /openai, event: error with the door's envelope
// on /anthropic and /lux, and nothing on /gemini, which the gateway
// never decodes and so never writes into.
func streamErrorFrame(door v1.Dialect, id string, f *failure) []byte {
	return bridge.ErrorFrame(wireOf(door), failureOf(id, f))
}
