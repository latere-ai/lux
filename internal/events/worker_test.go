// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/retry"
)

// TestRetryIsResigned: a sink that fails once receives the event again
// after the backoff, with a fresh t, the worker's clock at that attempt,
// and a signature over it; both verify, and the two bodies are the same
// bytes.
func TestRetryIsResigned(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.sink.failFirst(1, http.StatusInternalServerError, "")
	h.append(t, "evt_a1", "key_a")
	w := h.worker("a", nil, nil)
	tick(ctx, w)
	if got := h.sink.ids(); !slices.Equal(got, []string{"evt_a1"}) {
		t.Fatalf("after the first tick: %v", got)
	}
	if rows := h.rows(t, "key_a"); rows[0].Attempts != 1 || !rows[0].AckedAt.IsZero() || rows[0].NextAttemptAt.Sub(h.c.Now()) <= 0 || rows[0].NextAttemptAt.Sub(h.c.Now()) > time.Second {
		t.Fatalf("after the failure: %+v", rows[0])
	}
	if !strings.Contains(h.log.String(), "level=WARN") || !strings.Contains(h.log.String(), "events: delivery failed; retrying") || !strings.Contains(h.log.String(), "status 500") {
		t.Fatalf("no warning in:\n%s", h.log.String())
	}
	h.c.advance(time.Second)
	w.Tick(ctx)
	got := h.sink.deliveries()
	if len(got) != 2 || !bytes.Equal(got[0].body, got[1].body) || got[0].header == got[1].header {
		t.Fatalf("deliveries %d, same body %v, same header %v", len(got), len(got) == 2 && bytes.Equal(got[0].body, got[1].body), len(got) == 2 && got[0].header == got[1].header)
	}
	ts := h.sink.verified(t)
	if !ts[0].Equal(h.c.Now().Add(-time.Second)) || !ts[1].Equal(h.c.Now()) {
		t.Fatalf("t = %v, want the clock at each attempt", ts)
	}
	if rows := h.rows(t, "key_a"); rows[0].AckedAt.IsZero() || w.Pending() != 0 {
		t.Fatalf("not acknowledged: %+v, pending %d", rows[0], w.Pending())
	}
}

// TestDeliveryIsOrderedPerObject: a sink that fails an event three times
// receives it on the fourth attempt, each deferral within the policy's
// delay for that attempt, and that object's later event after it, while
// another object's events are delivered meanwhile.
func TestDeliveryIsOrderedPerObject(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.sink.failFirst(3, http.StatusServiceUnavailable, "evt_a1")
	for _, e := range [][2]string{{"evt_a1", "key_a"}, {"evt_b1", "key_b"}, {"evt_a2", "key_a"}, {"evt_b2", "key_b"}} {
		h.append(t, e[0], e[1])
	}
	w := h.worker("a", nil, nil)
	for attempt := 1; attempt <= 3; attempt++ {
		tick(ctx, w)
		rows := h.rows(t, "key_a")
		delay := rows[0].NextAttemptAt.Sub(h.c.Now())
		ceiling := DeliveryPolicy.Base << (attempt - 1)
		if rows[0].Attempts != attempt || delay <= 0 || delay > ceiling {
			t.Fatalf("attempt %d: attempts %d, delay %s, want within (0, %s]", attempt, rows[0].Attempts, delay, ceiling)
		}
		h.c.advance(ceiling)
	}
	w.Tick(ctx)
	w.Tick(ctx)
	got := h.sink.ids()
	// Attempts 1 to 3 of a1 fail; b1 goes in the first tick, b2 in the
	// second, then a1 on its fourth attempt, then a2.
	if !slices.Equal(got, []string{"evt_a1", "evt_b1", "evt_a1", "evt_b2", "evt_a1", "evt_a1", "evt_a2"}) {
		t.Fatalf("deliveries %v", got)
	}
	for _, object := range []string{"key_a", "key_b"} {
		for _, r := range h.rows(t, object) {
			if r.AckedAt.IsZero() {
				t.Errorf("%s is not acknowledged", r.ID)
			}
		}
	}
	if w.Pending() != 0 {
		t.Fatalf("pending %d", w.Pending())
	}
	h.sink.verified(t)
}

