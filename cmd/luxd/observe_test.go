// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// The request telemetry of the public listener: latere.ai/x/pkg/otel's
// Handler around the whole listener records http.server.request.duration
// for every request but the probes, labeled with a bounded http.route,
// and opens the one SERVER span of the request, under which lux.request
// and lux.api are INTERNAL children.

// inMemory is the process's meter and tracer providers replaced by an
// in-memory reader and recorder for one test, installed before serve
// builds its handlers, because otelhttp takes its meter when the handler
// is built.
type inMemory struct {
	reader *sdkmetric.ManualReader
	spans  *tracetest.SpanRecorder
}

func installInMemory(t *testing.T) inMemory {
	t.Helper()
	// Without an endpoint Bootstrap installs no provider of its own, so
	// the ones set here are the ones serve's handlers find.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(rec))
	prevMeter, prevTracer, prevProp := otel.GetMeterProvider(), otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMeter)
		otel.SetTracerProvider(prevTracer)
		otel.SetTextMapPropagator(prevProp)
		if err := mp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
		if err := tp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("tracer provider shutdown: %v", err)
		}
	})
	return inMemory{reader: reader, spans: rec}
}

// durationCounts is the number of requests http.server.request.duration
// recorded by http.route, "" for a point without one.
func (m inMemory) durationCounts(t *testing.T) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != "http.server.request.duration" {
				continue
			}
			h, ok := md.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("http.server.request.duration is a %T", md.Data)
			}
			for _, dp := range h.DataPoints {
				route, _ := dp.Attributes.Value("http.route")
				out[route.AsString()] += dp.Count
			}
		}
	}
	return out
}

// observedRequest is one request to the public listener and the
// http.route it is recorded under; probe marks a request that must leave
// no point and no span.
type observedRequest struct {
	method, path string
	route        string
	probe        bool
}

// callerKeys are the attributes that would put the caller's address or
// client on a span, which spec 019 forbids on every span.
var callerKeys = []string{"client.address", "client.port", "network.peer.address", "network.peer.port", "user_agent.original"}

