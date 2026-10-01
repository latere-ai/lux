// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/lux/metering"
)

// EncodeQueryCursor renders a cursor over records under q: base64url,
// without padding, of "<kind>|<digest>|<from>|<to>|<position>", the
// digest over the query's canonical JSON, the range as Unix
// nanoseconds, and the position in the kind's own spelling. The range
// rides in the cursor because an end the caller leaves open is filled
// from the clock, so a follow-up page reads the range the first page
// resolved (QueryCursorWindow) instead of one the clock has moved.
// Every record source uses this one framing.
func EncodeQueryCursor(kind string, q metering.RecordQuery, position string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + recordDigest(q) + "|" + strconv.FormatInt(q.From.UnixNano(), 10) + "|" + strconv.FormatInt(q.To.UnixNano(), 10) + "|" + position))
}

// DecodeQueryCursor checks cursor against the kind and query of the
// request and returns its position. A cursor that does not decode, or
// whose kind or query differs, is ErrInvalidCursor with the detail.
func DecodeQueryCursor(cursor, kind string, q metering.RecordQuery) (position string, err error) {
	parts, err := queryCursorParts(cursor)
	if err != nil {
		return "", err
	}
	if parts[0] != kind {
		return "", fmt.Errorf("%w: cursor is over %s, the request over %s", ErrInvalidCursor, parts[0], kind)
	}
	if parts[1] != recordDigest(q) {
		return "", fmt.Errorf("%w: cursor query %s, the request's %s", ErrInvalidCursor, parts[1], recordDigest(q))
	}
	return parts[4], nil
}

// QueryCursorWindow is the range a query cursor was issued for, of any
// kind, and false when the cursor does not decode; the cursor's kind and
// query are checked by the source that reads it.
func QueryCursorWindow(cursor string) (from, to time.Time, ok bool) {
	parts, err := queryCursorParts(cursor)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return unixNano(parts[2]), unixNano(parts[3]), true
}

// queryCursorParts splits a query cursor into its kind, digest, from,
// to, and position, the range checked as two Unix nanosecond counts.
func queryCursorParts(cursor string) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64url: %w", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 5)
	if len(parts) != 5 {
		return nil, fmt.Errorf("%w: %d fields, want kind, query, from, to, and position", ErrInvalidCursor, len(parts))
	}
	for _, p := range parts[2:4] {
		if _, err := strconv.ParseInt(p, 10, 64); err != nil {
			return nil, fmt.Errorf("%w: range end %q is not a number", ErrInvalidCursor, p)
		}
	}
	return parts, nil
}

// unixNano reads a count queryCursorParts has already checked.
func unixNano(s string) time.Time {
	n, _ := strconv.ParseInt(s, 10, 64)
	return time.Unix(0, n).UTC()
}

// EncodeRecordCursor renders the cursor that resumes Records under q
// after last: EncodeQueryCursor's framing, kind records, the position
// the record's At as Unix nanoseconds beside its id, because the order
// is At then id and two records may share an instant.
func EncodeRecordCursor(q metering.RecordQuery, last metering.Record) string {
	return EncodeQueryCursor(RecordsKind, q, strconv.FormatInt(last.At.UnixNano(), 10)+"|"+last.ID)
}

// DecodeRecordCursor checks cursor against q and returns the At and id
// to resume after. A cursor that does not decode, or is over another
// kind or query, is ErrInvalidCursor with the detail.
func DecodeRecordCursor(cursor string, q metering.RecordQuery) (at time.Time, id string, err error) {
	position, err := DecodeQueryCursor(cursor, RecordsKind, q)
	if err != nil {
		return time.Time{}, "", err
	}
	nanos, id, ok := strings.Cut(position, "|")
	if !ok {
		return time.Time{}, "", fmt.Errorf("%w: position %q is not an at and an id", ErrInvalidCursor, position)
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: at %q is not a number", ErrInvalidCursor, nanos)
	}
	return time.Unix(0, n).UTC(), id, nil
}

// recordDigest is the 8 hex characters of SHA-256 over the query's
// canonical JSON: members in a fixed order, lists sorted, labels by
// sorted key, so two queries that select the same records encode the
// same bytes.
func recordDigest(q metering.RecordQuery) string {
	sorted := func(s []string) []string {
		out := slices.Clone(s)
		slices.Sort(out)
		return out
	}
	data, err := json.Marshal(struct {
		From      time.Time         `json:"from"`
		To        time.Time         `json:"to"`
		Keys      []string          `json:"keys,omitempty"`
		Models    []string          `json:"models,omitempty"`
		Providers []string          `json:"providers,omitempty"`
		Owners    []string          `json:"owners,omitempty"`
		Labels    map[string]string `json:"labels,omitempty"`
		Status    metering.Status   `json:"status,omitempty"`
		Error     string            `json:"error,omitempty"`
		Stream    *bool             `json:"stream,omitempty"`
	}{q.From.UTC(), q.To.UTC(), sorted(q.Keys), sorted(q.Models), sorted(q.Providers), sorted(q.Owners), q.Labels, q.Status, q.Error, q.Stream})
	if err != nil {
		panic("store: a RecordQuery of strings and times does not encode: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:4])
}

// NewerFirst orders records newest first: by At descending, then by id
// descending, which is the order Records answers and a cursor resumes.
func NewerFirst(a, b metering.Record) int {
	if c := b.At.Compare(a.At); c != 0 {
		return c
	}
	return strings.Compare(b.ID, a.ID)
}
