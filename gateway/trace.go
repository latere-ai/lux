// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	v1 "latere.ai/x/lux/manifest/v1"
)

// This file is the data plane's half of spec 019's traces and logs: one
// lux.request span per request with one lux.upstream child per target
// tried, and one INFO line per request. The tracer is the global
// provider's, which latere.ai/x/pkg/otel installs from the OTEL_*
// variables; without an endpoint the provider is the SDK's no-op, a
// span started is a value no exporter sees, and the wiring passes
// nothing. Identity is never a span attribute: no subject, owner, Key
// id, Key prefix, or caller address, by default or by configuration.
// The log line carries the Key's prefix and owner, because the log is
// the operator's own record.

// The two span names of the table this package writes.
const (
	SpanRequest  = "lux.request"
	SpanUpstream = "lux.upstream"
)

// tracerScope names this package as the instrumentation scope.
const tracerScope = "latere.ai/x/lux/gateway"

// The attribute keys of the two spans, spec 019's table.
const (
	AttrDoor       = "lux.door"
	AttrRoute      = "lux.route"
	AttrModel      = "lux.model"
	AttrProvider   = "lux.provider"
	AttrStatus     = "lux.status"
	AttrCode       = "lux.code"
	AttrRequestID  = "lux.request_id"
	AttrStream     = "lux.stream"
	AttrTranslated = "lux.translated"
	AttrAttempt    = "lux.attempt"
	AttrTTFBMs     = "lux.ttfb_ms"
	AttrMethod     = "http.request.method"
	AttrTemplate   = "url.template"
	AttrHTTPStatus = "http.response.status_code"
)

// startRequest opens the lux.request span under the request's context,
// as a child of the context the caller propagated, when it did, through
// the W3C headers.
func startRequest(ctx context.Context, h http.Header) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(h))
	return otel.Tracer(tracerScope).Start(ctx, SpanRequest, trace.WithSpanKind(trace.SpanKindServer))
}

// startUpstream opens one lux.upstream child for an attempt.
func startUpstream(ctx context.Context, provider string, attempt int, method, template string) (context.Context, trace.Span) {
	return otel.Tracer(tracerScope).Start(ctx, SpanUpstream, trace.WithAttributes(
		attribute.String(AttrProvider, provider),
		attribute.Int(AttrAttempt, attempt),
		attribute.String(AttrMethod, method),
		attribute.String(AttrTemplate, template),
	))
}

// endUpstream closes one attempt's span with what arrived: the upstream
// status and the time to its response line, when there was one.
func endUpstream(span trace.Span, at Attempt, ttfb time.Duration) {
	if at.HTTPStatus > 0 {
		span.SetAttributes(attribute.Int(AttrHTTPStatus, at.HTTPStatus), attribute.Int64(AttrTTFBMs, ttfb.Milliseconds()))
	}
	span.End()
}

// requestAttributes is what the record puts on lux.request when it
// ends: the door, the route template, the resolved names, the outcome,
// the request id, and the two flags.
func requestAttributes(rec Record) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(AttrDoor, string(rec.Door)),
		attribute.String(AttrRoute, rec.Route),
		attribute.String(AttrModel, rec.Model),
		attribute.String(AttrProvider, rec.Provider),
		attribute.String(AttrStatus, string(rec.Status)),
		attribute.String(AttrCode, string(rec.Error)),
		attribute.String(AttrRequestID, rec.ID),
		attribute.Bool(AttrStream, rec.Stream),
		attribute.Bool(AttrTranslated, rec.Translated),
	}
}

// addAttempt appends one target tried to the record and observes it in
// the two upstream metrics.
func (c *call) addAttempt(at Attempt) {
	c.rec.Attempts = append(c.rec.Attempts, at)
	c.h.metrics.observeAttempt(at)
}

// urlTemplate is the upstream path of one attempt with the model's
// place held by {model}, the span's url.template: bounded by the route
// table and the dialects, never the caller's own path.
func (c *call) urlTemplate(t Target, m mode) string {
	switch c.route.op {
	case opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed:
		_, verb, _ := strings.Cut(c.route.template, ":")
		return "/models/{model}:" + verb
	case opChatCompletions, opResponses, opEmbeddings, opMessages, opAnthropicCount, opGenerate, opLuxCount, opModelsList, opModelsRead, opOpaque, opNone:
	}
	if m == modeTranslate {
		d := t.Provider.Spec.Dialect
		return upstreamPath(d, c.route.op, d == v1.DialectOpenAI && OpenAIReasoningFamily(t.Model))
	}
	return passthroughPath(c.door, c.route.rest)
}

// LogRequest is the message of the data plane's one line per request,
// spec 019's field row for the plane, written at INFO inside the
// request's span so the shared handler adds trace_id and span_id. Every
// field is a fixed name from the row and every value is a scalar the
// encoder escapes; no body, header value, credential, or caller string
// is a field name.
const LogRequest = SpanRequest

func logRequest(ctx context.Context, logger *slog.Logger, rec Record) {
	logger.LogAttrs(ctx, slog.LevelInfo, LogRequest,
		slog.String("request_id", rec.ID),
		slog.String("door", string(rec.Door)),
		slog.String("route", rec.Route),
		slog.String("model", rec.Model),
		slog.String("provider", rec.Provider),
		slog.String("status", string(rec.Status)),
		slog.String("code", string(rec.Error)),
		slog.String("key_prefix", rec.KeyPrefix),
		slog.String("owner", rec.Owner),
		slog.Int64("duration_ms", rec.Latency.Milliseconds()),
		slog.Int64("ttfb_ms", rec.TTFB.Milliseconds()),
		slog.Int64("input_tokens", rec.Tokens.Input),
		slog.Int64("output_tokens", rec.Tokens.Output),
		slog.Bool("stream", rec.Stream),
	)
}
