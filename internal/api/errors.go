// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
)

// Code is one refusal or failure a handler on either plane writes: a
// row of spec 011's error table, which owns the HTTP status and the one
// fixed user sentence of every code, the doors' included.
type Code string

// The codes, in the table's order.
const (
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
	CodeInvalidRequest        Code = "invalid_request"
	CodeUpstreamRejected      Code = "upstream_rejected"
	CodeDialectUnsupported    Code = "dialect_unsupported"
	CodeProviderRequired      Code = "provider_required"
	CodeCurrencyMismatch      Code = "currency_mismatch"
	CodeUnauthenticated       Code = "unauthenticated"
	CodeForbidden             Code = "forbidden"
	CodeKeyDisabled           Code = "key_disabled"
	CodeKeyExpired            Code = "key_expired"
	CodeRouteNotAllowed       Code = "route_not_allowed"
	CodeModelNotAllowed       Code = "model_not_allowed"
	CodeModelUnpriced         Code = "model_unpriced"
	CodeNotFound              Code = "not_found"
	CodeModelNotFound         Code = "model_not_found"
	CodeReadOnly              Code = "read_only"
	CodeAlreadyExists         Code = "already_exists"
	CodeConflict              Code = "conflict"
	CodeKeyFenced             Code = "key_fenced"
	CodeFenceConflict         Code = "fence_conflict"
	CodeImmutableField        Code = "immutable_field"
	CodeBudgetInUse           Code = "budget_in_use"
	CodeProviderInUse         Code = "provider_in_use"
	CodeBodyTooLarge          Code = "body_too_large"
	CodeUnsupportedMediaType  Code = "unsupported_media_type"
	CodeCeilingExceeded       Code = "ceiling_exceeded"
	CodeRateLimited           Code = "rate_limited"
	CodeSpendExceeded         Code = "spend_exceeded"
	CodeBudgetExhausted       Code = "budget_exhausted"
	CodeInternal              Code = "internal"
	CodeUpstreamError         Code = "upstream_error"
	CodeAuthorizerUnavailable Code = "authorizer_unavailable"
	CodeStoreUnavailable      Code = "store_unavailable"
	CodeProviderUnavailable   Code = "provider_unavailable"
	CodeUpstreamTimeout       Code = "upstream_timeout"
)

// Row is one code's status and fixed user sentence.
type Row struct {
	Status  int
	Message string
}

