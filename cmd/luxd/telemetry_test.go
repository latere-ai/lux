// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"
)

// The e2e tier of spec 019 at the process: one serve run in server mode
// with a stub issuer, a stub authorizer, a stub provider on loopback,
// and a stub OTLP collector, driven through both planes with canaries
// in every place a secret could leak from, then stopped so every
// exporter flushes. The four tests of the spec's table read what the run
// produced: the log on stderr, the /metrics text, and the spans the
// collector received. The run is made once per test binary.

// The canaries: a Provider credential, a prompt, and a completion; the
// Key value is minted by the server and read from the apply's answer.
const (
	canaryCredential = "sk-canary-credential-0f3a9c-never-in-telemetry"
	canaryPrompt     = "canary prompt 7f3c9a: the quick brown fox"
	canaryCompletion = "canary completion 91c0d2: jumps over the lazy dog"
	hostileString    = "x\ny\x1b[31mz\r\n{\"level\":\"ERROR\"}"
)

// collector is the stub OTLP receiver: it keeps every body by signal.
type collector struct {
	*httptest.Server
	mu     sync.Mutex
	bodies map[string][][]byte // by path: /v1/traces, /v1/logs, /v1/metrics
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{bodies: map[string][][]byte{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rd io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			rd = gz
		}
		body, _ := io.ReadAll(rd)
		c.mu.Lock()
		c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], body)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *collector) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][]byte
	for _, bodies := range c.bodies {
		out = append(out, bodies...)
	}
	return out
}

func (c *collector) spans(t *testing.T) []*tracepb.Span {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*tracepb.Span
	for _, body := range c.bodies["/v1/traces"] {
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			t.Fatalf("a trace export did not decode: %v", err)
		}
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				out = append(out, ss.GetSpans()...)
			}
		}
	}
	return out
}

// telemetryRun is what one run produced.
type telemetryRun struct {
	stderr     string
	metrics    string
	exported   [][]byte // every OTLP body, raw
	spans      []*tracepb.Span
	keyValue   string
	keyID      string
	keyPrefix  string
	subject    string
	issuerURL  string
	requestIDs []string
}

var (
	runOnce sync.Once
	theRun  *telemetryRun
	runErr  string
)

// telemetry runs the e2e once and returns it, failing every caller when
// the run did not complete.
func telemetry(t *testing.T) *telemetryRun {
	t.Helper()
	runOnce.Do(func() { theRun, runErr = runTelemetry(t) })
	if runErr != "" {
		t.Fatalf("the telemetry run did not complete: %s", runErr)
	}
	return theRun
}

