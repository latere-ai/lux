// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package sink is the stub event sink of spec 015: the contract of spec
// 012 as an http.Handler. It recomputes the Lux-Signature over
// "<t>.<body>" with events.Verify, which compares in constant time and
// refuses a t more than five minutes from its clock, stores what
// verified, and answers a body that does not verify with 400 while
// recording it apart, so a signature test asserts a rejection rather
// than an absence.
//
// The control routes sit under /_: GET /_events returns the verified
// deliveries in receipt order with their delivery attempt counts, GET
// /_events?invalid=1 the rejected ones, DELETE /_events clears both, and
// PUT /_fail {"first": n} makes the next n verified deliveries answer
// 500, which is what lux-stubs -fail-first sets at start and what drives
// the ordering and resume criteria of spec 012.
package sink

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/lux/internal/events"
)

// The sink's fixed values.
const (
	// DefaultSecret is the LUX_EVENTS_SECRET the sink verifies with when
	// Options names none, and the value make run hands luxd.
	DefaultSecret = "stub-sink-secret"
	// Skew is how far a delivery's t may sit from the clock, spec 012's
	// five minutes.
	Skew = 5 * time.Minute
	// maxBody bounds one delivery.
	maxBody = 1 << 20
	// controlPrefix is where the control routes live.
	controlPrefix = "/_"
	eventsPath    = "/_events"
	failPath      = "/_fail"
)

// Options is what New builds a sink under.
type Options struct {
	// Secret is the HMAC key; empty is DefaultSecret.
	Secret []byte
	// FailFirst is how many verified deliveries answer 500 before the
	// first is stored: -fail-first on lux-stubs.
	FailFirst int
	// Now is the clock the skew is measured against; nil is time.Now.
	Now func() time.Time
}

// Event is one verified delivery: the event's id, how many deliveries
// of that id arrived, the first one's clock, and the body as received.
type Event struct {
	ID         string          `json:"id"`
	Attempts   int             `json:"attempts"`
	ReceivedAt time.Time       `json:"receivedAt"`
	Body       json.RawMessage `json:"body"`
}

// Rejected is one delivery that did not verify: why, the header it
// carried, and the body as text.
type Rejected struct {
	Reason     string    `json:"reason"`
	Signature  string    `json:"signature"`
	ReceivedAt time.Time `json:"receivedAt"`
	Body       string    `json:"body"`
}

// Sink is one stub sink. It is an http.Handler.
type Sink struct {
	o Options

	mu       sync.Mutex
	failLeft int
	attempts map[string]int
	events   []Event
	rejected []Rejected
}

// New builds a sink.
func New(o Options) *Sink {
	if len(o.Secret) == 0 {
		o.Secret = []byte(DefaultSecret)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Sink{o: o, failLeft: o.FailFirst, attempts: map[string]int{}}
}

// Events lists the verified deliveries in receipt order, one per id,
// each with the attempts seen so far. An empty record is an empty list.
func (s *Sink) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.events))
	for _, e := range s.events {
		e.Attempts = s.attempts[e.ID]
		out = append(out, e)
	}
	return out
}

// RejectedDeliveries lists what did not verify, in receipt order.
func (s *Sink) RejectedDeliveries() []Rejected {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejected == nil {
		return []Rejected{}
	}
	return slices.Clone(s.rejected)
}

// Reset clears both records and the attempt counts; DELETE /_events is
// the same over HTTP. The outage counter is left alone.
func (s *Sink) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events, s.rejected, s.attempts = nil, nil, map[string]int{}
}

// FailFirst makes the next n verified deliveries answer 500; PUT /_fail
// is the same over HTTP.
func (s *Sink) FailFirst(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLeft = n
}

// ServeHTTP verifies a POST on any path outside /_ and serves the control
// routes under it.
func (s *Sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, controlPrefix) {
		s.control(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "a delivery is a POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the body could not be read: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := s.o.Now()
	header := r.Header.Get(events.Header)
	if err := events.Verify(s.o.Secret, header, body, now, Skew); err != nil {
		s.mu.Lock()
		s.rejected = append(s.rejected, Rejected{Reason: err.Error(), Signature: header, ReceivedAt: now, Body: string(body)})
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_signature", "message": "The delivery's signature does not verify.", "details": map[string]any{"detail": err.Error()}})
		return
	}
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.ID == "" {
		s.mu.Lock()
		s.rejected = append(s.rejected, Rejected{Reason: "the body is not an event with an id", Signature: header, ReceivedAt: now, Body: string(body)})
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_event", "message": "The delivery is not an event.", "details": map[string]any{"detail": "the body has no string id member"}})
		return
	}
	s.mu.Lock()
	s.attempts[envelope.ID]++
	if s.failLeft > 0 {
		s.failLeft--
		s.mu.Unlock()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": "stub_failing", "message": "The sink was asked to refuse this delivery.", "details": map[string]any{"detail": "-fail-first has deliveries left to refuse"}})
		return
	}
	if !slices.ContainsFunc(s.events, func(e Event) bool { return e.ID == envelope.ID }) {
		s.events = append(s.events, Event{ID: envelope.ID, ReceivedAt: now, Body: json.RawMessage(slices.Clone(body))})
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// control serves the routes under /_.
func (s *Sink) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == eventsPath && r.Method == http.MethodGet:
		if r.URL.Query().Get("invalid") == "1" {
			writeJSON(w, http.StatusOK, s.RejectedDeliveries())
			return
		}
		writeJSON(w, http.StatusOK, s.Events())
	case r.URL.Path == eventsPath && r.Method == http.MethodDelete:
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == failPath && r.Method == http.MethodPut:
		var body struct {
			First int `json:"first"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.First < 0 {
			http.Error(w, `the body is {"first": <n>} with n at least 0`, http.StatusBadRequest)
			return
		}
		s.FailFirst(body.First)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, r.Method+" "+r.URL.Path+" is no control route; GET and DELETE /_events and PUT /_fail are", http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
