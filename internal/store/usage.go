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

// EncodeRecordCursor renders the cursor that resumes Records under q
// after last: the cursor shape of EncodeCursor, kind records, the
// digest over the query's canonical JSON, and the record's At as Unix
// nanoseconds beside its id, because the order is At then id and two
// records may share an instant.
func EncodeRecordCursor(q metering.RecordQuery, last metering.Record) string {
	return base64.RawURLEncoding.EncodeToString([]byte(RecordsKind + "|" + recordDigest(q) + "|" + strconv.FormatInt(last.At.UnixNano(), 10) + "|" + last.ID))
}

// DecodeRecordCursor checks cursor against q and returns the At and id
// to resume after. A cursor that does not decode, or is over another
// kind or query, is ErrInvalidCursor with the detail.
func DecodeRecordCursor(cursor string, q metering.RecordQuery) (at time.Time, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: not base64url: %w", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 4)
	if len(parts) != 4 {
		return time.Time{}, "", fmt.Errorf("%w: %d fields, want kind, query, at, and id", ErrInvalidCursor, len(parts))
	}
	if parts[0] != RecordsKind {
		return time.Time{}, "", fmt.Errorf("%w: cursor is over %s, the request over %s", ErrInvalidCursor, parts[0], RecordsKind)
	}
	if parts[1] != recordDigest(q) {
		return time.Time{}, "", fmt.Errorf("%w: cursor query %s, the request's %s", ErrInvalidCursor, parts[1], recordDigest(q))
	}
	nanos, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: at %q is not a number", ErrInvalidCursor, parts[2])
	}
	return time.Unix(0, nanos).UTC(), parts[3], nil
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
