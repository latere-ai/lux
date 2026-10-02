// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"bufio"
	"context"
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
	bucket    Bucket
	prefix    string
	partition string
}

// NewReader constructs the reader over the bucket, LUX_S3_PREFIX, and
// LUX_REQUESTLOG_PARTITION_LABEL, empty for an archive with no
// partitions.
func NewReader(b Bucket, prefix, partitionLabel string) *Reader {
	if prefix == "" {
		prefix = DefaultPrefix
	}
	return &Reader{bucket: b, prefix: prefix, partition: partitionLabel}
}

// roots are the key prefixes an hour's objects sit under, in key order:
// the prefix itself, and with partitions each partition under it, only
// the one a query's label filter names when it names the partition
// label. The prefix itself stays among them, so objects written before
// partitioning was turned on are read too. Each listing adds one to
// calls, the page's count of bucket calls.
func (r *Reader) roots(ctx context.Context, q metering.RecordQuery, calls *int) ([]string, error) {
	if r.partition == "" {
		return []string{r.prefix}, nil
	}
	if v, ok := q.Labels[r.partition]; ok {
		return []string{path.Join(r.prefix, v)}, nil
	}
	out := []string{r.prefix}
	base := strings.TrimSuffix(r.prefix, "/") + "/"
	after := ""
	for {
		*calls++
		res, err := r.bucket.ListObjects(ctx, s3.ListOptions{Prefix: base, Delimiter: "/", StartAfter: after})
		if err != nil {
			return nil, fmt.Errorf("listing the partitions under %s: %w", base, err)
		}
		for _, p := range res.Prefixes {
			out = append(out, strings.TrimSuffix(p, "/"))
		}
		if !res.Truncated || len(res.Prefixes)+len(res.Objects) == 0 {
			break
		}
		after = continueAfter(res)
	}
	return out, nil
}

// PageReads is the bucket calls, listings and reads together, after
// which a page of a limited List ends at the next point where the walk
// has passed the position it resumed from. Reading is what a page costs,
// and a filter that matches few records reads many objects per record,
// so the bound on calls rather than on records is what makes every page
// cost about the same whatever its depth and its filters.
const PageReads = 64

// position is where a walk resumes: in hour, the objects before key are
// read, and the first line lines of key; an empty key is the start of
// the hour.
type position struct {
	hour time.Time
	key  string
	line int
}

// List pages the archived records that match q, the shape of
// store.Usage().Records so the route switches on the source. The walk
// goes newest first through the hours that hold objects: for each month
// of the range a delimited listing per root names the days present, for
// each present day one names the hours present, and each present hour's
// objects are read in ascending key order across the roots, which is by
// replica and then write order, decoded a line at a time with the
// filters applied. A page with a positive Limit ends when it holds Limit
// records, or once it has made PageReads bucket calls and passed the
// position it resumed from, so a page may hold fewer records than Limit,
// or none, while a cursor remains. The cursor is the range, the hour, the
// object key, and the line to resume from; one from another query is
// store.ErrInvalidCursor. A Limit of 0 or less is every record.
func (r *Reader) List(ctx context.Context, q metering.RecordQuery, p store.Page) ([]metering.Record, string, error) {
	if q.From.IsZero() || q.To.IsZero() || !q.To.After(q.From) {
		return nil, "", errors.New("reqlog: the query needs a range with to after from")
	}
	w := &walk{r: r, q: q, limit: p.Limit, first: q.From.UTC().Truncate(time.Hour)}
	w.start = position{hour: q.To.UTC().Add(-time.Nanosecond).Truncate(time.Hour)}
	if p.Cursor != "" {
		at, err := r.decodeCursor(p.Cursor, q)
		if err != nil {
			return nil, "", err
		}
		if at.hour.After(w.start.hour) || at.hour.Before(w.first) {
			return nil, "", fmt.Errorf("%w: hour %s is outside the range %s to %s", store.ErrInvalidCursor, at.hour.Format(time.RFC3339), q.From.UTC().Format(time.RFC3339), q.To.UTC().Format(time.RFC3339))
		}
		w.start = at
	}
	w.next = w.start
	roots, err := r.roots(ctx, q, &w.calls)
	if err != nil {
		return nil, "", err
	}
	w.roots = roots
	done, err := w.run(ctx)
	if err != nil {
		return nil, "", err
	}
	if done {
		return w.out, "", nil
	}
	return w.out, r.encodeCursor(q, w.next), nil
}

