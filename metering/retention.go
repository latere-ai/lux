// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"
)

// The retention of spec 038: rules matched on a usage row's Key labels,
// each keeping hourly rows for a period and then folding them into one
// row per month, with chosen dimensions dropped, kept for a period of
// its own. A row no rule matches is kept hourly for ever.

// The shortest periods a rule may name: an hourly row lives at least a
// day, so the hour a flush is still writing is never folded, and a
// monthly row at least thirty days, a month.
const (
	MinHourlyRetention  = 24 * time.Hour
	MinMonthlyRetention = 720 * time.Hour
)

// Retention is a period a row is kept for; zero is for ever.
type Retention time.Duration

// Forever is the retention that never ends.
const Forever Retention = 0

// UnmarshalJSON reads a Go duration or the word forever.
func (r *Retention) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("a retention is a duration such as 2160h or the word forever: %w", err)
	}
	if s == "forever" {
		*r = Forever
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return fmt.Errorf("%q is neither a positive duration nor forever", s)
	}
	*r = Retention(d)
	return nil
}

// MarshalJSON writes the duration, or forever.
func (r Retention) MarshalJSON() ([]byte, error) {
	if r == Forever {
		return json.Marshal("forever")
	}
	return json.Marshal(time.Duration(r).String())
}

// The dimensions a rule may drop from its monthly rows.
const (
	DropOwner  Dimension = "owner"
	DropKey    Dimension = "key"
	DropLabels Dimension = "labels"
)

// RetentionRule is one rule: the Key labels a row must carry, how long
// its hourly rows are kept, how long its monthly rows are kept after
// their month ends, and what the monthly rows drop.
type RetentionRule struct {
	Match   map[string]string `json:"match,omitempty"`
	Hourly  Retention         `json:"hourly"`
	Monthly Retention         `json:"monthly"`
	Drop    []Dimension       `json:"drop,omitempty"`
}

// RetentionRules are the rules in the order written; the first whose
// match a row's labels satisfy applies to it.
type RetentionRules []RetentionRule

// ParseRetentionRules reads LUX_USAGE_RETENTION: a JSON list of rules,
// every member known. An empty value is no rule. A refusal names the
// rule by its position, from zero.
func ParseRetentionRules(raw string) (RetentionRules, error) {
	if len(bytes.TrimSpace([]byte(raw))) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var rules RetentionRules
	if err := dec.Decode(&rules); err != nil {
		return nil, fmt.Errorf("not a JSON list of rules: %w", err)
	}
	for i, r := range rules {
		at := "rule " + strconv.Itoa(i)
		// An absent period decodes as zero, for ever, as the table says.
		if r.Hourly != Forever && time.Duration(r.Hourly) < MinHourlyRetention {
			return nil, errors.New(at + ": hourly is at least 24h or forever")
		}
		if r.Monthly != Forever && time.Duration(r.Monthly) < MinMonthlyRetention {
			return nil, errors.New(at + ": monthly is at least 720h or forever")
		}
		for _, d := range r.Drop {
			if d != DropOwner && d != DropKey && d != DropLabels {
				return nil, fmt.Errorf("%s: drop %q is not owner, key, or labels", at, d)
			}
		}
		if len(slices.Compact(slices.Sorted(slices.Values(r.Drop)))) != len(r.Drop) {
			return nil, errors.New(at + ": drop names a dimension twice")
		}
	}
	return rules, nil
}

// For is the rule that applies to a row carrying labels: the first
// whose every match pair the labels hold, and nil when none does.
func (rs RetentionRules) For(labels map[string]string) *RetentionRule {
	for i := range rs {
		ok := true
		for k, v := range rs[i].Match {
			if labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return &rs[i]
		}
	}
	return nil
}

// ShortestHourly is the shortest finite hourly retention of any rule,
// and false when every rule keeps hourly rows for ever.
func (rs RetentionRules) ShortestHourly() (time.Duration, bool) {
	return shortest(rs, func(r RetentionRule) Retention { return r.Hourly })
}

// ShortestMonthly is the shortest finite monthly retention of any rule.
func (rs RetentionRules) ShortestMonthly() (time.Duration, bool) {
	return shortest(rs, func(r RetentionRule) Retention { return r.Monthly })
}

func shortest(rs RetentionRules, of func(RetentionRule) Retention) (time.Duration, bool) {
	var out time.Duration
	found := false
	for _, r := range rs {
		if d := time.Duration(of(r)); d > 0 && (!found || d < out) {
			out, found = d, true
		}
	}
	return out, found
}

// HourlyDue reports whether an hourly row is past the rule's hourly
// retention at now, measured from the end of its hour.
func (r *RetentionRule) HourlyDue(a Aggregate, now time.Time) bool {
	return r != nil && r.Hourly != Forever && !now.Before(a.Bucket.Add(time.Hour).Add(time.Duration(r.Hourly)))
}

// MonthlyDue reports whether a monthly row is past the rule's monthly
// retention at now, measured from the end of its month.
func (r *RetentionRule) MonthlyDue(a Aggregate, now time.Time) bool {
	return r != nil && r.Monthly != Forever && !now.Before(a.Bucket.AddDate(0, 1, 0).Add(time.Duration(r.Monthly)))
}

// MonthlyRow is the row an hourly row folds into under the rule: the first
// instant of its UTC month, the rule's dimensions dropped, the labels
// narrowed to the rule's match pairs when labels are dropped, so the
// rule still matches the row it wrote. The sums are the hourly row's.
func (r *RetentionRule) MonthlyRow(a Aggregate) Aggregate {
	out := a
	out.Bucket = IntervalMonth.Bucket(a.Bucket)
	out.Labels = maps.Clone(a.Labels)
	for _, d := range r.Drop {
		switch d {
		case DropOwner:
			out.Owner = ""
		case DropKey:
			out.KeyID = ""
		case DropLabels:
			out.Labels = maps.Clone(r.Match)
		}
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	return out
}

// Move is one hourly row folded into a monthly one: the hourly row's
// key, which the store deletes, and the monthly row's dimensions, into
// which the store adds the sums the deleted row held.
type Move struct {
	From AggregateKey
	To   Aggregate
}

// MonthOverlaps reports whether the month starting at a monthly row's
// bucket overlaps q's range and the row passes q's filters.
func (q Query) MonthOverlaps(a Aggregate) bool {
	if !a.Bucket.Before(q.To) || !a.Bucket.AddDate(0, 1, 0).After(q.From) {
		return false
	}
	return q.selects(a.KeyID, a.ModelID, a.ProviderID, a.Owner, a.Labels)
}

// Compare is -1, 0, or 1 as k sorts before, with, or after o: by bucket
// and then by every dimension, the order a roll-up pass pages through
// the rows in.
func (k AggregateKey) Compare(o AggregateKey) int {
	if k.Bucket != o.Bucket {
		if k.Bucket < o.Bucket {
			return -1
		}
		return 1
	}
	for _, p := range [][2]string{
		{k.KeyID, o.KeyID}, {k.ModelID, o.ModelID}, {k.ProviderID, o.ProviderID}, {k.Owner, o.Owner},
		{string(k.Door), string(o.Door)}, {string(k.Status), string(o.Status)}, {k.Currency, o.Currency},
	} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}
