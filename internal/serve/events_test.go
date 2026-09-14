// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

func now() time.Time { return time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC) }

// TestAppendEventWritesTheRecord is spec 012's record as the API writes
// it: the envelope with subject, reason, and request id, the object
// block with labels never null, data never null, the id minted when
// empty and kept when given; an event with no object or type is an
// error and no row.
func TestAppendEventWritesTheRecord(t *testing.T) {
	st := memory.New()
	ctx := t.Context()
	k := &v1.Key{Metadata: v1.ObjectMeta{Name: "run-42"}, Status: v1.KeyStatus{ID: v1.NewID(v1.PrefixKey, now(), nil), Owner: subject}}
	if err := AppendEvent(ctx, st.Journal(), Event{Type: "key.created", At: now(), Subject: subject, Reason: ReasonRequest, RequestID: "req_1", Object: k}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(ctx, st.Journal(), Event{ID: "evt_given", Type: "key.deleted", At: now(), Reason: ReasonRequest, Object: k, Data: map[string]any{"prefix": "lux_ab12cd34"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Journal().Since(ctx, 0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %d, %v", len(rows), err)
	}
	var rec eventRecord
	if err := json.Unmarshal(rows[0].Payload, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Type != "key.created" || rec.Subject != subject || rec.Reason != "request" || rec.RequestID != "req_1" || rec.ID != rows[0].ID || rec.Object.ID != k.Status.ID || rec.Object.Name != "run-42" || rec.Object.Owner != subject {
		t.Errorf("record %+v", rec)
	}
	if string(rows[0].Payload) == "" || !json.Valid(rows[0].Payload) {
		t.Error("the payload is not JSON")
	}
	var raw map[string]any
	_ = json.Unmarshal(rows[0].Payload, &raw)
	if raw["data"] == nil || raw["object"].(map[string]any)["labels"] == nil {
		t.Errorf("data or labels is null: %s", rows[0].Payload)
	}
	if rows[1].ID != "evt_given" || rows[1].Type != "key.deleted" {
		t.Errorf("the given id was not kept: %+v", rows[1])
	}
	if err := AppendEvent(ctx, st.Journal(), Event{Type: "key.created", At: now()}); err == nil {
		t.Error("an event without an object was journalled")
	}
	if err := AppendEvent(ctx, st.Journal(), Event{Object: k, At: now()}); err == nil {
		t.Error("an event without a type was journalled")
	}
	if err := AppendEvent(ctx, st.Journal(), Event{ID: "evt_given", Type: "key.deleted", At: now(), Object: k}); err == nil {
		t.Error("a repeated id was journalled")
	}
}

func TestDiscardRecorderKeepsNothing(t *testing.T) {
	var r gateway.Recorder = DiscardRecorder{}
	r.Record(gateway.Record{ID: "req_1"})
}
