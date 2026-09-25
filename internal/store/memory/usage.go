// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

type usage struct{ s *Store }

// AddRows implements store.Usage: a row is upserted on its primary key,
// its sums added to what the key holds and its labels replaced, so a
// Key relabelled between two flushes reads with its newest labels.
func (u usage) AddRows(ctx context.Context, rows []metering.Aggregate) error {
	return u.s.write(ctx, func(st *state) error {
		for _, a := range rows {
			if a.Bucket.IsZero() {
				return errors.New("memory: Usage.AddRows with a row that has no bucket")
			}
			a.Bucket = a.Bucket.UTC()
			k := a.Key()
			if have, ok := st.aggregates[k]; ok {
				have.Add(a.Sums)
				have.Labels = maps.Clone(a.Labels)
				st.aggregates[k] = have
				continue
			}
			a.Labels = maps.Clone(a.Labels)
			if a.Labels == nil {
				a.Labels = map[string]string{}
			}
			st.aggregates[k] = a
		}
		return nil
	})
}

// QueryRows implements store.Usage: the rows inside the range that
// pass the filters, grouped by metering.Group.
func (u usage) QueryRows(ctx context.Context, q metering.Query) ([]metering.Row, error) {
	var matched []metering.Aggregate
	err := u.s.read(ctx, func(st *state) error {
		for _, a := range st.aggregates {
			if q.Matches(a) {
				matched = append(matched, a)
			}
		}
		for _, a := range st.monthly {
			if q.MonthOverlaps(a) {
				matched = append(matched, a)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return metering.Group(matched, q.By, q.Interval), nil
}

// AppendRecord implements store.Usage.
func (u usage) AppendRecord(ctx context.Context, r metering.Record) error {
	if r.ID == "" {
		return errors.New("memory: Usage.AppendRecord with a record that has no id")
	}
	return u.s.write(ctx, func(st *state) error {
		ring := st.records[r.Key.ID]
		if len(ring) >= metering.RecordsPerKey {
			ring = ring[len(ring)-metering.RecordsPerKey+1:]
		}
		st.records[r.Key.ID] = append(ring, r)
		return nil
	})
}

// Records implements store.Usage.
func (u usage) Records(ctx context.Context, q metering.RecordQuery, p store.Page) ([]metering.Record, string, error) {
	var after *metering.Record
	if p.Cursor != "" {
		at, id, err := store.DecodeRecordCursor(p.Cursor, q)
		if err != nil {
			return nil, "", err
		}
		after = &metering.Record{At: at, ID: id}
	}
	var out []metering.Record
	err := u.s.read(ctx, func(st *state) error {
		for _, ring := range st.records {
			for _, r := range ring {
				if q.Matches(r) && (after == nil || store.NewerFirst(*after, r) < 0) {
					out = append(out, r)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	slices.SortFunc(out, store.NewerFirst)
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
		return out, store.EncodeRecordCursor(q, out[len(out)-1]), nil
	}
	return out, "", nil
}

// Hourly implements store.Usage.
func (u usage) Hourly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	var out []metering.Aggregate
	err := u.s.read(ctx, func(st *state) error {
		out = page(st.aggregates, before, after, limit)
		return nil
	})
	return out, err
}

// Monthly implements store.Usage.
func (u usage) Monthly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	var out []metering.Aggregate
	err := u.s.read(ctx, func(st *state) error {
		out = page(st.monthly, before, after, limit)
		return nil
	})
	return out, err
}

// page is the rows of m before before and after after, in key order, at
// most limit when limit is positive.
func page(m map[metering.AggregateKey]metering.Aggregate, before time.Time, after metering.AggregateKey, limit int) []metering.Aggregate {
	var out []metering.Aggregate
	for k, a := range m {
		if a.Bucket.Before(before) && k.Compare(after) > 0 {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b metering.Aggregate) int { return a.Key().Compare(b.Key()) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i].Labels = maps.Clone(out[i].Labels)
	}
	return out
}

// add sums a into the row of m under a's key, its labels replacing the
// row's, as AddRows does.
func add(m map[metering.AggregateKey]metering.Aggregate, a metering.Aggregate) {
	k := a.Key()
	if have, ok := m[k]; ok {
		have.Add(a.Sums)
		have.Labels = maps.Clone(a.Labels)
		m[k] = have
		return
	}
	a.Labels = maps.Clone(a.Labels)
	if a.Labels == nil {
		a.Labels = map[string]string{}
	}
	m[k] = a
}

// Fold implements store.Usage.
func (u usage) Fold(ctx context.Context, moves []metering.Move, expired []metering.AggregateKey) (folded, deleted int, err error) {
	err = u.s.write(ctx, func(st *state) error {
		for _, mv := range moves {
			from, ok := st.aggregates[mv.From]
			if !ok {
				continue
			}
			delete(st.aggregates, mv.From)
			to := mv.To
			to.Bucket = to.Bucket.UTC()
			to.Sums = from.Sums
			add(st.monthly, to)
			folded++
		}
		for _, k := range expired {
			if _, ok := st.monthly[k]; ok {
				delete(st.monthly, k)
				deleted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return folded, deleted, nil
}

// RedactOwner implements store.Usage.
func (u usage) RedactOwner(ctx context.Context, owner string) (int, error) {
	if owner == "" {
		return 0, errors.New("memory: Usage.RedactOwner with no owner")
	}
	n := 0
	err := u.s.write(ctx, func(st *state) error {
		for _, m := range []map[metering.AggregateKey]metering.Aggregate{st.aggregates, st.monthly} {
			for k, a := range m {
				if a.Owner != owner {
					continue
				}
				delete(m, k)
				a.Owner = ""
				add(m, a)
				n++
			}
		}
		for id, ring := range st.records {
			var fresh []metering.Record
			for i, r := range ring {
				if r.Owner != owner {
					continue
				}
				if fresh == nil {
					fresh = slices.Clone(ring)
				}
				fresh[i].Owner = ""
			}
			if fresh != nil {
				st.records[id] = fresh
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

var _ store.Usage = usage{}