// TestDeliveryResumesFromTheJournal: a restart with a store resumes an
// unacknowledged event where the acknowledgements stopped: a second
// worker on the same store takes the lease the first released and
// delivers the deferred row, from the same moment the sink was set. A
// worker that does not hold the lease delivers nothing.
func TestDeliveryResumesFromTheJournal(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.sink.failFirst(1, http.StatusInternalServerError, "")
	h.append(t, "evt_a1", "key_a")
	first := h.worker("first", nil, nil)
	tick(ctx, first)
	if got := h.sink.ids(); !slices.Equal(got, []string{"evt_a1"}) || first.Pending() != 1 {
		t.Fatalf("first: %v, pending %d", got, first.Pending())
	}
	bystander := h.worker("bystander", nil, nil)
	h.c.advance(time.Second)
	tick(ctx, bystander)
	if bystander.Held() || len(h.sink.ids()) != 1 {
		t.Fatal("a replica without the lease delivered")
	}
	first.release(ctx)
	if first.Held() {
		t.Fatal("still held after release")
	}
	second := h.worker("second", nil, nil)
	tick(ctx, second)
	if !second.Held() || !slices.Equal(h.sink.ids(), []string{"evt_a1", "evt_a1"}) || second.Pending() != 0 {
		t.Fatalf("second: held %v, %v, pending %d", second.Held(), h.sink.ids(), second.Pending())
	}
	if !second.since.Equal(first.since) || second.since.IsZero() {
		t.Fatalf("since: first %s, second %s", first.since, second.since)
	}
	h.sink.verified(t)
}

// TestDeliveryIsAtLeastOnce: an acknowledgement lost after the sink
// committed produces a second POST of the same id, in both ways it is
// lost: the sink answering after the deadline, and the journal refusing
// the acknowledgement.
func TestDeliveryIsAtLeastOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.sink.setPlan(func(n int, _ []byte) (int, bool) { return http.StatusOK, n == 1 })
	h.append(t, "evt_a1", "key_a")
	w := h.worker("a", nil, nil)
	tick(ctx, w)
	if rows := h.rows(t, "key_a"); rows[0].Attempts != 1 || !strings.Contains(h.log.String(), "context deadline exceeded") {
		t.Fatalf("a hanging sink: %+v\n%s", rows[0], h.log.String())
	}
	h.c.advance(time.Second)
	w.Tick(ctx)
	if got := h.sink.ids(); !slices.Equal(got, []string{"evt_a1", "evt_a1"}) {
		t.Fatalf("deliveries %v", got)
	}
	// The journal refuses the acknowledgement once.
	fail := map[string]bool{"Journal.Acknowledge": true}
	h.append(t, "evt_b1", "key_b")
	flaky := h.worker("a", &broken{h.st, fail}, nil)
	tick(ctx, flaky)
	if !strings.Contains(h.log.String(), "acknowledging a delivered event") || len(h.sink.ids()) != 3 {
		t.Fatalf("lost acknowledgement: %v\n%s", h.sink.ids(), h.log.String())
	}
	fail["Journal.Acknowledge"] = false
	flaky.Tick(ctx)
	if got := h.sink.ids(); !slices.Equal(got[2:], []string{"evt_b1", "evt_b1"}) || flaky.Pending() != 0 {
		t.Fatalf("deliveries %v, pending %d", got, flaky.Pending())
	}
}

// TestDeliveryGivesUp: an event that fails for 24 hours is dropped
// through Journal.Drop with an ERROR line naming its id and type, and
// lux_events_pending returns to zero; the row is gone from the journal.
func TestDeliveryGivesUp(t *testing.T) {
	if GiveUpAfter != 24*time.Hour {
		t.Fatalf("GiveUpAfter = %s", GiveUpAfter)
	}
	h := newHarness(t)
	ctx := t.Context()
	h.sink.failFirst(1000, http.StatusInternalServerError, "")
	h.append(t, "evt_a1", "key_a")
	w := h.worker("a", nil, nil)
	tick(ctx, w)
	if w.Pending() != 1 || !strings.Contains(h.scrape(), "lux_events_pending 1") {
		t.Fatalf("pending %d\n%s", w.Pending(), h.scrape())
	}
	h.c.advance(GiveUpAfter)
	w.Tick(ctx)
	if got := h.sink.ids(); !slices.Equal(got, []string{"evt_a1", "evt_a1"}) {
		t.Fatalf("deliveries %v", got)
	}
	if rows := h.rows(t, "key_a"); len(rows) != 0 {
		t.Fatalf("the row is still in the journal: %+v", rows)
	}
	logged := h.log.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "events: dropped an event") || !strings.Contains(logged, "id=evt_a1") || !strings.Contains(logged, "type=key.updated") {
		t.Fatalf("no error line naming the event in:\n%s", logged)
	}
	if w.Pending() != 0 || !strings.Contains(h.scrape(), "lux_events_pending 0") {
		t.Fatalf("pending %d\n%s", w.Pending(), h.scrape())
	}
}

