// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// Command driver is a closed-loop load generator for the proxy comparison
// under benchmarks/compare. It is standard-library only and deliberately
// excluded from the module's package set (the //go:build ignore tag keeps
// it out of go build ./..., go test ./..., and the coverage gate); run it
// with `go run driver.go <subcommand>`.
//
// Three subcommands:
//
//	run     drive one subject at a fixed concurrency for a fixed request
//	        count, after a discarded warmup, recording every request's
//	        wall latency and, optionally, the peak resident set of a pid
//	        tree sampled while the load runs. Appends one JSON line to -out,
//	        tagged with its -trial index so repeated trials aggregate.
//	report  read the JSON lines of a run file and print a Markdown table
//	        per scenario (request group x streaming mode), one row per
//	        subject.
//	csv     read the JSON lines of a run file and write tidy per-trial CSV
//	        (trial,shape,mode,subject,metric,value), the data the chart
//	        renderer reads and aggregates across trials.
//
// Nothing here fabricates a number: a subject that cannot be driven fails
// the run and writes no line, and the runner script records why.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sample is one subject-scenario measurement, the JSON line the run
// subcommand appends and the report subcommand reads.
type sample struct {
	Trial       int     `json:"trial"`        // 1-based measurement window; trials repeat the matrix
	Subject     string  `json:"subject"`      // baseline | luxd | litellm
	Group       string  `json:"group"`        // passthrough | translated
	Mode        string  `json:"mode"`         // nonstream | stream
	Shape       string  `json:"shape"`        // openai | anthropic (request wire shape)
	URL         string  `json:"url"`          // the endpoint driven
	Model       string  `json:"model"`        // the model named in the body
	Concurrency int     `json:"concurrency"`  // in-flight requests held constant
	Requests    int     `json:"requests"`     // measured requests (warmup excluded)
	Warmup      int     `json:"warmup"`       // discarded warmup requests
	Errors      int     `json:"errors"`       // measured requests that were not 2xx
	WallSeconds float64 `json:"wall_seconds"` // wall of the measured phase
	Throughput  float64 `json:"throughput_rps"`
	P50ms       float64 `json:"p50_ms"`
	P75ms       float64 `json:"p75_ms"`
	P90ms       float64 `json:"p90_ms"`
	P95ms       float64 `json:"p95_ms"`
	P99ms       float64 `json:"p99_ms"`
	PeakRSSMB   float64 `json:"peak_rss_mb"` // -1 when no pid was sampled (e.g. baseline)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: driver <run|report|csv> [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "report":
		os.Exit(reportCmd(os.Args[2:]))
	case "csv":
		os.Exit(csvCmd(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "driver: unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

// runCmd drives one subject and appends its sample line to -out.
func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	subject := fs.String("subject", "", "subject label: baseline, luxd, or litellm")
	group := fs.String("group", "passthrough", "request group: passthrough or translated")
	shape := fs.String("shape", "openai", "request wire shape: openai or anthropic")
	stream := fs.Bool("stream", false, "send stream:true and read the SSE body to completion")
	url := fs.String("url", "", "chat/completions (or messages) endpoint to drive")
	key := fs.String("key", "", "credential value")
	model := fs.String("model", "", "model named in the request body")
	concurrency := fs.Int("concurrency", 50, "in-flight requests held constant")
	requests := fs.Int("requests", 20000, "measured requests")
	warmup := fs.Int("warmup", 2000, "warmup requests, discarded")
	rssPID := fs.Int("rss-pid", 0, "root pid whose process tree RSS is sampled; 0 disables")
	trial := fs.Int("trial", 1, "1-based trial index recorded on the sample line so trials aggregate")
	out := fs.String("out", "", "file to append the JSON sample line to; empty is stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *subject == "" || *url == "" || *model == "" {
		fmt.Fprintln(os.Stderr, "run: -subject, -url and -model are required")
		return 2
	}
	mode := "nonstream"
	if *stream {
		mode = "stream"
	}

	body := requestBody(*model, *stream)
	client := newClient(*concurrency)

	// The RSS sampler runs across warmup and measurement, because a
	// resident set grows toward its steady state and the true peak may be
	// reached while the warmup is still filling caches and pools.
	var peakKB int64
	ctx, stop := context.WithCancel(context.Background())
	var sw sync.WaitGroup
	if *rssPID > 0 {
		sw.Add(1)
		go func() { defer sw.Done(); sampleRSS(ctx, *rssPID, &peakKB) }()
	}

	// A single request first, so a misconfigured subject fails loudly
	// before thousands of requests bury the reason.
	if err := doOne(client, *url, *shape, *key, body, *stream); err != nil {
		stop()
		sw.Wait()
		fmt.Fprintf(os.Stderr, "run %s/%s/%s: confirm request failed: %v\n", *subject, *group, mode, err)
		return 1
	}

	if *warmup > 0 {
		_, _, _ = runPhase(client, *url, *shape, *key, body, *stream, *concurrency, *warmup)
	}
	lat, errs, wall := runPhase(client, *url, *shape, *key, body, *stream, *concurrency, *requests)

	stop()
	sw.Wait()

	sort.Float64s(lat)
	s := sample{
		Trial:   *trial,
		Subject: *subject, Group: *group, Mode: mode, Shape: *shape,
		URL: *url, Model: *model, Concurrency: *concurrency,
		Requests: *requests, Warmup: *warmup, Errors: errs,
		WallSeconds: round(wall, 4), Throughput: round(float64(*requests)/wall, 1),
		P50ms: pct(lat, 50), P75ms: pct(lat, 75), P90ms: pct(lat, 90),
		P95ms: pct(lat, 95), P99ms: pct(lat, 99),
		PeakRSSMB: -1,
	}
	if *rssPID > 0 {
		s.PeakRSSMB = round(float64(atomic.LoadInt64(&peakKB))/1024.0, 1)
	}

	line, _ := json.Marshal(s)
	if *out == "" {
		fmt.Println(string(line))
	} else {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run: open %s: %v\n", *out, err)
			return 1
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			fmt.Fprintf(os.Stderr, "run: write %s: %v\n", *out, err)
			_ = f.Close()
			return 1
		}
		_ = f.Close()
	}
	fmt.Fprintf(os.Stderr,
		"%-8s %-11s %-9s  p50=%.3fms p90=%.3fms p99=%.3fms  %.0f req/s  errors=%d  rss=%.1fMB\n",
		s.Subject, s.Group, s.Mode, s.P50ms, s.P90ms, s.P99ms, s.Throughput, s.Errors, s.PeakRSSMB)
	if errs > 0 {
		return 1
	}
	return 0
}

// runPhase drives n requests closed-loop at the given concurrency and
// returns each request's latency in milliseconds, the count that failed,
// and the wall time of the phase.
func runPhase(client *http.Client, url, shape, key string, body []byte, stream bool, concurrency, n int) ([]float64, int, float64) {
	lat := make([]float64, n)
	var idx int64 = -1
	var errc int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddInt64(&idx, 1)
				if i >= int64(n) {
					return
				}
				t0 := time.Now()
				err := doOne(client, url, shape, key, body, stream)
				lat[i] = float64(time.Since(t0).Microseconds()) / 1000.0
				if err != nil {
					atomic.AddInt64(&errc, 1)
				}
			}
		}()
	}
	wg.Wait()
	return lat, int(errc), time.Since(start).Seconds()
}

