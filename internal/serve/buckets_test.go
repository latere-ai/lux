// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// specPath is spec 019 beside this package, the file the two tests
// below read.
const specPath = "../../specs/019-observability.md"

// bucketRows reads the buckets table of spec 019: each row's metric
// names to the boundaries the row gives, or to the shared default when
// it names that.
func bucketRows(t *testing.T) map[string][]float64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(data), "| Histogram | Buckets, seconds |")
	if !ok {
		t.Fatal("no buckets table in the spec")
	}
	code := regexp.MustCompile("`([^`]+)`")
	out := map[string][]float64{}
	for line := range strings.SplitSeq(table, "\n") {
		if !strings.HasPrefix(line, "| `lux_") {
			if len(out) > 0 && !strings.HasPrefix(line, "|") {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("a row with %d cells: %s", len(cells), line)
		}
		var bounds []float64
		if strings.Contains(cells[1], "DefaultDurationBuckets") {
			bounds = metrics.DefaultDurationBuckets
		} else {
			for _, m := range code.FindAllStringSubmatch(cells[1], -1) {
				for part := range strings.SplitSeq(m[1], ",") {
					f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
					if err != nil {
						t.Fatalf("%q in %s: %v", part, line, err)
					}
					bounds = append(bounds, f)
				}
			}
		}
		for _, m := range code.FindAllStringSubmatch(cells[0], -1) {
			out[m[1]] = bounds
		}
	}
	return out
}

// TestHistogramBuckets is spec 019's row: each histogram of the buckets
// table is registered with exactly the boundaries in its row, read from
// the spec beside the Go values the code passes, and a ten minute
// stream lands in the 600 second bucket and not in +Inf alone.
func TestHistogramBuckets(t *testing.T) {
	rows := bucketRows(t)
	code := map[string][]float64{
		gateway.MetricRequestDuration:  gateway.DurationBuckets,
		gateway.MetricUpstreamDuration: gateway.DurationBuckets,
		gateway.MetricTimeToFirstByte:  gateway.TimeToFirstByteBuckets,
		MetricOutputTokensPerSecond:    OutputTokensPerSecondBuckets,
		MetricAuthorizerDuration:       metrics.DefaultDurationBuckets,
	}
	if len(rows) != len(code) {
		t.Fatalf("the table has %d histograms, the code %d: %v", len(rows), len(code), slices.Sorted(mapsKeys(rows)))
	}
	for name, want := range rows {
		if got, ok := code[name]; !ok || !slices.Equal(got, want) {
			t.Errorf("%s: code %v, table %v", name, got, want)
		}
	}
	reg := metrics.NewRegistry()
	r := NewRecorder(RecorderOptions{Store: memory.New(), Metrics: reg})
	r.tps.Observe(map[string]string{"provider": "p", "model": "m"}, 42)
	reg.Histogram(gateway.MetricRequestDuration, "", gateway.DurationBuckets).Observe(map[string]string{"door": "openai", "model": "m", "status": "ok"}, (10 * time.Minute).Seconds())
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	text := buf.String()
	for _, want := range []string{
		gateway.MetricRequestDuration + `_bucket{door="openai",le="300",model="m",status="ok"} 0`,
		gateway.MetricRequestDuration + `_bucket{door="openai",le="600",model="m",status="ok"} 1`,
		gateway.MetricRequestDuration + `_bucket{door="openai",le="+Inf",model="m",status="ok"} 1`,
		MetricOutputTokensPerSecond + `_bucket{le="20",model="m",provider="p"} 0`,
		MetricOutputTokensPerSecond + `_bucket{le="50",model="m",provider="p"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition lacks %s:\n%s", want, text)
		}
	}
}

func mapsKeys(m map[string][]float64) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// TestOutputTokensPerSecond is spec 019's placing of the fourth metric
// of spec 009's row: a served stream observes its output tokens over the
// time from its first byte to its last, a served non-stream over its
// answering attempt's duration, by provider and model; a refusal, a
// failure, a record with no output, and a time too short to measure
// observe nothing.
func TestOutputTokensPerSecond(t *testing.T) {
	reg := metrics.NewRegistry()
	r := NewRecorder(RecorderOptions{Store: memory.New(), Metrics: reg})
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	base := gateway.Record{ID: "req_1", At: at, KeyID: "key_1", Model: "gpt", Provider: "oai", Door: v1.DialectOpenAI, Route: "/openai/v1/chat/completions", Status: gateway.StatusOK}
	stream := base
	stream.Stream, stream.TTFB, stream.Latency, stream.EndedAt = true, time.Second, 3*time.Second, at.Add(3*time.Second)
	stream.Tokens = gateway.Tokens{Input: 10, Output: 200}
	stream.Attempts = []gateway.Attempt{{Provider: "oai", Status: gateway.StatusOK, HTTPStatus: 200, Duration: 3 * time.Second}}
	r.Record(stream) // 200 tokens over 2 seconds: 100 a second

	whole := base
	whole.ID, whole.Latency, whole.EndedAt = "req_2", 2500*time.Millisecond, at.Add(2500*time.Millisecond)
	whole.Tokens = gateway.Tokens{Input: 10, Output: 50}
	whole.Attempts = []gateway.Attempt{{Provider: "ant", Status: gateway.StatusFailed, Error: gateway.CodeUpstreamError, Duration: 500 * time.Millisecond}, {Provider: "oai", Status: gateway.StatusOK, HTTPStatus: 200, Duration: 2 * time.Second}}
	r.Record(whole) // 50 tokens over the answering attempt's 2 seconds: 25 a second

	for i, rec := range []gateway.Record{
		func() gateway.Record {
			x := whole
			x.Status, x.Error = gateway.StatusRefused, gateway.CodeModelNotFound
			x.Model = ""
			return x
		}(),
		func() gateway.Record {
			x := whole
			x.Status, x.Error = gateway.StatusFailed, gateway.CodeUpstreamError
			return x
		}(),
		func() gateway.Record { x := whole; x.Tokens.Output = 0; return x }(),
		func() gateway.Record { x := whole; x.Attempts = nil; return x }(),
		func() gateway.Record { x := stream; x.Latency = x.TTFB; return x }(),
	} {
		rec.ID = "req_none_" + strconv.Itoa(i)
		r.Record(rec)
	}
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	text := buf.String()
	if got := r.tps.Count(map[string]string{"provider": "oai", "model": "gpt"}); got != 2 {
		t.Errorf("%d observations, want 2:\n%s", got, text)
	}
	for _, want := range []string{
		MetricOutputTokensPerSecond + `_bucket{le="20",model="gpt",provider="oai"} 0`,
		MetricOutputTokensPerSecond + `_bucket{le="50",model="gpt",provider="oai"} 1`,
		MetricOutputTokensPerSecond + `_bucket{le="100",model="gpt",provider="oai"} 2`,
		MetricOutputTokensPerSecond + `_sum{model="gpt",provider="oai"} 125`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the exposition lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, `model="",provider="oai"`) {
		t.Error("a refusal observed a rate")
	}
}