// TestSinkNamedLaterStartsFromThen: rows journalled before the sink was
// first set are acknowledged without a POST, rows after it are
// delivered, and a later holder reads the same moment from the store, so
// a row from between is delivered and not skipped.
func TestSinkNamedLaterStartsFromThen(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.append(t, "evt_a1", "key_a")
	h.append(t, "evt_b1", "key_b")
	h.c.advance(time.Hour)
	set := h.c.Now()
	w := h.worker("a", nil, nil)
	tick(ctx, w)
	if n := len(h.sink.ids()); n != 0 {
		t.Fatalf("%d deliveries of rows from before the sink", n)
	}
	for _, object := range []string{"key_a", "key_b"} {
		if rows := h.rows(t, object); rows[0].AckedAt.IsZero() {
			t.Fatalf("%s is not acknowledged", rows[0].ID)
		}
	}
	if !strings.Contains(h.log.String(), "acknowledged unsent") || !strings.Contains(h.log.String(), "the sink is set from now") || w.Pending() != 0 {
		t.Fatalf("pending %d\n%s", w.Pending(), h.log.String())
	}
	h.c.advance(time.Second)
	h.append(t, "evt_a2", "key_a")
	w.Tick(ctx)
	if got := h.sink.ids(); !slices.Equal(got, []string{"evt_a2"}) {
		t.Fatalf("deliveries %v", got)
	}
	// A later holder, after a restart, reads the moment from the store.
	w.release(ctx)
	h.c.advance(time.Hour)
	h.appendAt(t, "evt_b2", "key_b", set.Add(30*time.Minute))
	later := h.worker("later", nil, nil)
	tick(ctx, later)
	if got := h.sink.ids(); !slices.Equal(got, []string{"evt_a2", "evt_b2"}) || !later.since.Equal(set) {
		t.Fatalf("deliveries %v, since %s, want %s", got, later.since, set)
	}
}

// TestWorkerRunHoldsTheLease: Run takes the lease, delivers on the poll,
// and releases the lease when its context ends, so the next holder does
// not wait out the TTL; the gauge reads nothing on a replica that does
// not hold the lease.
func TestWorkerRunHoldsTheLease(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	w := h.worker("a", nil, func(o *WorkerOptions) { o.Poll = 5 * time.Millisecond })
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	h.append(t, "evt_a1", "key_a")
	deadline := time.Now().Add(5 * time.Second)
	for len(h.sink.ids()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run delivered nothing")
		}
		time.Sleep(time.Millisecond)
	}
	if !w.Held() {
		t.Fatal("Run does not hold the lease")
	}
	cancel()
	<-done
	if w.Held() || strings.Contains(h.scrape(), "lux_events_pending ") {
		t.Fatalf("held %v after Run;\n%s", w.Held(), h.scrape())
	}
	next := h.worker("b", nil, nil)
	next.acquire(t.Context())
	if !next.Held() {
		t.Fatal("the lease was not released")
	}
	if NewWorker(WorkerOptions{Store: h.st}).o.Holder == "" || !strings.Contains(defaultHolder(), ":") {
		t.Fatal("defaultHolder")
	}
}