// doOne sends one request and reads the whole response body, so the
// latency covers the full round trip including the streamed body.
func doOne(client *http.Client, url, shape, key string, body []byte, stream bool) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	switch shape {
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// requestBody renders the request the shape asks for. The OpenAI Chat
// Completions body and the Anthropic Messages body share these members
// (model, max_tokens, messages), so one map serves both; only the
// credential header and the endpoint differ, and doOne sets those. The
// openai shape is what both proxies accept at their door; the anthropic
// shape is for driving an anthropic-format mock directly as the translated
// baseline.
func requestBody(model string, stream bool) []byte {
	m := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "Say hello in one short sentence."}},
	}
	if stream {
		m["stream"] = true
	}
	b, _ := json.Marshal(m)
	return b
}

// newClient builds a keep-alive client sized to the concurrency, so the
// pool holds one connection per in-flight request and no request pays a
// fresh dial after warmup.
func newClient(concurrency int) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        concurrency * 2,
		MaxIdleConnsPerHost: concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// pct is the nearest-rank percentile of a sorted slice, in the same unit
// (milliseconds) as its elements.
func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return round(sorted[rank-1], 3)
}

func round(v float64, places int) float64 {
	f := math.Pow(10, float64(places))
	return math.Round(v*f) / f
}

