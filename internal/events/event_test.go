// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestAppendWritesTheRecord is spec 012's record as a writer journals
// it: the envelope with subject, reason, and request id, the object
// block with labels never null, data never null, the id minted when
// empty and kept when given; an event with no object or type is an
// error and no row.
func TestAppendWritesTheRecord(t *testing.T) {
	c := newClock()
	st := memory.New(memory.WithClock(c.Now))
	ctx := t.Context()
	const subject = "https://login.example.com|alice"
	k := &v1.Key{Metadata: v1.ObjectMeta{Name: "run-42"}, Status: v1.KeyStatus{ID: v1.NewID(v1.PrefixKey, c.Now(), nil), Owner: subject}}
	if err := Append(ctx, st.Journal(), Event{Type: KeyCreated, At: c.Now(), Subject: subject, Reason: ReasonRequest, RequestID: "req_1", Object: k}); err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, st.Journal(), Event{ID: "evt_given", Type: KeyDeleted, At: c.Now(), Reason: ReasonRequest, Object: k, Data: map[string]any{"prefix": "lux_ab12cd34"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Journal().Since(ctx, 0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %d, %v", len(rows), err)
	}
	var rec Record
	if err := json.Unmarshal(rows[0].Payload, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Type != KeyCreated || rec.Subject != subject || rec.Reason != "request" || rec.RequestID != "req_1" || rec.ID != rows[0].ID || !strings.HasPrefix(rec.ID, "evt_") || rec.Object.Kind != v1.KindKey || rec.Object.ID != k.Status.ID || rec.Object.Name != "run-42" || rec.Object.Owner != subject {
		t.Errorf("record %+v", rec)
	}
	var raw map[string]any
	if err := json.Unmarshal(rows[0].Payload, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["data"] == nil || raw["object"].(map[string]any)["labels"] == nil {
		t.Errorf("data or labels is null: %s", rows[0].Payload)
	}
	if rows[1].ID != "evt_given" || rows[1].Type != KeyDeleted || rows[1].ObjectID != k.Status.ID {
		t.Errorf("the given id was not kept: %+v", rows[1])
	}
	for name, e := range map[string]Event{
		"no object":     {Type: KeyCreated, At: c.Now()},
		"no type":       {Object: k, At: c.Now()},
		"a repeated id": {ID: "evt_given", Type: KeyDeleted, At: c.Now(), Object: k},
		"unencodable":   {Type: KeyDeleted, At: c.Now(), Object: k, Data: map[string]any{"ch": make(chan int)}},
	} {
		if err := Append(ctx, st.Journal(), e); err == nil {
			t.Errorf("%s was journalled", name)
		}
	}
	if rows, _ := st.Journal().Since(ctx, 0, 10); len(rows) != 2 {
		t.Errorf("%d rows after the refused events, want 2", len(rows))
	}
	// The labels of every kind, and none for anything else.
	if labelsOf(&v1.Key{}) != nil || labelsOf(&v1.Provider{Metadata: v1.ObjectMeta{Labels: map[string]string{"a": "b"}}})["a"] != "b" || labelsOf(&v1.Model{Metadata: v1.ObjectMeta{Labels: map[string]string{"a": "b"}}})["a"] != "b" || labelsOf(&v1.Budget{Metadata: v1.ObjectMeta{Labels: map[string]string{"a": "b"}}})["a"] != "b" {
		t.Error("labelsOf")
	}
}

// TestTableNamesEveryType: every type constant has a row, every row's
// reason is one of the five, the four server-raised rows and check.ping
// carry no request reason, and the members of each row are named once.
func TestTableNamesEveryType(t *testing.T) {
	types := []string{ProviderCreated, ProviderUpdated, ProviderDeleted, ProviderUnreachable, ProviderHealthy, ModelCreated, ModelUpdated, ModelDeleted, ModelDiscovered, ModelRemoved, KeyCreated, KeyUpdated, KeyRotated, KeyDeleted, KeyExhausted, BudgetCreated, BudgetUpdated, BudgetDeleted, BudgetExhausted, CheckPing}
	if len(Table) != len(types) {
		t.Fatalf("%d rows for %d types", len(Table), len(types))
	}
	reasons := []string{ReasonRequest, ReasonDiscovery, ReasonProbe, ReasonLimit, ReasonCheck}
	for _, typ := range types {
		row, ok := Table[typ]
		if !ok {
			t.Errorf("%s has no row", typ)
			continue
		}
		if !slices.Contains(reasons, row.Reason) {
			t.Errorf("%s has reason %q", typ, row.Reason)
		}
		all := append(slices.Clone(row.Members), row.Optional...)
		slices.Sort(all)
		if len(slices.Compact(all)) != len(row.Members)+len(row.Optional) {
			t.Errorf("%s names a member twice: %v", typ, row)
		}
	}
	for _, typ := range []string{ProviderUnreachable, ProviderHealthy, ModelDiscovered, ModelRemoved, KeyExhausted, BudgetExhausted, CheckPing} {
		if Table[typ].Reason == ReasonRequest {
			t.Errorf("%s is server-raised and has reason request", typ)
		}
	}
}
