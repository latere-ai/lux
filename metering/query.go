// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"maps"
	"slices"
	"strconv"
	"time"
)

// The bounds of a usage query: the range is at most ninety days, which
// is what keeps a response finite without paging; at most three by
// dimensions, because a fourth multiplies rows past what a person
// reads; and a query naming no range reads the last day.
const (
	MaxRange     = 90 * 24 * time.Hour
	MaxBy        = 3
	DefaultRange = 24 * time.Hour
)

// Query is GET /v1/usage as the route parsed it: the range, the
// groupings, the interval, and the filters, every id already resolved
// from a name by the route and the authorizer's filter already
// intersected through Intersect. Keys, Models, and Providers carry ids;
// Owners carry rendered subjects; Labels select Keys carrying every
// pair.
type Query struct {
	From, To time.Time
	By       []Dimension
	Interval Interval

	Keys, Models, Providers, Owners []string
	Labels                          map[string]string
}

// RecordQuery is GET /v1/requests: the same range and filters over
// records, plus the record's own status, error code, and stream flag.
type RecordQuery struct {
	Query
	Status Status
	Error  string
	Stream *bool
}

// QueryError is a parameter the query refuses, with the parameter's
// name for the route's invalid_field and the developer's detail.
type QueryError struct {
	Field  string
	Detail string
}

func (e *QueryError) Error() string { return e.Field + ": " + e.Detail }

// WithDefaults fills the range a query left open: To is now, From is
// DefaultRange before To.
func (q Query) WithDefaults(now time.Time) Query {
	if q.To.IsZero() {
		q.To = now
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-DefaultRange)
	}
	return q
}

// Validate holds q to the parameter table: To after From and at most
// MaxRange later; at most MaxBy dimensions, each known and named once;
// a known interval.
func (q Query) Validate() error {
	switch {
	case q.From.IsZero() || q.To.IsZero():
		return &QueryError{"from", "the range needs both from and to"}
	case !q.To.After(q.From):
		return &QueryError{"to", "to " + q.To.UTC().Format(time.RFC3339) + " is not after from " + q.From.UTC().Format(time.RFC3339)}
	case q.To.Sub(q.From) > MaxRange:
		return &QueryError{"to", "the range " + q.To.Sub(q.From).String() + " is over the " + strconv.Itoa(int(MaxRange.Hours()/24)) + " day bound"}
	case len(q.By) > MaxBy:
		return &QueryError{"by", strconv.Itoa(len(q.By)) + " dimensions, at most " + strconv.Itoa(MaxBy)}
	case !q.Interval.Valid():
		return &QueryError{"interval", strconv.Quote(string(q.Interval)) + " is not one of none, hour, day, month"}
	}
	for i, d := range q.By {
		if !d.Valid() {
			return &QueryError{"by", strconv.Quote(string(d)) + " is not a dimension: key, model, provider, owner, door, status, or label:<name>"}
		}
		if slices.Contains(q.By[:i], d) {
			return &QueryError{"by", strconv.Quote(string(d)) + " is named twice"}
		}
	}
	return nil
}

// Matches reports whether the hourly row a is inside q's range and
// passes its filters. An hour that overlaps the range is inside it, so
// a from inside an hour reads that hour whole.
func (q Query) Matches(a Aggregate) bool {
	if !a.Bucket.Before(q.To) || !a.Bucket.Add(time.Hour).After(q.From) {
		return false
	}
	return q.selects(a.KeyID, a.ModelID, a.ProviderID, a.Owner, a.Labels)
}

// selects is the filter half shared by the two queries.
func (q Query) selects(keyID, modelID, providerID, owner string, labels map[string]string) bool {
	if len(q.Keys) > 0 && !slices.Contains(q.Keys, keyID) {
		return false
	}
	if len(q.Models) > 0 && !slices.Contains(q.Models, modelID) {
		return false
	}
	if len(q.Providers) > 0 && !slices.Contains(q.Providers, providerID) {
		return false
	}
	if len(q.Owners) > 0 && !slices.Contains(q.Owners, owner) {
		return false
	}
	for name, value := range q.Labels {
		if labels[name] != value {
			return false
		}
	}
	return true
}

// Matches reports whether r is inside q's range, From inclusive and To
// exclusive on the record's At, and passes its filters.
func (q RecordQuery) Matches(r Record) bool {
	if r.At.Before(q.From) || !r.At.Before(q.To) {
		return false
	}
	if !q.selects(r.Key.ID, r.Model.ID, r.Provider.ID, r.Owner, r.Labels) {
		return false
	}
	if q.Status != "" && r.Status != q.Status {
		return false
	}
	if q.Error != "" && r.Error != q.Error {
		return false
	}
	if q.Stream != nil && r.Stream != *q.Stream {
		return false
	}
	return true
}

// Intersect narrows q by the authorizer's filter: owners narrows
// q.Owners to the subjects both name, or to the filter's when the query
// named none, and labels adds every pair to q.Labels. ok is false when
// the intersection selects nothing, an owners list with nothing in
// common or a label the query already names with another value, which
// the route answers with an empty result and never a 403. An empty
// filter leaves q as it is.
func Intersect(q Query, owners []string, labels map[string]string) (out Query, ok bool) {
	out = q
	if len(owners) > 0 {
		if len(q.Owners) == 0 {
			out.Owners = slices.Clone(owners)
		} else {
			out.Owners = nil
			for _, o := range q.Owners {
				if slices.Contains(owners, o) {
					out.Owners = append(out.Owners, o)
				}
			}
			if len(out.Owners) == 0 {
				return out, false
			}
		}
	}
	if len(labels) > 0 {
		out.Labels = maps.Clone(q.Labels)
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		for name, value := range labels {
			if have, set := out.Labels[name]; set && have != value {
				return out, false
			}
			out.Labels[name] = value
		}
	}
	return out, true
}
