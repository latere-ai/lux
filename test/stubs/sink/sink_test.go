// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/events"
)

// clock is a fixed instant the sink and the client both read.
var clock = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func now() time.Time { return clock }

// start serves one sink for the test and returns it with the client
// spec 012's worker delivers through, pointed at it.
func start(t *testing.T, o Options) (*httptest.Server, *Sink, *events.Sink) {
	t.Helper()
	if o.Now == nil {
		o.Now = now
	}
	s := New(o)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	client := events.NewSink(events.SinkOptions{URL: srv.URL, Secret: []byte(DefaultSecret), Client: &http.Client{}, Now: now})
	return srv, s, client
}

func event(id string) []byte {
	return []byte(`{"id":"` + id + `","type":"key.created","object":{"kind":"Key","id":"key_1"}}`)
}

// get reads a control route.
func get(t *testing.T, url string, v any) int {
	t.Helper()
	return call(t, http.MethodGet, url, "", v)
}

func call(t *testing.T, method, url, body string, v any) int {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if v != nil && len(data) > 0 {
		if err := json.Unmarshal(data, v); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, data)
		}
	}
	return resp.StatusCode
}

// TestSinkStub drives every route and the -fail-first flag: a verified
// delivery is stored once per id with its attempts, the first n
// deliveries are refused with 500 and counted, DELETE clears, and PUT
// /_fail arms the outage again.
func TestSinkStub(t *testing.T) {
	srv, s, client := start(t, Options{FailFirst: 2})
	ctx := t.Context()
	var derr *events.DeliveryError
	for i := range 2 {
		err := client.Deliver(ctx, event("evt_1"))
		if !errors.As(err, &derr) || derr.Status != http.StatusInternalServerError {
			t.Fatalf("delivery %d during the outage: %v", i+1, err)
		}
	}
	if err := client.Deliver(ctx, event("evt_1")); err != nil {
		t.Fatalf("the third delivery: %v", err)
	}
	if err := client.Deliver(ctx, event("evt_2")); err != nil {
		t.Fatalf("a second event: %v", err)
	}
	// A duplicate of an acknowledged id is acknowledged again and stored
	// once, as a sink that deduplicates on id would.
	if err := client.Deliver(ctx, event("evt_1")); err != nil {
		t.Fatalf("the duplicate: %v", err)
	}
	var got []Event
	if code := get(t, srv.URL+"/_events", &got); code != http.StatusOK {
		t.Fatalf("GET /_events = %d", code)
	}
	if len(got) != 2 || got[0].ID != "evt_1" || got[0].Attempts != 4 || got[1].ID != "evt_2" || got[1].Attempts != 1 {
		t.Fatalf("events = %+v", got)
	}
	if !got[0].ReceivedAt.Equal(clock) || string(got[0].Body) != string(event("evt_1")) {
		t.Fatalf("the first event = %+v", got[0])
	}
	if len(s.Events()) != 2 {
		t.Fatalf("Events() = %d", len(s.Events()))
	}

	if code := call(t, http.MethodDelete, srv.URL+"/_events", "", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE /_events = %d", code)
	}
	got = nil
	get(t, srv.URL+"/_events", &got)
	if len(got) != 0 {
		t.Fatalf("after DELETE: %+v", got)
	}
	var rejected []Rejected
	get(t, srv.URL+"/_events?invalid=1", &rejected)
	if len(rejected) != 0 {
		t.Fatalf("after DELETE, rejected = %+v", rejected)
	}

	if code := call(t, http.MethodPut, srv.URL+"/_fail", `{"first":1}`, nil); code != http.StatusNoContent {
		t.Fatalf("PUT /_fail = %d", code)
	}
	if err := client.Deliver(ctx, event("evt_3")); !errors.As(err, &derr) || derr.Status != http.StatusInternalServerError {
		t.Fatalf("after PUT /_fail: %v", err)
	}
	if err := client.Deliver(ctx, event("evt_3")); err != nil {
		t.Fatalf("after the armed refusal: %v", err)
	}
	if got := s.Events(); len(got) != 1 || got[0].Attempts != 2 {
		t.Fatalf("events after the second outage = %+v", got)
	}
	if code := call(t, http.MethodPut, srv.URL+"/_fail", `{"first":-1}`, nil); code != http.StatusBadRequest {
		t.Fatalf("PUT /_fail with a negative count = %d", code)
	}
	if code := call(t, http.MethodPut, srv.URL+"/_fail", `nope`, nil); code != http.StatusBadRequest {
		t.Fatalf("PUT /_fail with no JSON = %d", code)
	}
	if code := call(t, http.MethodGet, srv.URL+"/", "", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET / = %d", code)
	}
	if code := call(t, http.MethodPost, srv.URL+"/_nothing", "", nil); code != http.StatusNotFound {
		t.Fatalf("an unknown control route = %d", code)
	}
	// A delivery on a path: the sink URL an operator sets may carry one.
	deep := events.NewSink(events.SinkOptions{URL: srv.URL + "/hooks/lux", Secret: []byte(DefaultSecret), Client: &http.Client{}, Now: now})
	if err := deep.Deliver(ctx, event("evt_4")); err != nil {
		t.Fatalf("a delivery under a path: %v", err)
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("check.ping: %v", err)
	}
}

