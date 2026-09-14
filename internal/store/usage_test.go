// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// TestRecordCursorBindsTheQuery: a record cursor resumes after the
// record's At and id, decodes only under the query it was minted for,
// reads the same query however its lists are ordered, and refuses a
// cursor of another kind, another query, or another shape.
func TestRecordCursorBindsTheQuery(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 0, 0, 123456789, time.UTC)
	q := metering.RecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour), Keys: []string{"key_b", "key_a"}, Labels: map[string]string{"team": "red"}, Error: "upstream_error"}
	cursor := store.EncodeRecordCursor(q, metering.Record{ID: "req_1", At: at})
	gotAt, id, err := store.DecodeRecordCursor(cursor, q)
	if err != nil || !gotAt.Equal(at) || id != "req_1" {
		t.Fatalf("Decode = %s, %s, %v", gotAt, id, err)
	}
	same := q
	same.Keys = []string{"key_a", "key_b"}
	if _, _, err := store.DecodeRecordCursor(cursor, same); err != nil {
		t.Fatalf("the same query with its keys reordered: %v", err)
	}
	other := q
	other.Status = metering.StatusFailed
	for name, c := range map[string]struct {
		cursor string
		q      metering.RecordQuery
		detail string
	}{
		"another query":   {cursor, other, "cursor query"},
		"not base64url":   {"not base64url!", q, "not base64url"},
		"two fields":      {store.EncodeCursor(store.RecordsKind, store.Filter{}, "x"), q, "fields"},
		"another kind":    {store.EncodeCursor(store.JournalKind, store.Filter{}, "1|x"), q, "cursor is over journal"},
		"at not a number": {malformed(cursor, "soon"), q, "not a number"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := store.DecodeRecordCursor(c.cursor, c.q)
			if !errors.Is(err, store.ErrInvalidCursor) || !strings.Contains(err.Error(), c.detail) {
				t.Fatalf("Decode = %v, want ErrInvalidCursor with %q", err, c.detail)
			}
		})
	}
	// NewerFirst orders by At descending then id descending.
	recs := []metering.Record{{ID: "req_a", At: at}, {ID: "req_c", At: at.Add(time.Second)}, {ID: "req_b", At: at}}
	slices.SortFunc(recs, store.NewerFirst)
	if recs[0].ID != "req_c" || recs[1].ID != "req_b" || recs[2].ID != "req_a" {
		t.Fatalf("NewerFirst = %s %s %s", recs[0].ID, recs[1].ID, recs[2].ID)
	}
}

// malformed takes a minted cursor and puts at in place of its At.
func malformed(cursor, at string) string {
	raw, _ := base64.RawURLEncoding.DecodeString(cursor)
	parts := strings.SplitN(string(raw), "|", 4)
	parts[2] = at
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "|")))
}
