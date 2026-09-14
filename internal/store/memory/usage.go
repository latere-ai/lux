// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"maps"
	"slices"

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

var _ store.Usage = usage{}