// walk is one page of List: the query, where it resumed, where it would
// resume now, whether it has moved past where it resumed, the bucket
// calls it has made, and the records it holds.
type walk struct {
	r     *Reader
	q     metering.RecordQuery
	limit int
	roots []string
	first time.Time // the range's first hour
	start position  // where the page resumed
	next  position  // where the next page resumes
	moved bool      // an object read or an hour passed since start
	calls int
	out   []metering.Record
}

// over reports whether a limited page ends here: its calls are spent
// and it has moved past where it resumed, which is what makes every page
// progress.
func (w *walk) over() bool {
	return w.limit > 0 && w.calls >= PageReads && w.moved
}

// full reports whether a limited page holds its limit.
func (w *walk) full() bool { return w.limit > 0 && len(w.out) >= w.limit }

// pass moves next to the start of hour, the top of a month, day, or
// hour the walk enters, once it is below where the page resumed.
func (w *walk) pass(hour time.Time) {
	if hour.Before(w.start.hour) {
		w.next, w.moved = position{hour: hour}, true
	}
}

// run walks from w.start down to the range's first hour and reports
// whether it reached it; false is a page that ended at w.next, full or
// over its calls.
func (w *walk) run(ctx context.Context) (bool, error) {
	firstMonth := monthOf(w.first)
	for month := monthOf(w.start.hour); !month.Before(firstMonth); month = month.AddDate(0, -1, 0) {
		w.pass(month.AddDate(0, 1, 0).Add(-time.Hour))
		if w.over() {
			return false, nil
		}
		days, err := w.present(ctx, w.roots, month.Format("2006/01"), func(n int) (time.Time, bool) {
			day := time.Date(month.Year(), month.Month(), n, 0, 0, 0, 0, time.UTC)
			return day, day.Month() == month.Month() && !day.After(w.start.hour) && day.Add(24*time.Hour).After(w.first)
		})
		if err != nil {
			return false, err
		}
		for _, day := range days {
			w.pass(day.at.Add(23 * time.Hour))
			if w.over() {
				return false, nil
			}
			hours, err := w.present(ctx, day.roots, day.at.Format("2006/01/02"), func(n int) (time.Time, bool) {
				hour := day.at.Add(time.Duration(n) * time.Hour)
				return hour, n < 24 && !hour.After(w.start.hour) && !hour.Before(w.first)
			})
			if err != nil {
				return false, err
			}
			for _, hour := range hours {
				if done, err := w.hour(ctx, hour); done || err != nil {
					return false, err
				}
			}
		}
	}
	return true, nil
}

// hour reads the objects of one present hour in ascending key order
// across its roots, from where the page resumed when it is that hour,
// and reports whether the page ended inside it.
func (w *walk) hour(ctx context.Context, hour present) (bool, error) {
	w.pass(hour.at)
	if w.over() {
		return true, nil
	}
	var keys []string
	for _, root := range hour.roots {
		prefix := path.Join(root, hourPrefix(hour.at)) + "/"
		after := ""
		for {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			w.calls++
			res, err := w.r.bucket.ListObjects(ctx, s3.ListOptions{Prefix: prefix, StartAfter: after})
			if err != nil {
				return false, fmt.Errorf("listing %s: %w", prefix, err)
			}
			for _, obj := range res.Objects {
				keys = append(keys, obj.Key)
			}
			if !res.Truncated || len(res.Objects) == 0 {
				break
			}
			after = res.Objects[len(res.Objects)-1].Key
		}
	}
	slices.Sort(keys)
	resuming := hour.at.Equal(w.start.hour)
	for _, key := range keys {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		skip := 0
		if resuming && w.start.key != "" {
			if key < w.start.key {
				continue
			}
			if key == w.start.key {
				skip = w.start.line
			}
		}
		w.next = position{hour: hour.at, key: key, line: skip}
		if w.over() {
			return true, nil
		}
		w.calls++
		last, err := w.r.read(ctx, key, skip, w.q, w.limit, &w.out)
		if err != nil {
			return false, err
		}
		w.moved = true
		if w.full() {
			w.next = position{hour: hour.at, key: key, line: last}
			return true, nil
		}
	}
	return false, nil
}

// present is one day or hour that holds objects, with the roots that
// hold it.
type present struct {
	at    time.Time
	roots []string
}

