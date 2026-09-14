// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"maps"
	"slices"
	"strings"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Dimension is one grouping of the usage API's by parameter: one of
// the six named below, or label:<name> over the Key's labels.
type Dimension string

// The named dimensions. The key, model, and provider dimensions carry
// ids, never names, because a Key's key_ id keeps working after the
// Key is deleted and a Model may be renamed under a running query.
const (
	DimensionKey      Dimension = "key"
	DimensionModel    Dimension = "model"
	DimensionProvider Dimension = "provider"
	DimensionOwner    Dimension = "owner"
	DimensionDoor     Dimension = "door"
	DimensionStatus   Dimension = "status"
	// LabelPrefix begins a dimension over one of the Key's labels.
	LabelPrefix = "label:"
)

// Label returns the label name of a label:<name> dimension and true,
// or false for a named dimension.
func (d Dimension) Label() (string, bool) {
	name, ok := strings.CutPrefix(string(d), LabelPrefix)
	return name, ok && name != ""
}

// Valid reports whether d is one of the six named dimensions or a
// label dimension with a name.
func (d Dimension) Valid() bool {
	switch d {
	case DimensionKey, DimensionModel, DimensionProvider, DimensionOwner, DimensionDoor, DimensionStatus:
		return true
	}
	_, ok := d.Label()
	return ok
}

// Interval is the width of a response row's bucket.
type Interval string

// The intervals; the empty string reads as none.
const (
	IntervalNone  Interval = "none"
	IntervalHour  Interval = "hour"
	IntervalDay   Interval = "day"
	IntervalMonth Interval = "month"
)

// Valid reports whether i is one of the four intervals or empty.
func (i Interval) Valid() bool {
	switch i {
	case "", IntervalNone, IntervalHour, IntervalDay, IntervalMonth:
		return true
	}
	return false
}

