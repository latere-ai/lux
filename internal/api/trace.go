// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// This file is the control plane's half of spec 019's traces and logs:
// one lux.api span per request, a child of the context the caller
// propagated, with the authorizer's and the store's spans under it, and
// one INFO line per request with that spec's fields for the plane. The
// tracer is the global provider's, so the wiring passes nothing and
// without an endpoint every span is the no-op's. The span carries the
// route template, the action, the kind, the outcome, and the request
// id, and never the subject; the log line carries the subject, because
// the log is the operator's own record.

// SpanAPI is the span's name and LogAPI the line's message.
const (
	SpanAPI     = "lux.api"
	LogAPI      = SpanAPI
	tracerScope = "latere.ai/x/lux/internal/api"
)

// The attribute keys of lux.api, spec 019's table.
const (
	AttrRoute     = "lux.route"
	AttrAction    = "lux.action"
	AttrKind      = "lux.kind"
	AttrStatus    = "lux.status"
	AttrCode      = "lux.code"
	AttrRequestID = "lux.request_id"
)

// The status field's closed set on the control plane: an answer, a
// refusal the caller can act on, and the gateway's or a dependency's
// failure.
const (
	StatusOK      = "ok"
	StatusRefused = "refused"
	StatusFailed  = "failed"
)

// startAPI opens the lux.api span as a child of the context the caller
// propagated, when it did, through the W3C headers.
func startAPI(r *http.Request) (context.Context, trace.Span) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	return otel.Tracer(tracerScope).Start(ctx, SpanAPI, trace.WithSpanKind(trace.SpanKindServer))
}

// outcome is the status and code the request ended with: ok without a
// refusal, refused for a code answered under 500, failed at or above.
func (c *call) outcome() (status, code string) {
	switch {
	case c.code == "":
		return StatusOK, ""
	case c.code.Status() >= http.StatusInternalServerError:
		return StatusFailed, string(c.code)
	default:
		return StatusRefused, string(c.code)
	}
}

// route is the template of the route the mux matched, spec 011's route
// table, and empty for a request no route answered: the mux's catch-all
// and a refusal before the mux are not routes.
func (c *call) route() string {
	pattern := c.r.Pattern
	if pattern == "/" {
		return ""
	}
	return pattern
}

// end writes the request's line inside its span, sets the span's
// attributes, and ends it. It runs once per request, after the route
// answered or the refusal was written.
func (c *call) end(ctx context.Context, span trace.Span) {
	status, code := c.outcome()
	route := c.route()
	c.h.logger.LogAttrs(ctx, slog.LevelInfo, LogAPI,
		slog.String("request_id", c.id),
		slog.String("route", route),
		slog.String("action", c.action),
		slog.String("kind", c.kind),
		slog.String("name", c.r.PathValue("name")),
		slog.String("status", status),
		slog.String("code", code),
		slog.String("subject", c.caller.Subject),
		slog.Int64("duration_ms", c.h.o.Now().Sub(c.start).Milliseconds()),
	)
	span.SetAttributes(
		attribute.String(AttrRoute, route),
		attribute.String(AttrAction, c.action),
		attribute.String(AttrKind, c.kind),
		attribute.String(AttrStatus, status),
		attribute.String(AttrCode, code),
		attribute.String(AttrRequestID, c.id),
	)
	span.End()
}