func runTelemetry(t *testing.T) (*telemetryRun, string) {
	t.Helper()
	coll := newCollector(t)
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	authorizer := stub.New(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // a measurable generation time
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"`+canaryCompletion+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":30}}`)
	}))
	t.Cleanup(provider.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", coll.URL)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("POD_NAME", "luxd-0")

	srv := startServe(t, map[string]string{
		"LUX_OIDC_ISSUERS":           iss.URL(),
		"LUX_AUTHORIZER_URL":         authorizer.URL(),
		"LUX_AUTHORIZER_TOKEN":       authorizer.Token(),
		"LUX_UPSTREAM_ALLOW_PRIVATE": "1",
		// The health job reads the catalog on its tick, so a Provider
		// applied after the start carries a lux_provider_health series
		// from the next one; the floor of the interval is what the scrape
		// below waits out.
		"LUX_HEALTH_INTERVAL": "5s",
	})
	run := &telemetryRun{issuerURL: iss.URL(), subject: iss.URL() + "|alice"}
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}
	note := func(resp *http.Response) { run.requestIDs = append(run.requestIDs, resp.Header.Get("Lux-Request-Id")) }

	resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/providers/oai", `{"spec": {"dialect": "openai", "baseURL": "`+provider.URL+`/v1", "credential": {"value": "`+canaryCredential+`"}, "discovery": {"mode": "none"}, "health": {"mode": "none"}}}`, bearer...)
	note(resp)
	if resp.StatusCode != http.StatusCreated {
		return nil, "provider apply: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	resp, body = do(t, http.MethodPut, srv.publicURL+"/v1/models/gpt", `{"spec": {"targets": [{"provider": "oai", "model": "gpt-4.1"}], "pricing": {"currency": "USD", "input": "1", "output": "2"}}}`, bearer...)
	note(resp)
	if resp.StatusCode != http.StatusCreated {
		return nil, "model apply: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	resp, body = do(t, http.MethodPut, srv.publicURL+"/v1/keys/k", `{"spec": {"models": ["*"]}}`, bearer...)
	note(resp)
	if resp.StatusCode != http.StatusCreated {
		return nil, "key apply: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	var applied struct {
		Status struct {
			ID, Prefix, Value string
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &applied); err != nil || applied.Status.Value == "" {
		return nil, "key apply answered no value: " + body
	}
	run.keyValue, run.keyID, run.keyPrefix = applied.Status.Value, applied.Status.ID, applied.Status.Prefix

	resp, body = do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model": "gpt", "messages": [{"role": "user", "content": "`+canaryPrompt+`"}]}`, "Authorization", "Bearer "+run.keyValue)
	note(resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, canaryCompletion) {
		return nil, "chat: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	hostile, _ := json.Marshal(hostileString)
	resp, body = do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model": `+string(hostile)+`, "messages": [{"role": "user", "content": "hi"}]}`, "Authorization", "Bearer "+run.keyValue)
	note(resp)
	if resp.StatusCode != http.StatusNotFound {
		return nil, "hostile model: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	resp, body = do(t, http.MethodGet, srv.publicURL+"/v1/nothing", "", bearer...)
	note(resp)
	if resp.StatusCode != http.StatusNotFound {
		return nil, "not_found: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	resp, body = do(t, http.MethodPut, srv.publicURL+"/v1/budgets/x%0Ay%1B%5B31mz", `{"spec": {"amount": "50"}}`, bearer...)
	note(resp)
	if resp.StatusCode < 400 {
		return nil, "hostile name applied: " + strconv.Itoa(resp.StatusCode) + " " + body
	}
	// The scrape is taken once the health job has read the Provider the
	// run applied, which is its first tick after the apply and the last
	// series the metric table waits for.
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, body = do(t, http.MethodGet, srv.internalURL+"/metrics", "")
		if resp.StatusCode != http.StatusOK {
			return nil, "/metrics: " + strconv.Itoa(resp.StatusCode)
		}
		if strings.Contains(body, "lux_provider_health{") || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	run.metrics = body
	if code := srv.stop(); code != 0 {
		return nil, "serve exited " + strconv.Itoa(code)
	}
	run.stderr = srv.errOut.String()
	run.exported = coll.all()
	run.spans = coll.spans(t)
	if len(run.spans) == 0 {
		return nil, "the collector received no span"
	}
	return run, ""
}

// specTable reads spec 019's metric table: name to type, labels, and
// whether the owner's cell marks it not built.
type specMetric struct {
	typ      string
	labels   []string
	notBuilt bool
}

var backticked = regexp.MustCompile("`([^`]+)`")

func specTable(t *testing.T) map[string]specMetric {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "specs", "019-observability.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "| Metric | Type | Labels | Owner |")
	if !ok {
		t.Fatal("no metric table in the spec")
	}
	out := map[string]specMetric{}
	for _, line := range strings.Split(rest, "\n")[1:] {
		if !strings.HasPrefix(line, "|") {
			if len(out) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 4 {
			t.Fatalf("a metric row with %d cells: %s", len(cells), line)
		}
		m := specMetric{typ: strings.TrimSpace(cells[1]), notBuilt: strings.Contains(cells[3], "not built")}
		if strings.TrimSpace(cells[2]) != "none" {
			for _, l := range backticked.FindAllStringSubmatch(cells[2], -1) {
				m.labels = append(m.labels, l[1])
			}
		}
		slices.Sort(m.labels)
		out[strings.Trim(strings.TrimSpace(cells[0]), "`")] = m
	}
	return out
}

// exposition is a parsed /metrics text: each family's type and the
// label sets of its series, the histogram's suffixes and le folded into
// the family.
type exposition struct {
	types  map[string]string
	series map[string][][]string
}

func parseExposition(text string) exposition {
	e := exposition{types: map[string]string{}, series: map[string][][]string{}}
	for line := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			e.types[name] = typ
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var name string
		var labels []string
		if open := strings.IndexByte(line, '{'); open >= 0 {
			name = line[:open]
			for pair := range strings.SplitSeq(line[open+1:strings.LastIndexByte(line, '}')], ",") {
				k, _, _ := strings.Cut(pair, "=")
				if k != "le" {
					labels = append(labels, k)
				}
			}
		} else {
			name, _, _ = strings.Cut(line, " ")
		}
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if family := strings.TrimSuffix(name, suffix); family != name && e.types[family] == "histogram" {
				name = family
				break
			}
		}
		slices.Sort(labels)
		e.series[name] = append(e.series[name], labels)
	}
	return e
}

// TestMetricsTable is spec 019's first row: the registry the process
// wires holds exactly the metrics of the spec's table, each with its type
// and, on every series, exactly the labels of its row; a row the
// registry does not carry is tolerated only where the spec marks it
// another spec's and not built.
func TestMetricsTable(t *testing.T) {
	run := telemetry(t)
	table := specTable(t)
	got := parseExposition(run.metrics)
	for name, typ := range got.types {
		row, ok := table[name]
		if !ok {
			t.Errorf("%s is in the registry and not in the table", name)
			continue
		}
		if row.typ != typ {
			t.Errorf("%s is a %s in the registry and a %s in the table", name, typ, row.typ)
		}
	}
	for name, row := range table {
		if _, ok := got.types[name]; !ok {
			if !row.notBuilt {
				t.Errorf("%s is in the table and not in the registry, and the spec does not mark it not built", name)
			}
			continue
		}
		if row.notBuilt {
			t.Errorf("%s is in the registry while the spec marks it not built", name)
		}
		if len(got.series[name]) == 0 {
			t.Errorf("%s has no series after the run, so its labels cannot be read", name)
		}
		for _, labels := range got.series[name] {
			if !slices.Equal(labels, row.labels) {
				t.Errorf("%s carries labels %v, the table %v", name, labels, row.labels)
			}
		}
	}
}

// TestTelemetryCarriesNoSecrets is spec 019's canary row: no metric, no
// span, no exported record, and no log line of the run carries the Key
// value, the Provider credential, the prompt, or the completion.
func TestTelemetryCarriesNoSecrets(t *testing.T) {
	run := telemetry(t)
	canaries := map[string]string{"Key value": run.keyValue, "credential": canaryCredential, "prompt": canaryPrompt, "completion": canaryCompletion}
	surfaces := map[string][]byte{"the log": []byte(run.stderr), "/metrics": []byte(run.metrics)}
	for i, body := range run.exported {
		surfaces["OTLP export "+strconv.Itoa(i)] = body
	}
	for surface, text := range surfaces {
		for what, canary := range canaries {
			if strings.Contains(string(text), canary) {
				t.Errorf("%s carries the %s", surface, what)
			}
		}
	}
	if len(run.exported) < 2 || !strings.Contains(run.stderr, `"msg":"lux.request"`) {
		t.Fatalf("the run exported %d bodies and logged:\n%s", len(run.exported), run.stderr)
	}
}

// The attribute sets of spec 019's span table, by span name; a span not
// in the table is the shared library's and carries http.* attributes.
var spanTable = map[string][]string{
	"lux.request":    {"lux.door", "lux.route", "lux.model", "lux.provider", "lux.status", "lux.code", "lux.request_id", "lux.stream", "lux.translated"},
	"lux.upstream":   {"lux.provider", "lux.attempt", "http.request.method", "url.template", "http.response.status_code", "lux.ttfb_ms"},
	"lux.api":        {"lux.route", "lux.action", "lux.kind", "lux.status", "lux.code", "lux.request_id"},
	"lux.authorizer": {"lux.action", "lux.decision"},
	"lux.store":      {"lux.op", "lux.kind", "lux.result"},
}

// identityKeys are the shapes a leak of identity would take on a span.
var identityKeys = []string{"lux.subject", "lux.owner", "lux.key_id", "lux.key_prefix", "lux.key", "subject", "owner", "key_id", "key_prefix",
	"client.address", "client.port", "network.peer.address", "network.peer.port", "net.sock.peer.addr", "net.peer.ip", "http.client_ip", "enduser.id", "user_agent.original"}

func attributeString(v interface{ GetStringValue() string }) string { return v.GetStringValue() }

// TestSpansCarryNoIdentity is spec 019's row over every span the run
// exported: no span carries a subject, owner, Key id, Key prefix, Key
// value, or caller address as a key or a value, every span of the table
// carries a subset of its row's attributes and no other, and the spans
// of the table are all present.
func TestSpansCarryNoIdentity(t *testing.T) {
	run := telemetry(t)
	// The issuer's URL alone is configuration, and the verifier's client
	// span names it in url.full; the subject, which is the issuer and the
	// sub joined, is identity and is forbidden with the sub itself.
	forbidden := map[string]string{"subject": run.subject, "Key id": run.keyID, "Key prefix": run.keyPrefix, "Key value": run.keyValue, "sub": "alice"}
	seen := map[string]int{}
	for _, s := range run.spans {
		seen[s.GetName()]++
		var keys []string
		for _, kv := range s.GetAttributes() {
			key := kv.GetKey()
			keys = append(keys, key)
			if slices.Contains(identityKeys, key) {
				t.Errorf("span %s carries %s", s.GetName(), key)
			}
			value := attributeString(kv.GetValue())
			for what, f := range forbidden {
				if f != "" && strings.Contains(value, f) {
					t.Errorf("span %s attribute %s carries the %s", s.GetName(), key, what)
				}
			}
		}
		if row, ok := spanTable[s.GetName()]; ok {
			for _, k := range keys {
				if !slices.Contains(row, k) {
					t.Errorf("span %s carries %s, which is not in its row", s.GetName(), k)
				}
			}
		}
	}
	for name := range spanTable {
		if seen[name] == 0 {
			t.Errorf("no %s span was exported; spans by name: %v", name, seen)
		}
	}
}

// The base fields the shared library puts on every record, and the two
// field rows of spec 019's log table.
var (
	baseFields    = []string{"service", "version", "replica"}
	joinFields    = []string{"trace_id", "span_id"}
	encoderFields = []string{"time", "level", "msg"}
	dataFields    = []string{"request_id", "door", "route", "model", "provider", "status", "code", "key_prefix", "owner", "duration_ms", "ttfb_ms", "input_tokens", "output_tokens", "stream"}
	controlFields = []string{"request_id", "route", "action", "kind", "name", "status", "code", "subject", "duration_ms"}
)

// TestLogFieldsAreTheTable is spec 019's row over every log line of the
// run: each is one JSON object carrying the base fields, each request
// line carries the trace join and exactly the fields of its plane's row
// and no other, and the hostile model string and object name produced
// one escaped line each with no raw control byte in the log.
func TestLogFieldsAreTheTable(t *testing.T) {
	run := telemetry(t)
	var data, control []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(run.stderr), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a log line is not one JSON object: %v\n%s", err, line)
		}
		for _, f := range baseFields {
			if v, _ := m[f].(string); v == "" {
				t.Errorf("a line lacks %s: %s", f, line)
			}
		}
		if m["service"] != "luxd" || m["replica"] != "luxd-0" {
			t.Errorf("service %v replica %v", m["service"], m["replica"])
		}
		switch m["msg"] {
		case "lux.request":
			data = append(data, m)
		case "lux.api":
			control = append(control, m)
		}
	}
	if len(data) != 2 || len(control) != 5 {
		t.Fatalf("%d data and %d control lines:\n%s", len(data), len(control), run.stderr)
	}
	check := func(lines []map[string]any, row []string) {
		t.Helper()
		for _, m := range lines {
			fields := slices.Sorted(maps.Keys(m))
			fields = slices.DeleteFunc(fields, func(k string) bool {
				return slices.Contains(encoderFields, k) || slices.Contains(baseFields, k) || slices.Contains(joinFields, k)
			})
			if !slices.Equal(fields, slices.Sorted(slices.Values(row))) {
				t.Errorf("fields %v, want %v", fields, row)
			}
			for _, f := range joinFields {
				if v, _ := m[f].(string); v == "" {
					t.Errorf("a request line lacks %s: %v", f, m)
				}
			}
			if m["level"] != "INFO" || !slices.Contains(run.requestIDs, m["request_id"].(string)) {
				t.Errorf("level %v request_id %v", m["level"], m["request_id"])
			}
		}
	}
	check(data, dataFields)
	check(control, controlFields)
	if data[0]["model"] != "gpt" || data[0]["provider"] != "oai" || data[0]["status"] != "ok" || data[0]["key_prefix"] != run.keyPrefix || data[0]["owner"] != run.subject || data[0]["output_tokens"] != float64(30) {
		t.Errorf("the served line: %v", data[0])
	}
	if data[1]["model"] != "" || data[1]["code"] != "model_not_found" || data[1]["status"] != "refused" {
		t.Errorf("the hostile model's line: %v", data[1])
	}
	if control[0]["route"] != "/v1/providers/{name}" || control[0]["action"] != "provider.create" || control[0]["kind"] != "Provider" || control[0]["name"] != "oai" || control[0]["subject"] != run.subject {
		t.Errorf("the provider apply's line: %v", control[0])
	}
	if control[4]["name"] != hostileString[:strings.Index(hostileString, "\r")] || control[4]["status"] != "refused" {
		t.Errorf("the hostile name's line: %v", control[4])
	}
	if strings.Contains(run.stderr, "\x1b") || strings.Contains(run.stderr, "\r") {
		t.Error("a raw control byte reached the log")
	}
}

// TestMetricsContentType is spec 019's last row with the scaffold's
// probe test: /metrics is served on the internal listener in the
// Prometheus text format with the version=0.0.4 content type, and
// answered 404 on the public one.
func TestMetricsContentType(t *testing.T) {
	srv := startServe(t, nil)
	defer srv.stop()
	resp, body := do(t, http.MethodGet, srv.internalURL+"/metrics", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("internal /metrics: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "# TYPE lux_requests_total counter") {
		t.Errorf("the exposition lacks the request counter:\n%s", body)
	}
	if resp, _ := do(t, http.MethodGet, srv.publicURL+"/metrics", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("public /metrics: %d", resp.StatusCode)
	}
}