// TestWorkerStoreFailures: every store failure the worker can meet is
// logged and leaves the row for the next tick; a policy of the caller's
// replaces the default; and the moment the sink was set survives two
// holders writing it at once.
func TestWorkerStoreFailures(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	fail := map[string]bool{}
	st := &broken{h.st, fail}
	w := h.worker("a", st, func(o *WorkerOptions) {
		o.Policy = retry.Policy{Base: time.Minute, Max: time.Minute, Jitter: -1}
		o.GiveUp = time.Hour
	})
	if w.o.Policy.Base != time.Minute || NewWorker(WorkerOptions{Store: h.st}).o.Policy != DeliveryPolicy {
		t.Fatal("the policy option")
	}
	h.append(t, "evt_a1", "key_a")
	expectLog := func(name, want string) {
		t.Helper()
		fail[name] = true
		before := len(h.log.String())
		tick(ctx, w)
		fail[name] = false
		if logged := h.log.String()[before:]; !strings.Contains(logged, want) {
			t.Fatalf("%s: want %q in:\n%s", name, want, logged)
		}
	}
	expectLog("Leases.Acquire", "events: acquiring the lease")
	if w.Held() {
		t.Fatal("held after a failed acquire")
	}
	expectLog("Counters.Read", "events: reading when the sink was set")
	expectLog("Counters.Add", "events: recording the moment the sink was set")
	if !w.since.IsZero() || len(h.sink.ids()) != 0 {
		t.Fatalf("since %s, deliveries %v", w.since, h.sink.ids())
	}
	expectLog("Journal.Pending", "events: reading the pending rows")
	if !w.since.Equal(h.c.Now()) {
		t.Fatalf("since %s, want the clock %s", w.since, h.c.Now())
	}
	h.sink.failFirst(2, http.StatusBadGateway, "")
	expectLog("Journal.Defer", "events: deferring a failed delivery")
	if rows := h.rows(t, "key_a"); rows[0].Attempts != 0 {
		t.Fatal("a failed Defer wrote attempts")
	}
	expectLog("Journal.Since", "events: counting the pending rows")
	if rows := h.rows(t, "key_a"); rows[0].Attempts != 1 || rows[0].NextAttemptAt.Sub(h.c.Now()) != time.Minute {
		t.Fatalf("the policy's delay: %+v", rows[0])
	}
	h.sink.failFirst(1, http.StatusBadGateway, "")
	h.c.advance(time.Hour)
	expectLog("Journal.Drop", "events: dropping an event")
	if len(h.rows(t, "key_a")) != 1 {
		t.Fatal("a failed Drop removed the row")
	}
	// A row from before the sink whose acknowledgement the journal
	// refuses stays for the next tick.
	h.appendAt(t, "evt_b0", "key_b", w.since.Add(-time.Minute))
	expectLog("Journal.Acknowledge", "acknowledging a row from before the sink was set")
	if rows := h.rows(t, "key_b"); !rows[0].AckedAt.IsZero() {
		t.Fatal("acknowledged through a refusing journal")
	}
	w.release(ctx)

	// Two holders writing the moment at once: the one that reads a sum
	// back withdraws its own value and takes the other's, so both agree
	// on the first; a reader that meets the sum in between delivers
	// nothing that tick.
	blind := h.worker("blind", &broken{h.st, map[string]bool{"Counters.ReadBlind": true}}, nil)
	tick(ctx, blind)
	if !blind.since.Equal(w.since) {
		t.Fatalf("the loser took %s, want the winner's %s", blind.since, w.since)
	}
	if have, _ := h.st.Counters().Read(ctx, []string{sinceKey}); have[sinceKey] != w.since.Unix() {
		t.Fatalf("the counter holds %d after the withdrawal, want %d", have[sinceKey], w.since.Unix())
	}
	blind.release(ctx)
	withdrawing := h.worker("withdrawing", &broken{h.st, map[string]bool{"Counters.ReadBlind": true, "Counters.AddNegative": true}}, nil)
	before := len(h.log.String())
	tick(ctx, withdrawing)
	if !strings.Contains(h.log.String()[before:], "withdrawing this holder's moment") || !withdrawing.since.IsZero() {
		t.Fatalf("since %s\n%s", withdrawing.since, h.log.String()[before:])
	}
	if _, err := h.st.Counters().Add(ctx, sinceKey, -withdrawing.o.Now().UTC().Truncate(time.Second).Unix(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	withdrawing.release(ctx)
	const far = int64(1) << 40
	if _, err := h.st.Counters().Add(ctx, sinceKey, far, time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.append(t, "evt_c1", "key_c")
	reader := h.worker("reader", nil, nil)
	before = len(h.log.String())
	tick(ctx, reader)
	if !strings.Contains(h.log.String()[before:], "is not settled") || !reader.since.IsZero() || slices.Contains(h.sink.ids(), "evt_c1") {
		t.Fatalf("an unsettled moment: since %s, deliveries %v\n%s", reader.since, h.sink.ids(), h.log.String()[before:])
	}
	if _, err := h.st.Counters().Add(ctx, sinceKey, -far, time.Time{}); err != nil {
		t.Fatal(err)
	}
	reader.Tick(ctx)
	if !reader.since.Equal(w.since) || !slices.Contains(h.sink.ids(), "evt_c1") {
		t.Fatalf("after the row settled: since %s, deliveries %v", reader.since, h.sink.ids())
	}
	reader.release(ctx)

	fail["Leases.Release"] = true
	before = len(h.log.String())
	brokenRelease := h.worker("r", st, nil)
	brokenRelease.acquire(ctx)
	brokenRelease.release(ctx)
	if !strings.Contains(h.log.String()[before:], "events: releasing the lease") {
		t.Fatal("a failed release was not logged")
	}
}

// TestRegisterIdleReadsZero: a process with no sink still exposes
// MetricPending, at zero, so the metric table holds without delivery.
func TestRegisterIdleReadsZero(t *testing.T) {
	reg := metrics.NewRegistry()
	RegisterIdle(reg)
	var out strings.Builder
	reg.WritePrometheus(&out)
	if !strings.Contains(out.String(), MetricPending+" 0") {
		t.Fatalf("registry lacks %s at zero:\n%s", MetricPending, out.String())
	}
}