// table is spec 011's error table: every code either plane writes, its
// status, and its one user sentence, never built from an underlying
// error. The developer detail travels in details.detail apart.
var table = map[Code]Row{
	CodeMalformedBody:         {http.StatusBadRequest, "The request body is not valid JSON or YAML."},
	CodeMultiDocument:         {http.StatusBadRequest, "Send one manifest per request."},
	CodeUnsupportedVersion:    {http.StatusBadRequest, "This server serves lux.latere.ai/v1beta1."},
	CodeUnsupportedKind:       {http.StatusBadRequest, "This server does not serve that kind."},
	CodeUnknownField:          {http.StatusBadRequest, "The manifest has a field this schema does not know."},
	CodeMissingField:          {http.StatusBadRequest, "A required field is missing."},
	CodeInvalidField:          {http.StatusBadRequest, "A field has a value it cannot take."},
	CodeReservedPrefix:        {http.StatusBadRequest, "That name is reserved for the gateway."},
	CodeExclusiveFields:       {http.StatusBadRequest, "Two fields that cannot be set together are set."},
	CodeDuplicateTarget:       {http.StatusBadRequest, "Two targets name the same provider and model."},
	CodeInvalidRequest:        {http.StatusBadRequest, "The request body is not valid for this request."},
	CodeUpstreamRejected:      {http.StatusBadRequest, "The provider rejected this request."},
	CodeDialectUnsupported:    {http.StatusBadRequest, "This door cannot reach the provider that serves this model."},
	CodeProviderRequired:      {http.StatusBadRequest, "Name a provider with the Lux-Provider header."},
	CodeCurrencyMismatch:      {http.StatusBadRequest, "The budget and the model are priced in different currencies."},
	CodeUnauthenticated:       {http.StatusUnauthorized, "This request needs a valid credential."},
	CodeForbidden:             {http.StatusForbidden, "You do not have permission to do this."},
	CodeKeyDisabled:           {http.StatusForbidden, "This key is disabled."},
	CodeKeyExpired:            {http.StatusForbidden, "This key has expired."},
	CodeRouteNotAllowed:       {http.StatusForbidden, "This key may not use this route."},
	CodeModelNotAllowed:       {http.StatusForbidden, "This key may not use that model."},
	CodeModelUnpriced:         {http.StatusForbidden, "This model has no price, and this key spends under a limit."},
	CodeNotFound:              {http.StatusNotFound, "There is no such object."},
	CodeModelNotFound:         {http.StatusNotFound, "There is no model of that name."},
	CodeReadOnly:              {http.StatusMethodNotAllowed, "This server reads its manifests from a directory and cannot change them."},
	CodeAlreadyExists:         {http.StatusConflict, "An object of this kind already has that name."},
	CodeKeyFenced:             {http.StatusConflict, "This key name is permanently closed to credential changes."},
	CodeFenceConflict:         {http.StatusConflict, "The key fence identity does not match."},
	CodeConflict:              {http.StatusConflict, "The object changed since you read it; read it again and retry."},
	CodeImmutableField:        {http.StatusConflict, "This field cannot be changed after the object is created."},
	CodeBudgetInUse:           {http.StatusConflict, "Keys still draw from this budget."},
	CodeProviderInUse:         {http.StatusConflict, "Models still target this provider."},
	CodeBodyTooLarge:          {http.StatusRequestEntityTooLarge, "The request body is larger than this server accepts."},
	CodeUnsupportedMediaType:  {http.StatusUnsupportedMediaType, "Send the manifest as JSON or YAML."},
	CodeCeilingExceeded:       {http.StatusUnprocessableEntity, "The value is above what you may ask for."},
	CodeRateLimited:           {http.StatusTooManyRequests, "Too many requests; wait and retry."},
	CodeSpendExceeded:         {http.StatusTooManyRequests, "This key has spent its limit for the window."},
	CodeBudgetExhausted:       {http.StatusTooManyRequests, "The budget has nothing left for the window."},
	CodeInternal:              {http.StatusInternalServerError, "Something went wrong on this server."},
	CodeUpstreamError:         {http.StatusBadGateway, "The provider returned an error."},
	CodeAuthorizerUnavailable: {http.StatusServiceUnavailable, "The permission service is unavailable; retry shortly."},
	CodeStoreUnavailable:      {http.StatusServiceUnavailable, "This server cannot reach its store; retry shortly."},
	CodeProviderUnavailable:   {http.StatusServiceUnavailable, "No provider for this model is available right now."},
	CodeUpstreamTimeout:       {http.StatusGatewayTimeout, "The provider did not answer in time."},
}

// codes is the table in its order.
var codes = []Code{
	CodeMalformedBody, CodeMultiDocument, CodeUnsupportedVersion, CodeUnsupportedKind, CodeUnknownField,
	CodeMissingField, CodeInvalidField, CodeReservedPrefix, CodeExclusiveFields, CodeDuplicateTarget,
	CodeInvalidRequest, CodeUpstreamRejected, CodeDialectUnsupported, CodeProviderRequired, CodeCurrencyMismatch,
	CodeUnauthenticated, CodeForbidden, CodeKeyDisabled, CodeKeyExpired, CodeRouteNotAllowed, CodeModelNotAllowed,
	CodeModelUnpriced, CodeNotFound, CodeModelNotFound, CodeReadOnly, CodeAlreadyExists, CodeKeyFenced, CodeFenceConflict, CodeConflict,
	CodeImmutableField, CodeBudgetInUse, CodeProviderInUse, CodeBodyTooLarge, CodeUnsupportedMediaType,
	CodeCeilingExceeded, CodeRateLimited, CodeSpendExceeded, CodeBudgetExhausted, CodeInternal, CodeUpstreamError,
	CodeAuthorizerUnavailable, CodeStoreUnavailable, CodeProviderUnavailable, CodeUpstreamTimeout,
}

// Codes lists every code of the table, in its order.
func Codes() []Code {
	out := make([]Code, len(codes))
	copy(out, codes)
	return out
}

// Status is the HTTP status of the code, and 500 for a string that is
// not one, because a code outside the table is a bug in the handler.
func (c Code) Status() int {
	if row, ok := table[c]; ok {
		return row.Status
	}
	return http.StatusInternalServerError
}