// sampleRSS tracks the peak resident set of pid and its descendants,
// polling ps every 50ms until ctx ends. RSS is read in KiB on Darwin.
func sampleRSS(ctx context.Context, pid int, peakKB *int64) {
	record := func() {
		if kb := rssTreeKB(pid); int64(kb) > atomic.LoadInt64(peakKB) {
			atomic.StoreInt64(peakKB, int64(kb))
		}
	}
	record() // an immediate sample, so a short run still records one
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			record() // a final sample at teardown
			return
		case <-t.C:
			record()
		}
	}
}

// rssTreeKB sums the resident set, in KiB, of root and every process
// descended from it, so a proxy that forks a worker is measured whole.
func rssTreeKB(root int) int {
	out, err := exec.Command("ps", "-Ao", "pid=,ppid=,rss=").Output()
	if err != nil {
		return 0
	}
	rss := map[int]int{}
	children := map[int][]int{}
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		r, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		rss[pid] = r
		children[ppid] = append(children[ppid], pid)
	}
	total := 0
	stack := []int{root}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		total += rss[p]
		stack = append(stack, children[p]...)
	}
	return total
}

// reportCmd reads a run file and prints a Markdown table per scenario.
func reportCmd(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	in := fs.String("in", "", "run file of JSON sample lines")
	md := fs.String("md", "-", "Markdown output path; - is stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "report: -in is required")
		return 2
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		return 1
	}
	var samples []sample
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var s sample
		if err := json.Unmarshal([]byte(ln), &s); err != nil {
			fmt.Fprintf(os.Stderr, "report: bad line: %v\n", err)
			return 1
		}
		samples = append(samples, s)
	}

	var b strings.Builder
	// Scenarios in a fixed order; subjects in a fixed order within each.
	scenarios := []struct{ group, mode, title string }{
		{"passthrough", "nonstream", "Passthrough (OpenAI in, OpenAI upstream), non-streaming"},
		{"passthrough", "stream", "Passthrough (OpenAI in, OpenAI upstream), streaming (SSE)"},
		{"translated", "nonstream", "Translated (OpenAI in, Anthropic upstream), non-streaming"},
		{"translated", "stream", "Translated (OpenAI in, Anthropic upstream), streaming (SSE)"},
	}
	order := map[string]int{"baseline": 0, "luxd": 1, "litellm": 2}
	for _, sc := range scenarios {
		var rows []sample
		for _, s := range samples {
			if s.Group == sc.group && s.Mode == sc.mode {
				rows = append(rows, s)
			}
		}
		if len(rows) == 0 {
			continue
		}
		sort.Slice(rows, func(i, j int) bool { return order[rows[i].Subject] < order[rows[j].Subject] })
		fmt.Fprintf(&b, "### %s\n\n", sc.title)
		fmt.Fprintf(&b, "Concurrency %d, %d measured requests after %d warmup.\n\n",
			rows[0].Concurrency, rows[0].Requests, rows[0].Warmup)
		b.WriteString("| Subject | p50 (ms) | p75 (ms) | p90 (ms) | p95 (ms) | p99 (ms) | Throughput (req/s) | Errors | Peak RSS (MB) |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, s := range rows {
			rss := "n/a"
			if s.PeakRSSMB >= 0 {
				rss = fmt.Sprintf("%.1f", s.PeakRSSMB)
			}
			fmt.Fprintf(&b, "| %s | %.3f | %.3f | %.3f | %.3f | %.3f | %.0f | %d | %s |\n",
				s.Subject, s.P50ms, s.P75ms, s.P90ms, s.P95ms, s.P99ms, s.Throughput, s.Errors, rss)
		}
		b.WriteString("\n")
	}

	if *md == "-" {
		fmt.Print(b.String())
		return 0
	}
	if err := os.WriteFile(*md, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		return 1
	}
	return 0
}

