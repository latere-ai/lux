// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
)

// recordSpans installs an SDK provider with an in-memory recorder as
// the global one, with the W3C propagator, and puts the no-op back when
// the test ends.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	return sr
}

func attrsOf(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

// apiAttrKeys is the attribute set of lux.api in spec 019's table.
var apiAttrKeys = []string{AttrRoute, AttrAction, AttrKind, AttrStatus, AttrCode, AttrRequestID}

// TestRequestSpans is spec 019's row for the control plane: one lux.api
// span per request, a child of the context the caller propagated, with
// the authorizer's and the store's spans as its children, the attributes
// exactly the table's, and no identity on any span; a request no route
// answers is a span too, with the refusal's code and no route.
func TestRequestSpans(t *testing.T) {
	sr := recordSpans(t)
	h := newHarness(t, func(o *Options) { o.Store = store.Instrument(o.Store, metrics.NewRegistry()) })
	rec := h.request(http.MethodPut, "/v1/providers/openai", providerJSON, "traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var api sdktrace.ReadOnlySpan
	var authorizers, stores int
	for _, s := range sr.Ended() {
		switch s.Name() {
		case SpanAPI:
			if api != nil {
				t.Fatal("two lux.api spans for one request")
			}
			api = s
		case auth.SpanAuthorizer:
			authorizers++
		case store.SpanStore:
			stores++
		}
	}
	if api == nil {
		t.Fatal("no lux.api span")
	}
	if api.SpanKind() != trace.SpanKindServer || api.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || api.Parent().SpanID().String() != "b7ad6b7169203331" {
		t.Errorf("lux.api is not the caller's child: kind %v trace %s parent %s", api.SpanKind(), api.SpanContext().TraceID(), api.Parent().SpanID())
	}
	attrs := attrsOf(api)
	if got := slices.Sorted(maps.Keys(attrs)); !slices.Equal(got, slices.Sorted(slices.Values(apiAttrKeys))) {
		t.Errorf("attributes %v", got)
	}
	want := map[string]string{AttrRoute: "/v1/providers/{name}", AttrAction: auth.ActionProviderCreate, AttrKind: "Provider", AttrStatus: StatusOK, AttrCode: "", AttrRequestID: rec.Header().Get("Lux-Request-Id")}
	for k, v := range want {
		if attrs[k].AsString() != v {
			t.Errorf("%s = %q, want %q", k, attrs[k].AsString(), v)
		}
	}
	if authorizers == 0 || stores == 0 {
		t.Errorf("%d authorizer and %d store spans under lux.api", authorizers, stores)
	}
	for _, s := range sr.Ended() {
		if s.Name() != SpanAPI && s.Parent().SpanID() != api.SpanContext().SpanID() {
			t.Errorf("%s is not lux.api's child", s.Name())
		}
		for _, kv := range s.Attributes() {
			for _, identity := range []string{"alice", h.iss.URL(), "203.0.113.9", canary} {
				if strings.Contains(kv.Value.Emit(), identity) {
					t.Errorf("span %s attribute %s carries %q", s.Name(), kv.Key, identity)
				}
			}
		}
	}

	sr.Reset()
	rec = h.request(http.MethodGet, "/v1/nothing", "")
	wantCode(t, rec, CodeNotFound)
	var refused []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == SpanAPI {
			refused = append(refused, s)
		}
	}
	if len(refused) != 1 {
		t.Fatalf("%d lux.api spans for a refusal", len(refused))
	}
	if a := attrsOf(refused[0]); a[AttrStatus].AsString() != StatusRefused || a[AttrCode].AsString() != "not_found" || a[AttrRoute].AsString() != "" || a[AttrAction].AsString() != "" {
		t.Errorf("refusal attributes %v", a)
	}
}

// controlLogFields is spec 019's field row for the control plane.
var controlLogFields = []string{"request_id", "route", "action", "kind", "name", "status", "code", "subject", "duration_ms"}

// requestLines is every line the surface wrote with the request
// message, parsed.
func requestLines(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON line: %v\n%s", err, line)
		}
		if m["msg"] == LogAPI {
			out = append(out, m)
		}
	}
	return out
}

// TestAPILineFields is the control plane's half of spec 019's log row at
// the package: one INFO line per request named after the span, with
// exactly the row's fields beside the encoder's three, the subject on
// the line and the route as a template, and a name of newlines and
// terminal escapes producing one escaped line.
func TestAPILineFields(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodPut, "/v1/providers/openai", providerJSON)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	lines := requestLines(t, h.log.String())
	if len(lines) != 1 {
		t.Fatalf("%d request lines:\n%s", len(lines), h.log.String())
	}
	line := lines[0]
	fields := slices.Sorted(maps.Keys(line))
	fields = slices.DeleteFunc(fields, func(k string) bool { return k == "time" || k == "level" || k == "msg" })
	if !slices.Equal(fields, slices.Sorted(slices.Values(controlLogFields))) {
		t.Errorf("fields %v", fields)
	}
	want := map[string]any{"request_id": rec.Header().Get("Lux-Request-Id"), "route": "/v1/providers/{name}", "action": auth.ActionProviderCreate, "kind": "Provider", "name": "openai",
		"status": StatusOK, "code": "", "subject": h.subject(), "duration_ms": float64(0)}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if line["level"] != "INFO" {
		t.Errorf("level %v", line["level"])
	}
	if strings.Contains(h.log.String(), canary) {
		t.Error("the credential reached the log")
	}

	h.log.Reset()
	rec = h.request(http.MethodPut, "/v1/budgets/x%0Ay%1B%5B31mz", budgetJSON)
	if rec.Code < 400 {
		t.Fatalf("a hostile name was applied: %d", rec.Code)
	}
	lines = requestLines(t, h.log.String())
	if len(lines) != 1 || lines[0]["name"] != "x\ny\x1b[31mz" || lines[0]["status"] != StatusRefused || lines[0]["route"] != "/v1/budgets/{name}" {
		t.Errorf("a hostile name produced %d lines: %v", len(lines), lines)
	}
	if strings.Contains(h.log.String(), "\x1b") || strings.Count(strings.TrimSpace(h.log.String()), "\n") != 0 {
		t.Errorf("the name's bytes reached the log raw: %q", h.log.String())
	}

	// The file mode's public /v1 writes a line too.
	h.log.Reset()
	rec2 := httptest.NewRecorder()
	h.h.Unmounted().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v1/self", nil))
	wantCode(t, rec2, CodeNotFound)
	lines = requestLines(t, h.log.String())
	if len(lines) != 1 || lines[0]["code"] != "not_found" || lines[0]["route"] != "" || lines[0]["subject"] != "" {
		t.Errorf("unmounted produced %d lines: %v", len(lines), lines)
	}
}
