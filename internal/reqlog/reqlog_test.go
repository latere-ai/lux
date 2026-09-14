// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/retry"
	"latere.ai/x/pkg/s3"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// clock is the fake clock the exporter and the records share.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// logs is a logger over a buffer the test reads back.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// fastRetry keeps the write policy's attempts and waits nothing, so a
// failing bucket fails at once.
var fastRetry = retry.Policy{MaxAttempts: 2, Base: time.Nanosecond, Max: time.Nanosecond, Jitter: -1}

// harness is one bucket, clock, registry, and exporter.
type harness struct {
	c     *clock
	srv   *s3test.Server
	reg   *metrics.Registry
	log   *logs
	ids   int
	idsMu sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{c: newClock(), srv: s3test.New(t, "archive"), reg: metrics.NewRegistry(), log: &logs{}}
}

// newID mints ordered, readable ULIDs.
func (h *harness) newID(time.Time) string {
	h.idsMu.Lock()
	defer h.idsMu.Unlock()
	h.ids++
	return fmt.Sprintf("01ULID%020d", h.ids)
}

// exporter builds an exporter over the bucket with the replica named.
func (h *harness) exporter(edit func(*ExporterOptions)) *Exporter {
	o := ExporterOptions{
		Bucket: h.srv.Client(true, s3.WithRetry(fastRetry)), Prefix: "lux/", Replica: "replica-a",
		Metrics: h.reg, Logger: slog.New(slog.NewTextHandler(h.log, nil)), Now: h.c.Now, NewID: h.newID,
	}
	if edit != nil {
		edit(&o)
	}
	return NewExporter(o)
}

// record is one record numbered i at the clock, its Key and status
// alternating so the filters have something to select.
func record(i int, at time.Time) metering.Record {
	r := metering.Record{
		ID: fmt.Sprintf("req_%06d", i), At: at.UTC(), EndedAt: at.UTC().Add(time.Second),
		Key:   metering.KeyRef{ID: "key_" + string(rune('a'+i%2)), Prefix: "lux_ab12cd34ef56"},
		Owner: "https://login.example.com|alice",
		Model: metering.Ref{Name: "gpt", ID: "mdl_1"}, Provider: metering.Ref{Name: "openai", ID: "prv_1"},
		UpstreamModel: "gpt-4.1", Door: "openai", TargetDialect: "openai", Route: "/openai/v1/chat/completions",
		Loss: []string{}, Attempts: []metering.Attempt{{Provider: "openai", UpstreamModel: "gpt-4.1", Status: metering.StatusOK, HTTPStatus: 200, DurationMs: 12}},
		Status: metering.StatusOK, LatencyMs: 12, TTFBMs: 3,
		Tokens: metering.Tokens{Input: 12, Output: 3}, Cost: metering.Charge{Amount: 1500, Currency: "USD", Priced: true},
		Labels: map[string]string{"run": "r_42"}, RequestLabels: map[string]string{},
	}
	if i%3 == 0 {
		r.Status, r.Error = metering.StatusRefused, "rate_limited"
	}
	return r
}

// lines reads one object back as records.
func (h *harness) lines(t *testing.T, key string) []metering.Record {
	t.Helper()
	data, ok := h.srv.Get(key)
	if !ok {
		t.Fatalf("no object %s among %v", key, h.srv.Keys())
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("%s does not end in a newline", key)
	}
	var out []metering.Record
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		var r metering.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("%s: line %q: %v", key, line, err)
		}
		out = append(out, r)
	}
	return out
}

// archived reads every object back, in key order.
func (h *harness) archived(t *testing.T) []metering.Record {
	t.Helper()
	var out []metering.Record
	for _, k := range h.srv.Keys() {
		out = append(out, h.lines(t, k)...)
	}
	return out
}