// TestPublicListenerRecordsRequestMetrics drives the real public listener
// under each mount of the base path and reads what the in-memory
// providers received after the listener stopped: one duration point per
// request under its bounded route, none for a probe, no raw name or path
// in any route, and exactly one SERVER span per request that carries no
// caller address.
func TestPublicListenerRecordsRequestMetrics(t *testing.T) {
	const base = "/v1/models"
	manifests := t.TempDir()
	writeFile(t, manifests, "provider.yaml", providerYAML)
	doorsAndPlane := func(prefix string, control func(string) string) []observedRequest {
		return []observedRequest{
			{http.MethodGet, prefix + "/version", "/version", false},
			{http.MethodGet, prefix + "/.well-known/lux", "/.well-known/lux", false},
			{http.MethodPost, prefix + "/openai/v1/chat/completions", "/openai/v1/chat/completions", false},
			{http.MethodPost, prefix + "/openai/v1/files/file-raw-91", "/openai/v1/*", false},
			{http.MethodGet, prefix + "/anthropic/v1/models/claude-raw-7", "/anthropic/v1/models/{model}", false},
			{http.MethodPost, prefix + "/gemini/v1beta/models/gemini-raw-3:generateContent", "/gemini/v1beta/models/{model}:generateContent", false},
			{http.MethodPost, prefix + "/lux/v1/generate", "/lux/v1/generate", false},
			{http.MethodGet, prefix + "/openai/nowhere-raw-5", "", false},
			{http.MethodGet, control("/keys/key-raw-11"), "/v1/keys/{name}", false},
			{http.MethodPost, control("/keys/key-raw-11/rotate"), "/v1/keys/{name}/rotate", false},
			{http.MethodGet, control("/models/org-raw/model-raw-2"), "/v1/models/{name...}", false},
			{http.MethodGet, control("/nothing-raw-13"), "", false},
		}
	}
	modes := []struct {
		name     string
		env      map[string]string
		requests []observedRequest
	}{
		{
			name: "at the root",
			requests: append(doorsAndPlane("", func(p string) string { return "/v1" + p }),
				observedRequest{http.MethodGet, "/", "/", false},
				observedRequest{http.MethodHead, "/", "/", false},
				observedRequest{http.MethodPost, "/version", "", false},
				observedRequest{http.MethodGet, "/livez", "", true},
				observedRequest{http.MethodGet, "/readyz", "", true},
				observedRequest{http.MethodGet, "/elsewhere-raw-17", "", false},
			),
		},
		{
			name: "under a base path",
			env:  map[string]string{"LUX_BASE_PATH": base, "LUX_PUBLIC_URL": "https://api.example.com" + base},
			requests: append(doorsAndPlane(base, func(p string) string { return base + "/v1" + p }),
				observedRequest{http.MethodGet, base, "/", false},
				observedRequest{http.MethodGet, base + "/livez", "", true},
				observedRequest{http.MethodGet, base + "/readyz", "", true},
				observedRequest{http.MethodGet, "/v1/keys/outside-raw-19", "", false},
			),
		},
		{
			name: "under a base path in the place of /v1",
			env:  map[string]string{"LUX_BASE_PATH": base, "LUX_BASE_PATH_MODE": "replace", "LUX_PUBLIC_URL": "https://api.example.com" + base},
			requests: append(doorsAndPlane(base, func(p string) string { return base + p }),
				observedRequest{http.MethodGet, base, "/", false},
				// The probes are the internal listener's here; under the
				// base they are control plane paths no route answers.
				observedRequest{http.MethodGet, base + "/livez", "", false},
			),
		},
		{
			// The file mode's public listener answers not_found under
			// /v1, so no control plane route is named there.
			name: "in the file mode",
			env:  map[string]string{"LUX_MANIFEST_DIR": manifests, "OPENAI_KEY": "sk-live", "LUX_OIDC_ISSUERS": "", "LUX_SECRETS_KEK": ""},
			requests: []observedRequest{
				{http.MethodGet, "/.well-known/lux", "/.well-known/lux", false},
				{http.MethodPost, "/openai/v1/chat/completions", "/openai/v1/chat/completions", false},
				{http.MethodGet, "/v1/providers/openai", "", false},
				{http.MethodGet, "/livez", "", true},
			},
		},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			mem := installInMemory(t)
			srv := startServe(t, mode.env)
			want := map[string]uint64{}
			served := 0
			for _, rq := range mode.requests {
				resp, body := do(t, rq.method, srv.publicURL+rq.path, "", "User-Agent", "canary-agent/1.0", "X-Forwarded-For", "203.0.113.9")
				if resp.StatusCode == 0 {
					t.Fatalf("%s %s: no answer: %s", rq.method, rq.path, body)
				}
				if !rq.probe {
					want[rq.route]++
					served++
				}
			}
			if code := srv.stop(); code != 0 {
				t.Fatalf("serve exited %d", code)
			}

			got := mem.durationCounts(t)
			if !maps.Equal(got, want) {
				t.Errorf("http.server.request.duration counts by http.route:\n got %v\nwant %v", got, want)
			}
			for route := range got {
				if strings.Contains(route, "raw") {
					t.Errorf("http.route %q carries a caller's path segment", route)
				}
			}

			servers := map[trace.TraceID]int{}
			for _, s := range mem.spans.Ended() {
				if s.SpanKind() != trace.SpanKindServer {
					continue
				}
				servers[s.SpanContext().TraceID()]++
				for _, kv := range s.Attributes() {
					if slices.Contains(callerKeys, string(kv.Key)) {
						t.Errorf("SERVER span %q carries %s", s.Name(), kv.Key)
					}
					if strings.Contains(kv.Value.String(), "canary-agent") || strings.Contains(kv.Value.String(), "203.0.113.9") {
						t.Errorf("SERVER span %q attribute %s carries the caller's %q", s.Name(), kv.Key, kv.Value.String())
					}
				}
			}
			if len(servers) != served {
				t.Errorf("%d traces carry a SERVER span, want one per request, %d", len(servers), served)
			}
			for id, n := range servers {
				if n != 1 {
					t.Errorf("trace %s carries %d SERVER spans", id, n)
				}
			}
		})
	}
}

