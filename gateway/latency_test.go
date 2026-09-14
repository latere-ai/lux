// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file measures the latency distribution and throughput the gateway
// adds per request, not just the mean a benchmark reports. It drives the
// in-process handler at a fixed concurrency against the harness's stub
// providers, so it isolates the gateway's overhead from a real provider
// and the network exactly as the -bench benchmarks do; the numbers are the
// added cost, and real end-to-end p95 is dominated by the provider, which
// this excludes (spec 023).
//
// TestGatewayAddedLatency is opt-in: it runs a small sweep by default, so
// the harness code is exercised without a perf assertion in the gate, and
// a full sweep printing the distribution when LUX_LATENCY is set. It never
// fails on a latency threshold; benchmarks are not assertions.

// distribution is one route class's measured added cost.
type distribution struct {
	name        string
	count       int
	concurrency int
	elapsed     time.Duration
	p50, p75    time.Duration
	p90, p95    time.Duration
	p99         time.Duration
	throughput  float64 // requests per second
	bytesPerReq uint64  // bytes allocated per request over the sweep
}

// percentile returns the p-th percentile of sorted by the nearest-rank
// method, p in (0, 100]. sorted must be sorted ascending and non-empty.
func percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// drive runs total requests through h at the given concurrency, each with a
// fresh request from mk and a fresh recorder, and returns every request's
// latency. Distinct workers write distinct slice indices, so no lock is on
// the timed path.
func drive(tb testing.TB, h *Handler, mk func() *http.Request, concurrency, total int) []time.Duration {
	tb.Helper()
	lat := make([]time.Duration, total)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= total {
					return
				}
				r := mk()
				rec := httptest.NewRecorder()
				start := time.Now()
				h.ServeHTTP(rec, r)
				lat[i] = time.Since(start)
				if rec.Code != http.StatusOK {
					tb.Errorf("%s: status %d", r.URL.Path, rec.Code)
					return
				}
			}
		})
	}
	wg.Wait()
	return lat
}

// measure drives one route class and summarises it: the percentile set the
// maintainer named, throughput, and bytes allocated per request over the
// whole sweep.
func measure(tb testing.TB, name string, h *Handler, mk func() *http.Request, concurrency, total int) distribution {
	tb.Helper()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()
	lat := drive(tb, h, mk, concurrency, total)
	elapsed := time.Since(start)
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	slices.Sort(lat)
	d := distribution{
		name: name, count: total, concurrency: concurrency, elapsed: elapsed,
		p50: percentile(lat, 50), p75: percentile(lat, 75), p90: percentile(lat, 90),
		p95: percentile(lat, 95), p99: percentile(lat, 99),
	}
	if elapsed > 0 {
		d.throughput = float64(total) / elapsed.Seconds()
	}
	if total > 0 {
		d.bytesPerReq = (m1.TotalAlloc - m0.TotalAlloc) / uint64(total)
	}
	return d
}

// line is one row of the report the opt-in run prints.
func (d distribution) line() string {
	return fmt.Sprintf("%-12s n=%-6d conc=%-2d p50=%-10v p75=%-10v p90=%-10v p95=%-10v p99=%-10v %8.0f req/s %6d B/req",
		d.name, d.count, d.concurrency, d.p50, d.p75, d.p90, d.p95, d.p99, d.throughput, d.bytesPerReq)
}

// benchClasses configures a world's stubs for the three route classes and
// returns a request factory per class: a same-dialect passthrough, a
// translated request, and a token count the gateway answers itself.
func benchClasses(tb testing.TB) (*world, map[string]func() *http.Request) {
	w := newWorld(tb)
	w.openai.respondJSON(http.StatusOK, openaiChatResponse)
	w.anthropic.respondJSON(http.StatusOK, anthropicResponse)
	mk := func(method, path, body string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+keyValue)
			r.Header.Set("Content-Type", "application/json")
			return r
		}
	}
	return w, map[string]func() *http.Request{
		"passthrough": mk(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt-4.1", false)),
		"translated":  mk(http.MethodPost, "/openai/v1/chat/completions", chatBody("claude", false)),
		"count":       mk(http.MethodPost, "/anthropic/v1/messages/count_tokens", messagesBody("gpt", false)),
	}
}

// TestPercentileNearestRank holds the percentile helper to known values, so
// the distribution math is checked without a machine-dependent measurement.
func TestPercentileNearestRank(t *testing.T) {
	sorted := make([]time.Duration, 100)
	for i := range sorted {
		sorted[i] = time.Duration(i+1) * time.Millisecond
	}
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{
		{50, 50 * time.Millisecond}, {75, 75 * time.Millisecond}, {90, 90 * time.Millisecond},
		{95, 95 * time.Millisecond}, {99, 99 * time.Millisecond}, {100, 100 * time.Millisecond},
		{0, 1 * time.Millisecond},
	} {
		if got := percentile(sorted, c.p); got != c.want {
			t.Errorf("p%v = %v, want %v", c.p, got, c.want)
		}
	}
	if percentile(nil, 95) != 0 {
		t.Error("percentile of an empty sample is not zero")
	}
}

// TestLatencySummary drives a small sweep of each route class, so the
// harness runs in the gate without a perf assertion, and checks the summary
// is well formed: every request measured, the percentiles ordered, and
// throughput positive.
func TestLatencySummary(t *testing.T) {
	w, classes := benchClasses(t)
	for _, name := range []string{"passthrough", "translated", "count"} {
		d := measure(t, name, w.h, classes[name], 4, 60)
		if d.count != 60 || d.p50 <= 0 || d.throughput <= 0 {
			t.Errorf("%s: %+v", name, d)
		}
		if d.p50 > d.p95 || d.p95 > d.p99 {
			t.Errorf("%s: percentiles out of order: %s", name, d.line())
		}
	}
}

// TestGatewayAddedLatency prints the added-latency distribution and
// throughput at a fixed concurrency for the three route classes, with the
// translation overhead called out as the translated-minus-passthrough
// delta. It is opt-in: set LUX_LATENCY to run the full sweep.
//
//	LUX_LATENCY=1 go test -run TestGatewayAddedLatency -v ./gateway/
func TestGatewayAddedLatency(t *testing.T) {
	if os.Getenv("LUX_LATENCY") == "" {
		t.Skip("set LUX_LATENCY to run the full latency and throughput sweep")
	}
	const (
		concurrency = 8
		total       = 20000
	)
	w, classes := benchClasses(t)
	dist := map[string]distribution{}
	for _, name := range []string{"passthrough", "translated", "count"} {
		d := measure(t, name, w.h, classes[name], concurrency, total)
		dist[name] = d
		t.Log(d.line())
	}
	delta := dist["translated"].p50 - dist["passthrough"].p50
	t.Logf("translation overhead (translated p50 - passthrough p50) = %v", delta)
}