// Bucket is the start of the bucket of i that holds at, in UTC, and
// the zero time for none: a month is the calendar month in UTC, and an
// hour or a day is the clock's.
func (i Interval) Bucket(at time.Time) time.Time {
	at = at.UTC()
	switch i {
	case IntervalHour:
		return at.Truncate(time.Hour)
	case IntervalDay:
		return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	case IntervalMonth:
		return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

// Sums are the summed columns of an aggregate row: Cost in micro-units
// of the row's currency, Unpriced the requests that carried no price.
type Sums struct {
	Requests          int64
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	Cost              int64
	Unpriced          int64
}

// Add adds o to s.
func (s *Sums) Add(o Sums) {
	s.Requests += o.Requests
	s.InputTokens += o.InputTokens
	s.OutputTokens += o.OutputTokens
	s.CachedInputTokens += o.CachedInputTokens
	s.CacheWriteTokens += o.CacheWriteTokens
	s.Cost += o.Cost
	s.Unpriced += o.Unpriced
}

// Aggregate is the store's row: one hour, one dimension tuple, the
// Key's labels denormalised, and the sums. The primary key is every
// dimension with the bucket, which AggregateKey renders; Labels are a
// function of KeyID, so they add no rows. A record's requestLabels are
// not here and cannot be grouped on.
type Aggregate struct {
	Bucket     time.Time
	KeyID      string
	ModelID    string
	ProviderID string
	Owner      string
	Door       v1.Dialect
	Status     Status
	Currency   string
	Labels     map[string]string
	Sums
}

// AggregateKey is the primary key of an Aggregate, comparable so a map
// upserts on it.
type AggregateKey struct {
	Bucket     int64 // Unix seconds
	KeyID      string
	ModelID    string
	ProviderID string
	Owner      string
	Door       v1.Dialect
	Status     Status
	Currency   string
}

// Key is a's primary key.
func (a Aggregate) Key() AggregateKey {
	return AggregateKey{
		Bucket: a.Bucket.Unix(), KeyID: a.KeyID, ModelID: a.ModelID, ProviderID: a.ProviderID,
		Owner: a.Owner, Door: a.Door, Status: a.Status, Currency: a.Currency,
	}
}

// AggregateOf is the hourly row one record adds to: its hour in UTC,
// its dimensions, its labels, one request, its tokens, and its cost
// when priced or one unpriced request when not.
func AggregateOf(r Record) Aggregate {
	a := Aggregate{
		Bucket: IntervalHour.Bucket(r.At), KeyID: r.Key.ID, ModelID: r.Model.ID, ProviderID: r.Provider.ID,
		Owner: r.Owner, Door: r.Door, Status: r.Status, Currency: r.Cost.Currency,
		Labels:   maps.Clone(r.Labels),
		Requests: 1, InputTokens: r.Tokens.Input, OutputTokens: r.Tokens.Output,
		CachedInputTokens: r.Tokens.CachedInput, CacheWriteTokens: r.Tokens.CacheWrite,
	}
	if a.Labels == nil {
		a.Labels = map[string]string{}
	}
	if r.Cost.Priced {
		a.Cost = r.Cost.Amount
	} else {
		a.Unpriced = 1
	}
	return a
}

// Aggregates folds records into hourly rows, one per hour per dimension
// tuple, in a fixed order, so the same records fold to the same rows
// wherever they are folded.
func Aggregates(rs []Record) []Aggregate {
	rows := map[AggregateKey]*Aggregate{}
	for _, r := range rs {
		a := AggregateOf(r)
		k := a.Key()
		if have, ok := rows[k]; ok {
			have.Add(a.Sums)
			continue
		}
		rows[k] = &a
	}
	out := make([]Aggregate, 0, len(rows))
	for _, a := range rows {
		out = append(out, *a)
	}
	slices.SortFunc(out, compareAggregates)
	return out
}

func compareAggregates(a, b Aggregate) int {
	if c := a.Bucket.Compare(b.Bucket); c != 0 {
		return c
	}
	for _, c := range []int{
		strings.Compare(a.KeyID, b.KeyID), strings.Compare(a.ModelID, b.ModelID), strings.Compare(a.ProviderID, b.ProviderID),
		strings.Compare(a.Owner, b.Owner), strings.Compare(string(a.Door), string(b.Door)), strings.Compare(string(a.Status), string(b.Status)),
	} {
		if c != 0 {
			return c
		}
	}
	return strings.Compare(a.Currency, b.Currency)
}

// Row is one response row of GET /v1/usage: the bucket's start for an
// interval other than none, each by dimension's value, the counts, the
// tokens, and the cost as an int64 of micro-units of Currency. Currency
// is a dimension of every row whether or not by names it, because two
// currencies are never summed; OK, Refused, and Failed sum the status
// dimension of the aggregate rows.
type Row struct {
	Bucket            time.Time         `json:"bucket,omitzero"`
	Dimensions        map[string]string `json:"dimensions"`
	Requests          int64             `json:"requests"`
	OK                int64             `json:"ok"`
	Refused           int64             `json:"refused"`
	Failed            int64             `json:"failed"`
	InputTokens       int64             `json:"inputTokens"`
	OutputTokens      int64             `json:"outputTokens"`
	CachedInputTokens int64             `json:"cachedInputTokens"`
	CacheWriteTokens  int64             `json:"cacheWriteTokens"`
	Cost              int64             `json:"cost"`
	Currency          string            `json:"currency"`
	UnpricedRequests  int64             `json:"unpricedRequests"`
}

// value is a's value of dimension d: the id, owner, door, or status,
// or the Key's label of that name, empty when the Key lacks it.
func (a Aggregate) value(d Dimension) string {
	switch d {
	case DimensionKey:
		return a.KeyID
	case DimensionModel:
		return a.ModelID
	case DimensionProvider:
		return a.ProviderID
	case DimensionOwner:
		return a.Owner
	case DimensionDoor:
		return string(a.Door)
	case DimensionStatus:
		return string(a.Status)
	}
	if name, ok := d.Label(); ok {
		return a.Labels[name]
	}
	return ""
}

// Group sums hourly rows into response rows: one row per bucket of in,
// per currency, per tuple of the by dimensions' values, in bucket then
// dimension then currency order. It is pure, so the aggregates an
// archive folds to can be compared against the store's.
func Group(as []Aggregate, by []Dimension, in Interval) []Row {
	type key struct {
		bucket   int64
		values   string
		currency string
	}
	rows := map[key]*Row{}
	for _, a := range as {
		bucket := in.Bucket(a.Bucket)
		values := make([]string, len(by))
		dims := make(map[string]string, len(by))
		for i, d := range by {
			values[i] = a.value(d)
			dims[string(d)] = values[i]
		}
		k := key{bucket.Unix(), strings.Join(values, "\x00"), a.Currency}
		if bucket.IsZero() {
			k.bucket = 0
		}
		row, ok := rows[k]
		if !ok {
			row = &Row{Bucket: bucket, Dimensions: dims, Currency: a.Currency}
			rows[k] = row
		}
		row.Requests += a.Requests
		row.InputTokens += a.InputTokens
		row.OutputTokens += a.OutputTokens
		row.CachedInputTokens += a.CachedInputTokens
		row.CacheWriteTokens += a.CacheWriteTokens
		row.Cost += a.Cost
		row.UnpricedRequests += a.Unpriced
		switch a.Status {
		case StatusOK:
			row.OK += a.Requests
		case StatusRefused:
			row.Refused += a.Requests
		case StatusFailed:
			row.Failed += a.Requests
		}
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Row) int {
		if c := a.Bucket.Compare(b.Bucket); c != 0 {
			return c
		}
		for _, d := range by {
			if c := strings.Compare(a.Dimensions[string(d)], b.Dimensions[string(d)]); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Currency, b.Currency)
	})
	return out
}

// Fold turns records into response rows: the hourly aggregates of
// Aggregates, grouped by Group. It is pure.
func Fold(rs []Record, by []Dimension, in Interval) []Row {
	return Group(Aggregates(rs), by, in)
}