// present lists the children of <root>/<under>/ under each root with a
// delimiter, reads each child's last segment as a number, keeps those
// that keep accepts as an instant, and answers the instants newest
// first with the roots that hold each.
func (w *walk) present(ctx context.Context, roots []string, under string, keep func(int) (time.Time, bool)) ([]present, error) {
	byAt := map[int64][]string{}
	for _, root := range roots {
		prefix := path.Join(root, under) + "/"
		after := ""
		for {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			w.calls++
			res, err := w.r.bucket.ListObjects(ctx, s3.ListOptions{Prefix: prefix, Delimiter: "/", StartAfter: after})
			if err != nil {
				return nil, fmt.Errorf("listing %s: %w", prefix, err)
			}
			for _, child := range res.Prefixes {
				n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(child, prefix), "/"))
				if err != nil || n < 0 {
					continue
				}
				if at, ok := keep(n); ok {
					byAt[at.Unix()] = append(byAt[at.Unix()], root)
				}
			}
			if !res.Truncated || len(res.Prefixes)+len(res.Objects) == 0 {
				break
			}
			after = continueAfter(res)
		}
	}
	out := make([]present, 0, len(byAt))
	for at, roots := range byAt {
		out = append(out, present{at: time.Unix(at, 0).UTC(), roots: roots})
	}
	slices.SortFunc(out, func(a, b present) int { return b.at.Compare(a.at) })
	return out, nil
}

// continueAfter is the StartAfter that continues a truncated listing:
// the last object's key, or, when the last entry is a common prefix, the
// prefix with its trailing delimiter raised by one byte. Every key under
// the prefix sorts before that, and continuing after the prefix itself
// would list those keys again and roll them up into the same prefix.
func continueAfter(res s3.ListResult) string {
	last := ""
	if n := len(res.Objects); n > 0 {
		last = res.Objects[n-1].Key
	}
	if n := len(res.Prefixes); n > 0 && res.Prefixes[n-1] > last {
		p := res.Prefixes[n-1]
		return p[:len(p)-1] + string([]byte{p[len(p)-1] + 1})
	}
	return last
}

// monthOf is the first instant of t's UTC month.
func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
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
	// The hour is the four segments before the object's name, after a
	// partition's own segment when there is one.
	parts := strings.Split(rest, "/")
	if len(parts) != 5 && len(parts) != 6 {
		return time.Time{}, fmt.Errorf("%w: key %s is not an hour and an object", store.ErrInvalidCursor, key)
	}
	at, err := time.Parse("2006/01/02/15", strings.Join(parts[len(parts)-5:len(parts)-1], "/"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: key %s: %w", store.ErrInvalidCursor, key, err)
	}
	return at, nil
}

// encodeCursor renders the cursor that resumes q at at: the store's
// query cursor of kind archive, its position the hour as Unix seconds,
// the line, and the object key, last so a '|' in a prefix stays in it.
func (r *Reader) encodeCursor(q metering.RecordQuery, at position) string {
	return store.EncodeQueryCursor(CursorKind, q, strconv.FormatInt(at.hour.Unix(), 10)+"|"+strconv.Itoa(at.line)+"|"+at.key)
}

// decodeCursor checks cursor against q and returns the position to
// resume from; a cursor that does not decode, is over another kind or
// query, or names a key outside the prefix or outside its hour is
// store.ErrInvalidCursor with the detail.
func (r *Reader) decodeCursor(cursor string, q metering.RecordQuery) (position, error) {
	raw, err := store.DecodeQueryCursor(cursor, CursorKind, q)
	if err != nil {
		return position{}, err
	}
	parts := strings.SplitN(raw, "|", 3)
	if len(parts) != 3 {
		return position{}, fmt.Errorf("%w: position %q is not an hour, a line, and a key", store.ErrInvalidCursor, raw)
	}
	secs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || secs%3600 != 0 {
		return position{}, fmt.Errorf("%w: hour %q is not the Unix seconds of an hour", store.ErrInvalidCursor, parts[0])
	}
	line, err := strconv.Atoi(parts[1])
	if err != nil || line < 0 {
		return position{}, fmt.Errorf("%w: line %q is not a count", store.ErrInvalidCursor, parts[1])
	}
	at := position{hour: time.Unix(secs, 0).UTC(), key: parts[2], line: line}
	if at.key == "" {
		if at.line != 0 {
			return position{}, fmt.Errorf("%w: line %d of no object", store.ErrInvalidCursor, at.line)
		}
		return at, nil
	}
	hour, err := hourOf(r.prefix, at.key)
	if err != nil {
		return position{}, err
	}
	if !hour.Equal(at.hour) {
		return position{}, fmt.Errorf("%w: key %s is not in the hour %s", store.ErrInvalidCursor, at.key, at.hour.Format(time.RFC3339))
	}
	return at, nil
}

// The seam: the reader is what internal/api switches to for the archive
// source, the shape of store.Usage().Records.
var _ interface {
	List(context.Context, metering.RecordQuery, store.Page) ([]metering.Record, string, error)
} = (*Reader)(nil)

// The client of latere.ai/x/pkg/s3 is a Bucket.
var _ Bucket = (*s3.Client)(nil)