// csvCmd reads a run file of JSON sample lines and writes tidy per-trial CSV:
// one row per (sample, metric), columns trial, shape, mode, subject, metric,
// value, where shape is the request group (passthrough or translated). The
// baseline has no proxy process, so its peak_rss_mb row is omitted rather
// than written as the -1 sentinel. Rows are sorted so the file is stable
// across runs regardless of the order the trials were appended in.
func csvCmd(args []string) int {
	fs := flag.NewFlagSet("csv", flag.ContinueOnError)
	in := fs.String("in", "", "run file of JSON sample lines")
	out := fs.String("out", "-", "CSV output path; - is stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "csv: -in is required")
		return 2
	}
	samples, err := readSamples(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "csv: %v\n", err)
		return 1
	}

	subjectOrder := map[string]int{"baseline": 0, "luxd": 1, "litellm": 2}
	groupOrder := map[string]int{"passthrough": 0, "translated": 1}
	modeOrder := map[string]int{"nonstream": 0, "stream": 1}
	sort.SliceStable(samples, func(i, j int) bool {
		a, b := samples[i], samples[j]
		if a.Trial != b.Trial {
			return a.Trial < b.Trial
		}
		if a.Group != b.Group {
			return groupOrder[a.Group] < groupOrder[b.Group]
		}
		if a.Mode != b.Mode {
			return modeOrder[a.Mode] < modeOrder[b.Mode]
		}
		return subjectOrder[a.Subject] < subjectOrder[b.Subject]
	})

	type metric struct {
		name string
		val  float64
		have bool
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"trial", "shape", "mode", "subject", "metric", "value"})
	for _, s := range samples {
		for _, m := range []metric{
			{"p50_ms", s.P50ms, true},
			{"p75_ms", s.P75ms, true},
			{"p90_ms", s.P90ms, true},
			{"p95_ms", s.P95ms, true},
			{"p99_ms", s.P99ms, true},
			{"reqs_per_sec", s.Throughput, true},
			{"peak_rss_mb", s.PeakRSSMB, s.PeakRSSMB >= 0},
		} {
			if !m.have {
				continue
			}
			_ = w.Write([]string{
				strconv.Itoa(s.Trial), s.Group, s.Mode, s.Subject,
				m.name, strconv.FormatFloat(m.val, 'f', -1, 64),
			})
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintf(os.Stderr, "csv: %v\n", err)
		return 1
	}

	if *out == "-" {
		fmt.Print(b.String())
		return 0
	}
	if err := os.WriteFile(*out, b.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "csv: %v\n", err)
		return 1
	}
	return 0
}

// readSamples reads a run file of JSON sample lines, skipping blank lines.
func readSamples(path string) ([]sample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var samples []sample
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var s sample
		if err := json.Unmarshal([]byte(ln), &s); err != nil {
			return nil, fmt.Errorf("bad line: %w", err)
		}
		samples = append(samples, s)
	}
	return samples, nil
}
