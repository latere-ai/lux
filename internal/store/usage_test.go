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
// record's At and id, carries the range it was minted under, decodes
// only under the query it was minted for, reads the same query however
// its lists are ordered, and refuses a cursor of another kind, another
// query, or another shape.
func TestRecordCursorBindsTheQuery(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 0, 0, 123456789, time.UTC)
	q := metering.RecordQuery{From: at.Add(-time.Hour), To: at.Add(time.Hour), Keys: []string{"key_b", "key_a"}, Labels: map[string]string{"team": "red"}, Error: "upstream_error"}
	cursor := store.EncodeRecordCursor(q, metering.Record{ID: "req_1", At: at})
	gotAt, id, err := store.DecodeRecordCursor(cursor, q)
	if err != nil || !gotAt.Equal(at) || id != "req_1" {
		t.Fatalf("Decode = %s, %s, %v", gotAt, id, err)
	}
	if from, to, ok := store.QueryCursorWindow(cursor); !ok || !from.Equal(q.From) || !to.Equal(q.To) {
		t.Fatalf("QueryCursorWindow = %s, %s, %v", from, to, ok)
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
		"another kind":    {store.EncodeQueryCursor("archive", q, "1|x"), q, "cursor is over archive"},
		"at not a number": {malformed(cursor, 4, "soon|req_1"), q, "at \"soon\" is not a number"},
		"no id":           {malformed(cursor, 4, "123"), q, "not an at and an id"},
		"a bad range end": {malformed(cursor, 3, "later"), q, "range end"},
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

// TestQueryCursorWindow: the range of a query cursor reads back
// whatever its kind, at the nanosecond, and a cursor that does not
// decode, has too few fields, or carries a range end that is not a count
// has none.
func TestQueryCursorWindow(t *testing.T) {
	from := time.Date(2026, 7, 5, 9, 30, 0, 1, time.UTC)
	q := metering.RecordQuery{From: from, To: from.Add(89 * 24 * time.Hour)}
	cursor := store.EncodeQueryCursor("archive", q, "a|b|c")
	if gotFrom, gotTo, ok := store.QueryCursorWindow(cursor); !ok || !gotFrom.Equal(q.From) || !gotTo.Equal(q.To) {
		t.Fatalf("QueryCursorWindow = %s, %s, %v", gotFrom, gotTo, ok)
	}
	if position, err := store.DecodeQueryCursor(cursor, "archive", q); err != nil || position != "a|b|c" {
		t.Fatalf("DecodeQueryCursor = %q, %v", position, err)
	}
	if _, err := store.DecodeQueryCursor(cursor, store.RecordsKind, q); !errors.Is(err, store.ErrInvalidCursor) {
		t.Fatalf("another kind: %v", err)
	}
	for name, c := range map[string]string{
		"not base64url": "not base64url!",
		"few fields":    base64.RawURLEncoding.EncodeToString([]byte("archive|00000000|1|2")),
		"a bad from":    malformed(cursor, 2, "early"),
		"a bad to":      malformed(cursor, 3, "late"),
	} {
		if _, _, ok := store.QueryCursorWindow(c); ok {
			t.Errorf("%s: a range was read", name)
		}
	}
}

// malformed takes a minted query cursor and puts value in place of its
// field i: 2 and 3 are the range, 4 the position.
func malformed(cursor string, i int, value string) string {
	raw, _ := base64.RawURLEncoding.DecodeString(cursor)
	parts := strings.SplitN(string(raw), "|", 5)
	parts[i] = value
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "|")))
}
