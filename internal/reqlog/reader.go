// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/s3"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// CursorKind is the kind an archive cursor is encoded under.
const CursorKind = "archive"

// maxLine bounds one archived line; a record is a few kilobytes.
const maxLine = 4 << 20

// Reader answers GET /v1/requests from the archive: the durable,
// installation-wide record set, in place of one replica's ring.
type Reader struct {
	bucket Bucket
	prefix string
}

// NewReader constructs the reader over the bucket and LUX_S3_PREFIX.
func NewReader(b Bucket, prefix string) *Reader {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return &Reader{bucket: b, prefix: prefix}
}

// List pages the archived records that match q, the shape of
// store.Usage().Records so the route switches on the source: for each
// UTC hour prefix the range covers, newest hour first, the objects are
// listed with StartAfter paging and decoded a line at a time with the
// filters applied, in listing order, which is by replica and then write
// order within an hour. The cursor is the object key and the line to
// resume from; one from another query is store.ErrInvalidCursor. A
// Limit of 0 or less is every record.
func (r *Reader) List(ctx context.Context, q metering.RecordQuery, p store.Page) ([]metering.Record, string, error) {
	if q.From.IsZero() || q.To.IsZero() || !q.To.After(q.From) {
		return nil, "", errors.New("reqlog: the query needs a range with to after from")
	}
	hour := q.To.UTC().Add(-time.Nanosecond).Truncate(time.Hour)
	first := q.From.UTC().Truncate(time.Hour)
	var resumeKey string
	var resumeLine int
	if p.Cursor != "" {
		var err error
		if resumeKey, resumeLine, err = decodeCursor(p.Cursor, q); err != nil {
			return nil, "", err
		}
		at, err := hourOf(r.prefix, resumeKey)
		if err != nil {
			return nil, "", err
		}
		if at.After(hour) || at.Before(first) {
			return nil, "", fmt.Errorf("%w: key %s is outside the range %s to %s", store.ErrInvalidCursor, resumeKey, q.From.UTC().Format(time.RFC3339), q.To.UTC().Format(time.RFC3339))
		}
		hour = at
	}
	var out []metering.Record
	for ; !hour.Before(first); hour = hour.Add(-time.Hour) {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		prefix := path.Join(r.prefix, hourPrefix(hour)) + "/"
		after := ""
		for {
			res, err := r.bucket.ListObjects(ctx, s3.ListOptions{Prefix: prefix, StartAfter: after})
			if err != nil {
				return nil, "", fmt.Errorf("listing %s: %w", prefix, err)
			}
			for _, obj := range res.Objects {
				if resumeKey != "" && obj.Key < resumeKey {
					continue
				}
				from := 0
				if obj.Key == resumeKey {
					from = resumeLine
				}
				next, err := r.read(ctx, obj.Key, from, q, p.Limit, &out)
				if err != nil {
					return nil, "", err
				}
				if p.Limit > 0 && len(out) >= p.Limit {
					return out, encodeCursor(q, obj.Key, next), nil
				}
			}
			if !res.Truncated || len(res.Objects) == 0 {
				break
			}
			after = res.Objects[len(res.Objects)-1].Key
		}
		resumeKey = ""
	}
	return out, "", nil
}

// read decodes one object, skipping the first skip lines, appending the
// records that match q to out until limit are held when limit is
// positive, and returns the last line read, which is where a cursor
// resumes.
func (r *Reader) read(ctx context.Context, key string, skip int, q metering.RecordQuery, limit int, out *[]metering.Record) (last int, err error) {
	rc, _, err := r.bucket.GetObject(ctx, key, "")
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	line := 0
	for sc.Scan() {
		line++
		if line <= skip {
			continue
		}
		var rec metering.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return 0, fmt.Errorf("reading %s line %d: %w", key, line, err)
		}
		if !q.Matches(rec) {
			continue
		}
		*out = append(*out, rec)
		if limit > 0 && len(*out) >= limit {
			return line, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("reading %s after line %d: %w", key, line, err)
	}
	return line, nil
}

// hourOf reads the hour of an archive key back, so a cursor names where
// to resume; a key not under the prefix is store.ErrInvalidCursor.
func hourOf(prefix, key string) (time.Time, error) {
	rest, ok := strings.CutPrefix(key, strings.TrimSuffix(prefix, "/")+"/")
	if !ok {
		return time.Time{}, fmt.Errorf("%w: key %s is not under %s", store.ErrInvalidCursor, key, prefix)
	}
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) != 5 {
		return time.Time{}, fmt.Errorf("%w: key %s is not an hour and an object", store.ErrInvalidCursor, key)
	}
	at, err := time.Parse("2006/01/02/15", strings.Join(parts[:4], "/"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: key %s: %w", store.ErrInvalidCursor, key, err)
	}
	return at, nil
}

// encodeCursor renders the cursor that resumes q at line of key: the
// store's cursor shape, kind archive, the digest of the query, the key,
// and the line.
func encodeCursor(q metering.RecordQuery, key string, line int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(CursorKind + "|" + digest(q) + "|" + key + "|" + strconv.Itoa(line)))
}

// decodeCursor checks cursor against q and returns the key and line to
// resume from; a cursor that does not decode or is over another kind or
// query is store.ErrInvalidCursor with the detail.
func decodeCursor(cursor string, q metering.RecordQuery) (key string, line int, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, fmt.Errorf("%w: not base64url: %w", store.ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 4)
	if len(parts) != 4 {
		return "", 0, fmt.Errorf("%w: %d fields, want kind, query, key, and line", store.ErrInvalidCursor, len(parts))
	}
	if parts[0] != CursorKind {
		return "", 0, fmt.Errorf("%w: cursor is over %s, the request over %s", store.ErrInvalidCursor, parts[0], CursorKind)
	}
	if parts[1] != digest(q) {
		return "", 0, fmt.Errorf("%w: cursor query %s, the request's %s", store.ErrInvalidCursor, parts[1], digest(q))
	}
	if line, err = strconv.Atoi(parts[3]); err != nil || line < 0 {
		return "", 0, fmt.Errorf("%w: line %q is not a count", store.ErrInvalidCursor, parts[3])
	}
	return parts[2], line, nil
}

// digest is the 8 hex characters of SHA-256 over the query's canonical
// JSON, the store's spelling: members in a fixed order, lists sorted,
// labels by sorted key.
func digest(q metering.RecordQuery) string {
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
		panic("reqlog: a RecordQuery of strings and times does not encode: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:4])
}

// The seam: the reader is what internal/api switches to for the archive
// source, the shape of store.Usage().Records.
var _ interface {
	List(context.Context, metering.RecordQuery, store.Page) ([]metering.Record, string, error)
} = (*Reader)(nil)

// The client of latere.ai/x/pkg/s3 is a Bucket.
var _ Bucket = (*s3.Client)(nil)