// Message is the fixed user sentence of the code, and "" for a string
// that is not one.
func (c Code) Message() string { return table[c].Message }

// Error is one refusal on its way to the caller: the code, the JSON
// paths of the fields it names, the developer detail, and the
// Retry-After of a rate refusal. It is the one value every handler
// returns and writeError renders, so no handler writes a sentence.
type Error struct {
	Code       Code
	Paths      []string
	Detail     string
	RetryAfter time.Duration
}

// Error renders the developer's line.
func (e *Error) Error() string {
	s := string(e.Code)
	if len(e.Paths) > 0 {
		s += " at " + joinPaths(e.Paths)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

func joinPaths(paths []string) string {
	var out strings.Builder
	for i, p := range paths {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(p)
	}
	return out.String()
}

// refuse builds an Error naming the paths.
func refuse(code Code, detail string, paths ...string) *Error {
	return &Error{Code: code, Paths: paths, Detail: detail}
}

// hashTakenDetail is the one developer sentence for a value another Key
// already holds. It names no Key, because the caller asking may see
// neither the Key nor its owner.
const hashTakenDetail = "a Key with this value already exists"

// hashTakenAt returns err as the refusal naming the field a create
// carried the Key's value in, spec.value or spec.valueSHA256, and any
// other error unchanged. The store's own error stays in the chain, so
// the transaction is still classified as a conflict rather than as a
// failure.
func hashTakenAt(err error, path string) error {
	if !errors.Is(err, store.ErrHashTaken) {
		return err
	}
	return fmt.Errorf("%w: %w", err, refuse(CodeInvalidField, hashTakenDetail, path))
}

// mapError turns any error a handler meets into the Error it answers
// with: an Error as it is; a manifest.Error as its code, paths, and
// detail; an auth.Error as its code and detail; a store error by name,
// ErrHashTaken as invalid_field at spec.value with a detail that names
// no Key and ErrInvalidCursor as invalid_field at cursor; and anything
// else, a Lookup's pass-through of its store's failure included, as
// store_unavailable with the developer's line.
func mapError(err error) *Error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	if me, ok := errors.AsType[*manifest.Error](err); ok {
		return &Error{Code: Code(me.Code), Paths: me.Paths, Detail: me.Detail}
	}
	if ae, ok := errors.AsType[*auth.Error](err); ok {
		return &Error{Code: Code(ae.Code), Detail: ae.Detail}
	}
	switch {
	case errors.Is(err, store.ErrKeyFenced):
		return refuse(CodeKeyFenced, err.Error())
	case errors.Is(err, store.ErrFenceConflict):
		return refuse(CodeFenceConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		return refuse(CodeNotFound, err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		return refuse(CodeConflict, err.Error())
	case errors.Is(err, store.ErrNameTaken):
		return refuse(CodeAlreadyExists, err.Error())
	case errors.Is(err, store.ErrHashTaken):
		return refuse(CodeInvalidField, hashTakenDetail, "spec.value")
	case errors.Is(err, store.ErrInvalidCursor):
		return refuse(CodeInvalidField, err.Error(), "cursor")
	case errors.Is(err, store.ErrReadOnly):
		return refuse(CodeReadOnly, err.Error())
	}
	return refuse(CodeStoreUnavailable, err.Error())
}

// writeError answers with the envelope of spec 011: one member error
// with the code, the fixed sentence, and details carrying request_id
// always, paths for a code that names fields, and detail where there is
// one; Retry-After on a rate refusal; Allow: GET on read_only. The
// sentence is the table's and never the detail's.
func writeError(w http.ResponseWriter, id string, e *Error) {
	details := map[string]any{"request_id": id}
	if len(e.Paths) > 0 {
		details["paths"] = e.Paths
	}
	if e.Detail != "" {
		details["detail"] = e.Detail
	}
	h := w.Header()
	h.Set(gateway.HeaderError, string(e.Code))
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(secondsUp(e.RetryAfter), 10))
	}
	if e.Code == CodeReadOnly {
		h.Set("Allow", http.MethodGet)
	}
	httpjson.WriteError(w, e.Code.Status(), httpjson.Error{Code: string(e.Code), Message: e.Code.Message(), Details: details})
}

// secondsUp is d in whole seconds, rounded up, at least one.
func secondsUp(d time.Duration) int64 {
	return max(int64((d+time.Second-1)/time.Second), 1)
}
