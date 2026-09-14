// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"sync"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// archive is a RecordSink that keeps what it is handed.
type archive struct {
	mu   sync.Mutex
	recs []metering.Record
}

func (a *archive) Append(r metering.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs = append(a.recs, r)
}

// TestRecorderFansOutToTheArchive is spec 012's seam on the Recorder:
// every record reaches the archive once, priced as the ring's copy is,
// and a Recorder without an archive hands nothing anywhere.
func TestRecorderFansOutToTheArchive(t *testing.T) {
	h := newHarness(t)
	sink := &archive{}
	r := NewRecorder(RecorderOptions{Store: h.st, Archive: sink, Now: h.clock, Logger: h.logger})
	rec := gateway.Record{ID: "req_1", At: h.clock(), EndedAt: h.clock().Add(time.Second), KeyID: "key_1", KeyPrefix: "lux_ab12cd34ef56", Model: "gpt", Door: "openai", Route: "/openai/v1/chat/completions", Status: gateway.StatusOK, Tokens: gateway.Tokens{Input: 3, Output: 4}}
	r.Record(rec)
	r.Record(gateway.Record{ID: "req_2", At: h.clock(), KeyID: "key_1", Status: gateway.StatusRefused, Error: "rate_limited"})
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.recs) != 2 || sink.recs[0].ID != "req_1" || sink.recs[1].ID != "req_2" || sink.recs[0].Tokens.Input != 3 || sink.recs[0].Labels == nil {
		t.Fatalf("archive got %+v", sink.recs)
	}
	ring, _, err := h.st.Usage().Records(t.Context(), metering.RecordQuery{Query: metering.Query{From: h.clock().Add(-time.Hour), To: h.clock().Add(time.Hour)}}, store.Page{})
	if err != nil || len(ring) != 2 || ring[1].ID != sink.recs[0].ID || ring[1].Cost != sink.recs[0].Cost {
		t.Fatalf("the ring holds %+v, %v", ring, err)
	}
	without := NewRecorder(RecorderOptions{Store: h.st, Now: h.clock, Logger: h.logger})
	without.Record(rec)
	if len(sink.recs) != 2 {
		t.Fatal("a Recorder without an archive reached one")
	}
}
