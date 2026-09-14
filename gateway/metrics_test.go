// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
)

// scrape renders a registry in the text format.
func scrape(reg *metrics.Registry) string {
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	return buf.String()
}

// series is every label set of one series name in an exposition,
// lux_requests_total or lux_request_duration_seconds_count for one.
func series(text, name string) []map[string]string {
	var out []map[string]string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		labels := map[string]string{}
		if open := strings.IndexByte(line, '{'); open >= 0 {
			body := line[open+1 : strings.LastIndexByte(line, '}')]
			for pair := range strings.SplitSeq(body, ",") {
				k, v, _ := strings.Cut(pair, "=")
				labels[k], _ = strconv.Unquote(v)
			}
		}
		out = append(out, labels)
	}
	return out
}

// metered is a world whose handler records into a registry.
func metered(t *testing.T) (*world, *metrics.Registry) {
	t.Helper()
	w := newWorld(t)
	reg := metrics.NewRegistry()
	o := w.options()
	o.Metrics = reg
	w.h = New(o)
	return w, reg
}

// TestMetricLabelValues is spec 019's row, table-driven over every
// label this package writes: after a served, a refused, a failed, and a
// timed-out request, every label value in the exposition is from the
// closed set of its row, or is the name of a Model or Provider of the
// catalog, or is empty where the row allows it.
func TestMetricLabelValues(t *testing.T) {
	w, reg := metered(t)
	w.openai.respondJSON(200, openaiChatResponse)
	w.anthropic.respondJSON(503, `{"error":"down"}`)
	slow := w.provider("slow", v1.DialectLux, w.lux.URL+"/v1", luxCredential)
	slow.Spec.Timeout = "20ms"
	w.model("slowm", target("slow", "slowm"))
	w.lux.respond(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = rw.Write([]byte(luxResponse))
	})
	w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	w.post("/openai/v1/chat/completions", chatBody("nope", false))
	w.post("/anthropic/v1/messages", messagesBody("claude", false))
	w.post("/lux/v1/generate", luxBody("slowm", false))
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false))
	r.Header.Set("Authorization", "Bearer lux_nobody")
	w.do(r)

	var models, providers []string
	for name := range w.catalog.models {
		models = append(models, name)
	}
	for name, p := range w.catalog.providers {
		if name == p.Metadata.Name {
			providers = append(providers, name)
		}
	}
	closed := map[string]func(string) bool{
		"door":     func(v string) bool { return slices.Contains([]string{"openai", "anthropic", "gemini", "lux"}, v) },
		"model":    func(v string) bool { return v == "" || slices.Contains(models, v) },
		"provider": func(v string) bool { return v == "" || slices.Contains(providers, v) },
		"status":   func(v string) bool { return slices.Contains([]string{"ok", "refused", "failed"}, v) },
		"code": func(v string) bool {
			_, known := table[Code(v)]
			return v == "" || known
		},
		"le": func(string) bool { return true },
	}
	upstream := map[string]func(string) bool{
		"provider": closed["provider"],
		"status":   func(v string) bool { return slices.Contains([]string{UpstreamOK, UpstreamError, UpstreamTimeout}, v) },
		"le":       func(string) bool { return true },
	}
	text := scrape(reg)
	rows := []struct {
		name   string
		labels []string
		sets   map[string]func(string) bool
	}{
		{MetricRequests, []string{"door", "model", "provider", "status", "code"}, closed},
		{MetricRequestDuration + "_count", []string{"door", "model", "status"}, closed},
		{MetricTimeToFirstByte + "_count", []string{"door", "model"}, closed},
		{MetricUpstreamRequests, []string{"provider", "status"}, upstream},
		{MetricUpstreamDuration + "_count", []string{"provider"}, upstream},
	}
	for _, row := range rows {
		got := series(text, row.name)
		if len(got) == 0 {
			t.Errorf("%s has no series", row.name)
		}
		for _, labels := range got {
			keys := make([]string, 0, len(labels))
			for k, v := range labels {
				keys = append(keys, k)
				if !row.sets[k](v) {
					t.Errorf("%s: %s=%q is outside its closed set", row.name, k, v)
				}
			}
			slices.Sort(keys)
			if !slices.Equal(keys, slices.Sorted(slices.Values(row.labels))) {
				t.Errorf("%s: labels %v, want %v", row.name, keys, row.labels)
			}
		}
	}
	for _, want := range []string{
		`lux_requests_total{code="",door="openai",model="gpt",provider="oai",status="ok"} 1`,
		`lux_requests_total{code="model_not_found",door="openai",model="",provider="",status="refused"} 1`,
		`lux_requests_total{code="unauthenticated",door="openai",model="",provider="",status="refused"} 1`,
		`lux_requests_total{code="upstream_error",door="anthropic",model="claude",provider="ant",status="failed"} 1`,
		`lux_requests_total{code="upstream_timeout",door="lux",model="slowm",provider="slow",status="failed"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition lacks %s:\n%s", want, text)
		}
	}
}

// TestUpstreamMetrics is the two families of spec 005's row: one
// observation per target tried, ok for an answer the caller gets, error
// for the provider failing, timeout for its deadline, and the duration
// on the request's buckets, so a fallback is two observations.
func TestUpstreamMetrics(t *testing.T) {
	w, reg := metered(t)
	w.openai.respondJSON(500, `{"error":"down"}`)
	w.anthropic.respondJSON(200, anthropicResponse)
	w.gemini.respondJSON(400, `{"error":{"message":"bad"}}`)
	slow := w.provider("slow", v1.DialectLux, w.lux.URL+"/v1", luxCredential)
	slow.Spec.Timeout = "20ms"
	w.model("slowm", target("slow", "slowm"))
	w.lux.respond(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = rw.Write([]byte(luxResponse))
	})
	if rec := w.post("/openai/v1/chat/completions", chatBody("dual", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	w.post("/gemini/v1beta/models/gemini:generateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	w.post("/lux/v1/generate", luxBody("slowm", false))
	counter := reg.Counter(MetricUpstreamRequests, "")
	for _, tc := range []struct {
		provider, status string
		want             uint64
	}{{"oai", UpstreamError, 1}, {"ant", UpstreamOK, 1}, {"gem", UpstreamOK, 1}, {"slow", UpstreamTimeout, 1}, {"oai", UpstreamOK, 0}} {
		if got := counter.Value(map[string]string{"provider": tc.provider, "status": tc.status}); got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.provider, tc.status, got, tc.want)
		}
	}
	hist := reg.Histogram(MetricUpstreamDuration, "", nil)
	for _, p := range []string{"oai", "ant", "gem", "slow"} {
		if hist.Count(map[string]string{"provider": p}) != 1 {
			t.Errorf("%s duration observed %d times", p, hist.Count(map[string]string{"provider": p}))
		}
	}
	text := scrape(reg)
	if !strings.Contains(text, "# TYPE "+MetricUpstreamDuration+" histogram") || !strings.Contains(text, MetricUpstreamDuration+`_bucket{le="600",provider="oai"}`) {
		t.Errorf("the upstream duration is not on the request's buckets:\n%s", text)
	}
	// The opaque route is one attempt too, and its answer is the caller's
	// whatever the status, so a relayed 500 reads ok.
	r := w.request(http.MethodPost, "/openai/v1/files", `{}`)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set(HeaderProvider, "oai")
	if rec := w.do(r); rec.Code != 500 {
		t.Fatalf("opaque: %d %s", rec.Code, rec.Body.String())
	}
	if got := counter.Value(map[string]string{"provider": "oai", "status": UpstreamOK}); got != 1 {
		t.Errorf("the opaque attempt was not observed as ok: %d", got)
	}
}

// TestUnresolvedModelAddsNoSeries is spec 019's cardinality row: ten
// thousand requests naming ten thousand model strings that resolve to
// nothing add one series, the refusal's with an empty model, and one
// hundred requests to one Model add one more.
func TestUnresolvedModelAddsNoSeries(t *testing.T) {
	w, reg := metered(t)
	w.openai.respondJSON(200, openaiChatResponse)
	for i := range 10000 {
		if rec := w.post("/openai/v1/chat/completions", chatBody("invented-"+strconv.Itoa(i), false)); rec.Code != http.StatusNotFound {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	text := scrape(reg)
	if n := len(series(text, MetricRequests)); n != 1 {
		t.Fatalf("%d series after ten thousand invented names:\n%s", n, text[:min(len(text), 2000)])
	}
	if n := len(series(text, MetricRequestDuration+"_count")); n != 1 {
		t.Fatalf("%d duration series after ten thousand invented names", n)
	}
	for range 100 {
		w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	}
	text = scrape(reg)
	if n := len(series(text, MetricRequests)); n != 2 {
		t.Fatalf("%d series after one hundred requests to one Model", n)
	}
	if reg.Counter(MetricRequests, "").Value(map[string]string{"door": "openai", "model": "gpt", "provider": "oai", "status": "ok", "code": ""}) != 100 {
		t.Error("the one Model's series did not count the hundred")
	}
}

// TestCircuitAndTunnelGauges is spec 019's row. The circuit half: a
// target whose circuit opens sets lux_circuit_open to 1 for that
// provider and model, and a success once the open period has passed
// sets it back to 0. The tunnel half is spec 013's lux_tunnel_sessions,
// which that spec builds.
func TestCircuitAndTunnelGauges(t *testing.T) {
	t.Run("circuit", func(t *testing.T) {
		f := newRouterFixture(t)
		m := newModel("m", tw("a", "m", 100, 0))
		if _, err := f.r.Targets(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(scrape(f.reg), MetricCircuitOpen+`{model="m",provider="a"} 0`) {
			t.Fatalf("a closed circuit is not 0:\n%s", scrape(f.reg))
		}
		f.open("a", "m")
		if !strings.Contains(scrape(f.reg), MetricCircuitOpen+`{model="m",provider="a"} 1`) {
			t.Fatalf("an open circuit is not 1:\n%s", scrape(f.reg))
		}
		f.advance(CircuitOpen)
		if !f.r.Allow(f.target("a", "m")) {
			t.Fatal("no half-open probe")
		}
		f.r.RecordSuccess(f.target("a", "m"))
		if !strings.Contains(scrape(f.reg), MetricCircuitOpen+`{model="m",provider="a"} 0`) {
			t.Fatalf("a closed circuit is not back to 0:\n%s", scrape(f.reg))
		}
	})
	t.Run("tunnel", func(t *testing.T) {
		t.Skip("lux_tunnel_sessions is spec 013's, not built")
	})
}