// TestHandMadeSpansNestUnderTheRequestSpan: a request that carries a W3C
// parent gets one SERVER span, the listener's, as that parent's child,
// and lux.request on a door and lux.api on the control plane are
// INTERNAL children of it with their attributes intact.
func TestHandMadeSpansNestUnderTheRequestSpan(t *testing.T) {
	mem := installInMemory(t)
	srv := startServe(t, nil)
	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	parent := "00-" + traceID + "-" + spanID + "-01"
	do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model":"m","messages":[]}`, "traceparent", parent)
	do(t, http.MethodGet, srv.publicURL+"/v1/self", "", "traceparent", parent)
	if code := srv.stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}

	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	var servers, hand []sdktrace.ReadOnlySpan
	for _, s := range mem.spans.Ended() {
		if s.SpanContext().TraceID().String() != traceID {
			continue
		}
		byID[s.SpanContext().SpanID()] = s
		switch {
		case s.SpanKind() == trace.SpanKindServer:
			servers = append(servers, s)
		case s.Name() == "lux.request" || s.Name() == "lux.api":
			hand = append(hand, s)
		}
	}
	if len(servers) != 2 {
		t.Fatalf("%d SERVER spans under the propagated parent, want 2, one per request", len(servers))
	}
	for _, s := range servers {
		if s.Parent().SpanID().String() != spanID || !s.Parent().IsRemote() {
			t.Errorf("SERVER span %q has parent %s, want the caller's %s", s.Name(), s.Parent().SpanID(), spanID)
		}
	}
	if len(hand) != 2 {
		t.Fatalf("%d lux.request and lux.api spans under the propagated parent, want 2", len(hand))
	}
	for _, s := range hand {
		p, ok := byID[s.Parent().SpanID()]
		if !ok || p.SpanKind() != trace.SpanKindServer {
			t.Errorf("%s is not a child of the request's SERVER span", s.Name())
		}
		if s.SpanKind() != trace.SpanKindInternal {
			t.Errorf("%s is a %s span, want internal", s.Name(), s.SpanKind())
		}
		keys := map[string]bool{}
		for _, kv := range s.Attributes() {
			keys[string(kv.Key)] = true
		}
		if !keys["lux.route"] || !keys["lux.status"] || !keys["lux.request_id"] {
			t.Errorf("%s lost its attributes: %v", s.Name(), s.Attributes())
		}
	}
}

// TestObservePublicHandsTheListenerTheCaller: the listener behind the
// handler reads the peer address, the User-Agent, and X-Forwarded-For as
// they arrived, which the per-address bucket and the authorizer's
// request.ip depend on, while the SERVER span carries none of them; the
// request the server passed in keeps its header.
func TestObservePublicHandsTheListenerTheCaller(t *testing.T) {
	mem := installInMemory(t)
	var remote, agent, forwarded string
	public := http.NewServeMux()
	public.HandleFunc("/openai/", func(w http.ResponseWriter, r *http.Request) {
		remote, agent, forwarded = r.RemoteAddr, r.UserAgent(), r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusNoContent)
	})
	h := observePublic(public, listenerRoutes{mounted: public, public: public})
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	r.RemoteAddr = "192.0.2.7:4711"
	r.Header.Set("User-Agent", "canary-agent/1.0")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	if remote != "192.0.2.7:4711" || agent != "canary-agent/1.0" || forwarded != "203.0.113.9" {
		t.Errorf("the listener read RemoteAddr %q, User-Agent %q, X-Forwarded-For %q", remote, agent, forwarded)
	}
	if r.UserAgent() != "canary-agent/1.0" || r.Header.Get("X-Forwarded-For") != "203.0.113.9" {
		t.Errorf("the server's request lost its header: %v", r.Header)
	}
	spans := mem.spans.Ended()
	if len(spans) != 1 || spans[0].SpanKind() != trace.SpanKindServer || spans[0].Name() != "POST /openai/v1/chat/completions" {
		t.Fatalf("spans %v", spans)
	}
	for _, kv := range spans[0].Attributes() {
		if slices.Contains(callerKeys, string(kv.Key)) {
			t.Errorf("the SERVER span carries %s", kv.Key)
		}
		if kv.Key == "http.route" && kv.Value.AsString() != "/openai/v1/chat/completions" {
			t.Errorf("http.route %q", kv.Value.AsString())
		}
	}
	if got := mem.durationCounts(t); !maps.Equal(got, map[string]uint64{"/openai/v1/chat/completions": 1}) {
		t.Errorf("duration counts %v", got)
	}
}

// TestCanonicalIsTheMuxCleaning: a path the mux would redirect names no
// route, and one it serves as it is does.
func TestCanonicalIsTheMuxCleaning(t *testing.T) {
	for p, want := range map[string]bool{
		"/":                            true,
		"/openai/v1/chat/completions":  true,
		"/v1/":                         true,
		"/openai//v1/chat/completions": false,
		"/v1/keys/../self":             false,
		"/v1/./keys":                   false,
		"/v1//":                        false,
	} {
		if got := canonical(p); got != want {
			t.Errorf("canonical(%q) = %v, want %v", p, got, want)
		}
	}
}