// TestSinkVerifiesSignature: a body whose signature does not verify, a
// stale t, a missing header, and a verified body that is no event are
// each 400 and recorded apart at GET /_events?invalid=1, never among the
// events.
func TestSinkVerifiesSignature(t *testing.T) {
	srv, s, client := start(t, Options{})
	ctx := t.Context()
	wrong := events.NewSink(events.SinkOptions{URL: srv.URL, Secret: []byte("another-secret"), Client: &http.Client{}, Now: now})
	var derr *events.DeliveryError
	if err := wrong.Deliver(ctx, event("evt_1")); !errors.As(err, &derr) || derr.Status != http.StatusBadRequest {
		t.Fatalf("a wrong secret: %v", err)
	}
	stale := events.NewSink(events.SinkOptions{URL: srv.URL, Secret: []byte(DefaultSecret), Client: &http.Client{}, Now: func() time.Time { return clock.Add(-Skew - time.Second) }})
	if err := stale.Deliver(ctx, event("evt_1")); !errors.As(err, &derr) || derr.Status != http.StatusBadRequest {
		t.Fatalf("a stale t: %v", err)
	}
	if code := call(t, http.MethodPost, srv.URL+"/", string(event("evt_1")), nil); code != http.StatusBadRequest {
		t.Fatalf("no header = %d", code)
	}
	tampered := event("evt_1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/", strings.NewReader(string(tampered)+" "))
	req.Header.Set(events.Header, events.Sign([]byte(DefaultSecret), clock, tampered))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a tampered body = %d", resp.StatusCode)
	}
	if err := client.Deliver(ctx, []byte(`{"type":"no id"}`)); !errors.As(err, &derr) || derr.Status != http.StatusBadRequest {
		t.Fatalf("a verified body with no id: %v", err)
	}

	if got := s.Events(); len(got) != 0 {
		t.Fatalf("a rejected delivery was stored: %+v", got)
	}
	var rejected []Rejected
	if code := get(t, srv.URL+"/_events?invalid=1", &rejected); code != http.StatusOK || len(rejected) != 5 {
		t.Fatalf("GET /_events?invalid=1 = %d, %d rejected", code, len(rejected))
	}
	for i, want := range []string{"v1 does not match", "from the clock", "is not key=value", "v1 does not match", "not an event with an id"} {
		if !strings.Contains(rejected[i].Reason, want) || !rejected[i].ReceivedAt.Equal(clock) {
			t.Errorf("rejected[%d] = %+v, want a reason with %q", i, rejected[i], want)
		}
	}
	if rejected[0].Signature == "" || rejected[0].Body != string(event("evt_1")) {
		t.Fatalf("the rejection keeps neither the header nor the body: %+v", rejected[0])
	}
	if err := client.Deliver(ctx, event("evt_1")); err != nil {
		t.Fatalf("a good delivery after the bad ones: %v", err)
	}
	if got := s.Events(); len(got) != 1 || got[0].Attempts != 1 {
		t.Fatalf("events = %+v", got)
	}
}

// TestSinkDefaults: the zero Options verify with DefaultSecret against
// the wall clock.
func TestSinkDefaults(t *testing.T) {
	s := New(Options{})
	srv := httptest.NewServer(s)
	defer srv.Close()
	client := events.NewSink(events.SinkOptions{URL: srv.URL, Secret: []byte(DefaultSecret), Client: &http.Client{}})
	if err := client.Deliver(t.Context(), event("evt_1")); err != nil {
		t.Fatal(err)
	}
	if got := s.Events(); len(got) != 1 || time.Since(got[0].ReceivedAt) > time.Minute {
		t.Fatalf("events = %+v", got)
	}
	if got := s.RejectedDeliveries(); len(got) != 0 {
		t.Fatalf("rejected = %+v", got)
	}
}