// same compares two records through their JSON.
func same(a, b metering.Record) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// ids is the record ids in order.
func ids(recs []metering.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

// await polls until cond holds or five seconds pass.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestBufferDropsOldest: the ring holds its cap and drops the oldest
// record to make room, a batch stays at the head while it is written so
// a drop meanwhile takes the batch's own oldest, the acknowledgement
// removes the batch alone, and the exporter counts every drop in
// lux_requestlog_dropped_total with one WARN line a minute.
func TestBufferDropsOldest(t *testing.T) {
	r := newRing(5)
	at := newClock().Now()
	for i := range 7 {
		if dropped := r.push(record(i, at)); dropped != (i >= 5) {
			t.Fatalf("push %d dropped %v", i, dropped)
		}
	}
	if r.len() != 5 {
		t.Fatalf("len %d", r.len())
	}
	oldest, through := r.peek(3)
	if got := ids(oldest); !slices.Equal(got, []string{"req_000002", "req_000003", "req_000004"}) || r.len() != 5 {
		t.Fatalf("peek = %v, len %d", got, r.len())
	}
	// A push while the batch is out drops the batch's own oldest; the
	// ack then removes what is left of the batch and nothing pushed since.
	if !r.push(record(7, at)) {
		t.Fatal("a push at the cap dropped nothing")
	}
	r.ack(through)
	if got, _ := r.peek(10); !slices.Equal(ids(got), []string{"req_000005", "req_000006", "req_000007"}) || r.len() != 3 {
		t.Fatalf("after the ack: %v, len %d", ids(got), r.len())
	}
	r.ack(through)
	if r.len() != 3 {
		t.Fatal("a second ack removed more")
	}
	if got, _ := r.peek(0); len(got) != 0 {
		t.Fatalf("peek 0 = %v", got)
	}
	empty := newRing(2)
	if got, _ := empty.peek(1); len(got) != 0 {
		t.Fatalf("peek from empty = %v", got)
	}

	h := newHarness(t)
	e := h.exporter(func(o *ExporterOptions) { o.Cap = 3 })
	for i := range 5 {
		e.Append(record(i, h.c.Now()))
	}
	if e.Len() != 3 || e.Dropped() != 2 || h.reg.Counter(MetricDropped, "").Value(nil) != 2 {
		t.Fatalf("len %d, dropped %d, counter %d", e.Len(), e.Dropped(), h.reg.Counter(MetricDropped, "").Value(nil))
	}
	if n := strings.Count(h.log.String(), "requestlog: dropped the oldest records"); n != 1 || !strings.Contains(h.log.String(), "dropped=1 cap=3 total=1") {
		t.Fatalf("%d WARN lines in the first minute:\n%s", n, h.log.String())
	}
	h.c.advance(time.Minute)
	e.Append(record(5, h.c.Now()))
	if n := strings.Count(h.log.String(), "requestlog: dropped the oldest records"); n != 2 || !strings.Contains(h.log.String(), "dropped=2 cap=3 total=3") {
		t.Fatalf("%d WARN lines after a minute:\n%s", n, h.log.String())
	}
	if got, _ := e.ring.peek(10); !slices.Equal(ids(got), []string{"req_000003", "req_000004", "req_000005"}) {
		t.Fatalf("the ring holds %v", ids(got))
	}
}

// TestArchiveBatching: the figures are the table's; a batch of the flush
// size is written at the size without waiting for the interval, a
// partial batch at the interval, each object one JSON object per line
// under application/x-ndjson, every line the record that was appended.
func TestArchiveBatching(t *testing.T) {
	if FlushSize != 5000 || FlushInterval != 30*time.Second || BufferCap != 50_000 || ContentType != "application/x-ndjson" || DefaultPrefix != "lux/" {
		t.Fatal("the figures moved")
	}
	if WritePolicy != (retry.Policy{MaxAttempts: 5, Base: time.Second, Max: time.Minute, Timeout: 30 * time.Second}) {
		t.Fatalf("WritePolicy = %+v", WritePolicy)
	}
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	full := h.exporter(func(o *ExporterOptions) { o.FlushInterval = time.Hour })
	go full.Run(ctx)
	want := make([]metering.Record, 0, FlushSize)
	for i := range FlushSize {
		r := record(i, h.c.Now())
		want = append(want, r)
		full.Append(r)
	}
	await(t, "the full batch", func() bool { return len(h.srv.Keys()) == 1 })
	key := h.srv.Keys()[0]
	if ct, _ := h.srv.ContentType(key); ct != ContentType {
		t.Fatalf("Content-Type %q", ct)
	}
	if !strings.HasPrefix(key, "lux/2026/09/14/10/replica-a-01ULID") || !strings.HasSuffix(key, ".ndjson") {
		t.Fatalf("key %s", key)
	}
	got := h.lines(t, key)
	if len(got) != FlushSize {
		t.Fatalf("%d lines", len(got))
	}
	for i := range got {
		if !same(got[i], want[i]) {
			t.Fatalf("line %d differs:\n%+v\n%+v", i, got[i], want[i])
		}
	}
	if full.Len() != 0 {
		t.Fatalf("%d records left after the flush", full.Len())
	}
	cancel()

	partial := h.exporter(func(o *ExporterOptions) { o.FlushInterval = 10 * time.Millisecond })
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	go partial.Run(ctx)
	for i := range 3 {
		partial.Append(record(100+i, h.c.Now()))
	}
	await(t, "the partial batch", func() bool { return len(h.srv.Keys()) == 2 })
	if got := ids(h.lines(t, h.srv.Keys()[1])); !slices.Equal(got, []string{"req_000100", "req_000101", "req_000102"}) {
		t.Fatalf("the partial object holds %v", got)
	}
	if h.srv.Count(http.MethodPut) != 2 {
		t.Fatalf("%d PUTs", h.srv.Count(http.MethodPut))
	}
}

// TestArchiveObjectKey: the key is <prefix>/yyyy/mm/dd/hh/<replica>-
// <ulid>.ndjson with the UTC hour of the batch's first record, whatever
// hour its later records fall in; the replica is the hostname reduced
// to [a-z0-9-]; two objects of one replica sort in write order; the
// defaults fill an empty option.
func TestArchiveObjectKey(t *testing.T) {
	first := time.Date(2026, 9, 14, 23, 59, 59, 0, time.FixedZone("plus-two", 2*3600))
	if got := objectKey("lux/", "pod-7", first, "01ULID"); got != "lux/2026/09/14/21/pod-7-01ULID.ndjson" {
		t.Fatalf("objectKey = %s", got)
	}
	if got := objectKey("archive", "pod-7", first, "01ULID"); got != "archive/2026/09/14/21/pod-7-01ULID.ndjson" {
		t.Fatalf("objectKey without a slash = %s", got)
	}
	for in, want := range map[string]string{"Pod-Name_1.local": "pod-name-1-local", "REPLICA": "replica", "": "replica", "---": "replica", "lux-7d9c": "lux-7d9c"} {
		if got := Replica(in); got != want {
			t.Errorf("Replica(%q) = %q, want %q", in, got, want)
		}
	}
	h := newHarness(t)
	e := h.exporter(func(o *ExporterOptions) { o.FlushSize = 3 })
	at := h.c.Now().Add(59 * time.Minute) // 11:29
	e.Append(record(1, at))
	e.Append(record(2, at.Add(time.Hour)))
	e.Append(record(3, at.Add(2*time.Hour)))
	if err := e.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.c.advance(time.Second)
	e.Append(record(4, at.Add(3*time.Hour)))
	if err := e.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keys := h.srv.Keys()
	if len(keys) != 2 || keys[0] != "lux/2026/09/14/11/replica-a-01ULID00000000000000000001.ndjson" || keys[1] != "lux/2026/09/14/14/replica-a-01ULID00000000000000000002.ndjson" {
		t.Fatalf("keys %v", keys)
	}
	if err := e.Flush(t.Context()); err != nil || len(h.srv.Keys()) != 2 {
		t.Fatalf("an empty flush wrote: %v %v", err, h.srv.Keys())
	}
	d := NewExporter(ExporterOptions{Bucket: h.srv.Client(true)})
	if d.o.Prefix != DefaultPrefix || d.o.Replica == "" || d.o.FlushSize != FlushSize || d.o.FlushInterval != FlushInterval || d.o.Cap != BufferCap || d.o.Logger == nil || d.o.Now == nil || len(d.o.NewID(time.Now())) != 26 {
		t.Fatalf("defaults %+v", d.o)
	}
}

// twoHours writes six objects across two hours from three replicas and
// returns every record produced, in production order.
func (h *harness) twoHours(t *testing.T) []metering.Record {
	t.Helper()
	var produced []metering.Record
	n := 0
	for hour := range 2 {
		for _, replica := range []string{"replica-a", "replica-b", "replica-c"} {
			e := h.exporter(func(o *ExporterOptions) { o.Replica = replica; o.FlushSize = 4 })
			for range 4 {
				r := record(n, h.c.Now().Add(time.Duration(hour)*time.Hour+time.Duration(n)*time.Second))
				produced = append(produced, r)
				e.Append(r)
				n++
			}
			if err := e.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	return produced
}

// TestArchiveRoundTrip: every line of every archived object parses as
// one record, the records of one hour's objects are the records
// produced in that hour and no other, and the reader answers the whole
// range and a filtered one.
func TestArchiveRoundTrip(t *testing.T) {
	h := newHarness(t)
	produced := h.twoHours(t)
	got := h.archived(t)
	if len(got) != len(produced) {
		t.Fatalf("%d archived, %d produced", len(got), len(produced))
	}
	byID := map[string]metering.Record{}
	for _, r := range produced {
		byID[r.ID] = r
	}
	for _, r := range got {
		if !same(r, byID[r.ID]) {
			t.Fatalf("%s differs after the round trip", r.ID)
		}
	}
	for hour, want := range map[string][]string{"10": ids(produced[:12]), "11": ids(produced[12:])} {
		var in []string
		for _, k := range h.srv.Keys() {
			if strings.HasPrefix(k, "lux/2026/09/14/"+hour+"/") {
				in = append(in, ids(h.lines(t, k))...)
			}
		}
		sort.Strings(in)
		if !slices.Equal(in, want) {
			t.Fatalf("hour %s holds %v, want %v", hour, in, want)
		}
	}
	rd := NewReader(h.srv.Client(true), "lux/")
	from := h.c.Now().Truncate(time.Hour)
	q := metering.RecordQuery{From: from, To: from.Add(2 * time.Hour)}
	all, next, err := rd.List(t.Context(), q, store.Page{})
	if err != nil || next != "" || len(all) != len(produced) {
		t.Fatalf("List = %d, %q, %v", len(all), next, err)
	}
	// Newest hour first, then the listing's order.
	if all[0].ID != "req_000012" || all[12].ID != "req_000000" {
		t.Fatalf("order: %v", ids(all))
	}
	refused, _, err := rd.List(t.Context(), metering.RecordQuery{Query: q.Query, Status: metering.StatusRefused, Error: "rate_limited"}, store.Page{})
	if err != nil || len(refused) != 8 {
		t.Fatalf("refused = %d, %v", len(refused), err)
	}
	keyB, _, err := rd.List(t.Context(), metering.RecordQuery{From: from, To: from.Add(2 * time.Hour), Keys: []string{"key_b"}}, store.Page{})
	if err != nil || len(keyB) != 12 {
		t.Fatalf("key_b = %d, %v", len(keyB), err)
	}
	// A range inside the first hour alone.
	one, _, err := rd.List(t.Context(), metering.RecordQuery{From: h.c.Now().Add(5 * time.Second), To: h.c.Now().Add(10 * time.Second)}, store.Page{})
	if err != nil || len(one) != 5 {
		t.Fatalf("a five second range = %d, %v", len(one), err)
	}
}

// TestArchiveReaderPages: the reader pages through two hours of objects
// with a cursor of the object key and line, every record once and in
// the same order as one unpaged read; a cursor from another query, of
// another kind, malformed, or naming a key outside the prefix is
// store.ErrInvalidCursor; a malformed line, a listing failure, and a
// read failure are errors naming the object.
func TestArchiveReaderPages(t *testing.T) {
	h := newHarness(t)
	produced := h.twoHours(t)
	rd := NewReader(h.srv.Client(true), "lux/")
	from := h.c.Now().Truncate(time.Hour)
	q := metering.RecordQuery{From: from, To: from.Add(2 * time.Hour)}
	all, _, err := rd.List(t.Context(), q, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 3, 7, 24, 100} {
		var paged []metering.Record
		cursor := ""
		pages := 0
		for {
			page, next, err := rd.List(t.Context(), q, store.Page{Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatalf("limit %d page %d: %v", limit, pages, err)
			}
			pages++
			paged = append(paged, page...)
			if next == "" {
				break
			}
			if len(page) != limit {
				t.Fatalf("limit %d: a page of %d with a cursor", limit, len(page))
			}
			cursor = next
		}
		if !slices.Equal(ids(paged), ids(all)) || len(paged) != len(produced) {
			t.Fatalf("limit %d: paged %v\nwant %v", limit, ids(paged), ids(all))
		}
		if want := (len(all) + limit - 1) / limit; pages != want && pages != want+1 {
			t.Fatalf("limit %d: %d pages", limit, pages)
		}
	}
	// A page that ends exactly at an object's last line resumes at the
	// next object.
	page, next, err := rd.List(t.Context(), q, store.Page{Limit: 4})
	if err != nil || next == "" || len(page) != 4 {
		t.Fatalf("first page: %d %q %v", len(page), next, err)
	}
	key, line, err := decodeCursor(next, q)
	if err != nil || line != 4 || !strings.HasPrefix(key, "lux/2026/09/14/11/replica-a-") {
		t.Fatalf("cursor = %s %d %v", key, line, err)
	}
	rest, _, err := rd.List(t.Context(), q, store.Page{Cursor: next})
	if err != nil || len(rest) != len(all)-4 || rest[0].ID != all[4].ID {
		t.Fatalf("the rest: %d %v", len(rest), err)
	}

	other := metering.RecordQuery{From: from, To: from.Add(time.Hour)}
	for name, cursor := range map[string]string{
		"another query": next,
		"garbage":       "not-base64!",
		"few fields":    encodeCursor(other, "", 0)[:8],
		"another kind":  encodeBase64("records|00000000|k|0"),
		"a bad line":    encodeBase64(CursorKind + "|" + digest(other) + "|lux/2026/09/14/10/x-1.ndjson|many"),
		"outside":       encodeCursor(other, "elsewhere/2026/09/14/10/x-1.ndjson", 0),
		"no hour":       encodeCursor(other, "lux/x-1.ndjson", 0),
		"a bad hour":    encodeCursor(other, "lux/2026/13/40/99/x-1.ndjson", 0),
	} {
		if _, _, err := rd.List(t.Context(), other, store.Page{Cursor: cursor}); !errors.Is(err, store.ErrInvalidCursor) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := rd.List(t.Context(), metering.RecordQuery{}, store.Page{}); err == nil {
		t.Error("a query without a range was answered")
	}
	// A cursor naming an hour outside the range is not this query's.
	for _, key := range []string{"lux/2026/09/15/10/x-1.ndjson", "lux/2026/09/14/09/x-1.ndjson"} {
		if _, _, err := rd.List(t.Context(), q, store.Page{Cursor: encodeCursor(q, key, 0)}); !errors.Is(err, store.ErrInvalidCursor) {
			t.Fatalf("a cursor outside the range: %v", err)
		}
	}
	h.srv.Put("lux/2026/09/14/11/replica-z-01ULID.ndjson", []byte("{\"id\":\"req_ok\",\"at\":\"2026-09-14T11:00:00Z\"}\nnot json\n"))
	if _, _, err := rd.List(t.Context(), q, store.Page{}); err == nil || !strings.Contains(err.Error(), "replica-z-01ULID.ndjson line 2") {
		t.Fatalf("a malformed line: %v", err)
	}
	h.srv.Fail(3, http.StatusInternalServerError) // the test client's three attempts
	if _, _, err := rd.List(t.Context(), q, store.Page{}); err == nil || !strings.Contains(err.Error(), "listing lux/2026/09/14/11/") {
		t.Fatalf("a failed listing: %v", err)
	}
	h.srv.Fail(1, http.StatusNotFound)
	if _, _, err := rd.List(t.Context(), q, store.Page{}); err == nil || !strings.Contains(err.Error(), "listing lux/2026/09/14/11/") {
		t.Fatalf("a refused listing: %v", err)
	}
	failing := NewReader(&getFails{h.srv.Client(true)}, "")
	if _, _, err := failing.List(t.Context(), q, store.Page{}); err == nil || !strings.Contains(err.Error(), "reading lux/2026/09/14/11/") {
		t.Fatalf("a failed read: %v", err)
	}
	if failing.prefix != DefaultPrefix {
		t.Fatalf("prefix %q", failing.prefix)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := rd.List(ctx, q, store.Page{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read: %v", err)
	}
}

// getFails is a Bucket whose reads fail.
type getFails struct{ Bucket }

func (getFails) GetObject(context.Context, string, string) (io.ReadCloser, s3.Object, error) {
	return nil, s3.Object{}, errors.New("the read failed")
}

func encodeBase64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// TestArchiveOutageNeverBlocksTheHotPath: with the bucket refusing every
// write, appends stay under the bound, the ring stops at its cap, the
// oldest records are dropped, and lux_requestlog_dropped_total counts
// exactly the drops.
func TestArchiveOutageNeverBlocksTheHotPath(t *testing.T) {
	h := newHarness(t)
	h.srv.Fail(1<<30, http.StatusServiceUnavailable)
	e := h.exporter(func(o *ExporterOptions) { o.FlushInterval = time.Millisecond })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	const extra = 100
	bound := time.Millisecond
	if raceEnabled {
		bound = 20 * time.Millisecond
	}
	var slowest time.Duration
	over := 0
	for i := range BufferCap + extra {
		start := time.Now()
		e.Append(record(i, h.c.Now()))
		if d := time.Since(start); d > slowest {
			slowest = d
		}
		if time.Since(start) > bound {
			over++
		}
	}
	if over > (BufferCap+extra)/1000 {
		t.Fatalf("%d appends over %s, the slowest %s", over, bound, slowest)
	}
	if e.Len() != BufferCap {
		t.Fatalf("the ring holds %d, want the cap %d", e.Len(), BufferCap)
	}
	if e.Dropped() != extra || h.reg.Counter(MetricDropped, "").Value(nil) != extra {
		t.Fatalf("dropped %d, counter %d, want %d", e.Dropped(), h.reg.Counter(MetricDropped, "").Value(nil), extra)
	}
	if oldest, _ := e.ring.peek(1); oldest[0].ID != fmt.Sprintf("req_%06d", extra) {
		t.Fatalf("the oldest record left is %s, want the one after the %d dropped", oldest[0].ID, extra)
	}
	cancel()
	<-done
	if len(h.srv.Keys()) != 0 || !strings.Contains(h.log.String(), "requestlog: writing a batch; it stays in the buffer") || !strings.Contains(h.log.String(), "the records still in the buffer are lost") {
		t.Fatalf("keys %v\n%s", h.srv.Keys(), h.log.String())
	}
}

// TestArchiveRecoversWithoutLoss: a bucket that refuses writes for a
// while, with fewer records appended than the ring holds, loses nothing
// once it answers again: every record lands exactly once.
func TestArchiveRecoversWithoutLoss(t *testing.T) {
	h := newHarness(t)
	h.srv.Fail(6, http.StatusServiceUnavailable)
	e := h.exporter(func(o *ExporterOptions) { o.FlushSize = 10; o.FlushInterval = 5 * time.Millisecond })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.Run(ctx)
	const total = 45
	for i := range total {
		e.Append(record(i, h.c.Now()))
		time.Sleep(time.Millisecond)
	}
	await(t, "every record archived", func() bool {
		n := 0
		for _, k := range h.srv.Keys() {
			data, _ := h.srv.Get(k)
			n += bytes.Count(data, []byte("\n"))
		}
		return n == total && e.Len() == 0
	})
	got := ids(h.archived(t))
	sort.Strings(got)
	want := ids(func() []metering.Record {
		out := make([]metering.Record, 0, total)
		for i := range total {
			out = append(out, record(i, h.c.Now()))
		}
		return out
	}())
	if !slices.Equal(got, want) || e.Dropped() != 0 {
		t.Fatalf("archived %v, dropped %d", got, e.Dropped())
	}
	if !strings.Contains(h.log.String(), "it stays in the buffer for the next flush") {
		t.Fatalf("no WARN for the refused writes:\n%s", h.log.String())
	}
}

// TestDrainWritesTheBuffer: when Run's context ends, what the ring holds
// is written before Run returns, in batches when there is more than one.
func TestDrainWritesTheBuffer(t *testing.T) {
	h := newHarness(t)
	e := h.exporter(func(o *ExporterOptions) { o.FlushSize = 4; o.FlushInterval = time.Hour })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	for i := range 3 {
		e.Append(record(i, h.c.Now()))
	}
	if len(h.srv.Keys()) != 0 {
		t.Fatal("a partial batch was written before the interval")
	}
	cancel()
	<-done
	if got := ids(h.archived(t)); !slices.Equal(got, []string{"req_000000", "req_000001", "req_000002"}) || e.Len() != 0 {
		t.Fatalf("after the drain: %v, %d left", got, e.Len())
	}
	// More than one batch drains in order.
	e = h.exporter(func(o *ExporterOptions) { o.FlushSize = 4; o.FlushInterval = time.Hour })
	ctx, cancel = context.WithCancel(t.Context())
	done = make(chan struct{})
	go func() {
		defer close(done)
		e.Run(ctx)
	}()
	for i := 10; i < 19; i++ {
		e.ring.push(record(i, h.c.Now()))
	}
	cancel()
	<-done
	if len(h.srv.Keys()) != 4 || e.Len() != 0 {
		t.Fatalf("keys %v, %d left", h.srv.Keys(), e.Len())
	}
}
