// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestDeliveryDeadline: each POST has a 10 second deadline by default,
// and a sink that hangs is abandoned at the deadline with the error
// naming it, in well under the time the sink would have held the
// worker.
func TestDeliveryDeadline(t *testing.T) {
	if DeliveryDeadline != 10*time.Second {
		t.Fatalf("DeliveryDeadline = %s", DeliveryDeadline)
	}
	if s := NewSink(SinkOptions{URL: "http://127.0.0.1:1", Secret: secret}); s.o.Deadline != DeliveryDeadline || s.o.Now == nil || s.o.Client == nil {
		t.Fatalf("defaults = %+v", s.o)
	}
	h := newHarness(t)
	h.sink.setPlan(func(int, []byte) (int, bool) { return 0, true })
	s := NewSink(SinkOptions{URL: h.sink.srv.URL, Secret: secret, Client: h.sink.srv.Client(), Deadline: 50 * time.Millisecond, Now: h.c.Now})
	start := time.Now()
	err := s.Deliver(t.Context(), []byte(`{}`))
	var de *DeliveryError
	if !errors.As(err, &de) || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "POST: ") {
		t.Fatalf("a hanging sink: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the POST was abandoned after %s", elapsed)
	}
	if n := len(h.sink.deliveries()); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
}

// TestSinkAcknowledgesTwoHundredsAlone: a 2xx acknowledges; a 4xx, a
// 5xx, and a redirect, which is never followed, are failures naming
// the status, and so is a URL nothing listens at.
func TestSinkAcknowledgesTwoHundredsAlone(t *testing.T) {
	var status atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		} else {
			w.WriteHeader(code)
		}
	}))
	defer target.Close()
	s := NewSink(SinkOptions{URL: target.URL, Secret: secret, Client: target.Client()})
	for _, code := range []int{200, 201, 202, 204, 299} {
		status.Store(int32(code))
		if err := s.Deliver(t.Context(), []byte(`{}`)); err != nil {
			t.Errorf("%d: %v", code, err)
		}
	}
	for _, code := range []int{302, 400, 401, 404, 500, 503} {
		status.Store(int32(code))
		err := s.Deliver(t.Context(), []byte(`{}`))
		var de *DeliveryError
		if !errors.As(err, &de) || de.Status != code || err.Error() != "status "+strconv.Itoa(code) {
			t.Errorf("%d: %v", code, err)
		}
	}
	// The caller's client keeps its own redirect policy.
	if target.Client().CheckRedirect != nil {
		t.Error("the caller's client was changed")
	}
	dead := NewSink(SinkOptions{URL: "http://127.0.0.1:1/events", Secret: secret, Deadline: time.Second})
	var de *DeliveryError
	if err := dead.Deliver(t.Context(), []byte(`{}`)); !errors.As(err, &de) || de.Err == nil {
		t.Errorf("nothing listening: %v", err)
	}
	bad := NewSink(SinkOptions{URL: "http://[::1]:namedport", Secret: secret})
	if err := bad.Deliver(t.Context(), []byte(`{}`)); !errors.As(err, &de) || de.Err == nil {
		t.Errorf("a URL that does not build a request: %v", err)
	}
}

// TestPing is the check.ping row: the sink receives one signed body of
// type check.ping with reason check, an empty object block whose
// labels are not null, empty data, and no request id; nothing reaches
// the journal.
func TestPing(t *testing.T) {
	h := newHarness(t)
	s := NewSink(SinkOptions{URL: h.sink.srv.URL, Secret: secret, Client: h.sink.srv.Client(), Now: h.c.Now})
	if err := s.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := h.sink.deliveries()
	if len(got) != 1 {
		t.Fatalf("%d deliveries", len(got))
	}
	h.sink.verified(t)
	var raw map[string]any
	if err := json.Unmarshal(got[0].body, &raw); err != nil {
		t.Fatal(err)
	}
	obj, _ := raw["object"].(map[string]any)
	if raw["type"] != CheckPing || raw["reason"] != ReasonCheck || raw["request_id"] != "" || raw["subject"] != "" || !strings.HasPrefix(raw["id"].(string), "evt_") || obj["kind"] != "" || obj["labels"] == nil || len(raw["data"].(map[string]any)) != 0 {
		t.Fatalf("ping body %s", got[0].body)
	}
	if rows, _ := h.st.Journal().Since(t.Context(), 0, 0); len(rows) != 0 {
		t.Fatal("a ping was journalled")
	}
	h.sink.failFirst(1, http.StatusBadGateway, "")
	if err := s.Ping(t.Context()); err == nil {
		t.Fatal("a refused ping passed")
	}
}
