// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"latere.ai/x/lux/internal/store"
)

type journal struct{ s *Store }

// copyEvent is an Event whose payload is not shared with the row.
func copyEvent(e store.Event) store.Event {
	e.Payload = slices.Clone(e.Payload)
	return e
}

// byGSeq orders rows in the store-wide sequence.
func byGSeq(a, b store.Event) int { return int(a.GSeq - b.GSeq) }

// Append implements store.Journal.
func (j journal) Append(ctx context.Context, e store.Event) (int64, error) {
	if e.ID == "" {
		return 0, errors.New("memory: Journal.Append of an event with no id")
	}
	if e.ObjectID == "" {
		return 0, fmt.Errorf("memory: Journal.Append of %s with no object id", e.ID)
	}
	err := j.s.write(ctx, func(st *state) error {
		if _, dup := st.journal[e.ID]; dup {
			return fmt.Errorf("memory: event %s is already in the journal", e.ID)
		}
		if e.At.IsZero() {
			e.At = j.s.now()
		}
		if e.NextAttemptAt.IsZero() {
			e.NextAttemptAt = e.At
		}
		st.gseq++
		st.seqs[e.ObjectID]++
		e.GSeq, e.Seq = st.gseq, st.seqs[e.ObjectID]
		st.journal[e.ID] = copyEvent(e)
		return nil
	})
	return e.Seq, err
}

// Pending implements store.Journal.
func (j journal) Pending(ctx context.Context, limit int) ([]store.Event, error) {
	var out []store.Event
	err := j.s.read(ctx, func(st *state) error {
		now := j.s.now()
		oldest := map[string]store.Event{}
		for _, e := range st.journal {
			if !e.AckedAt.IsZero() {
				continue
			}
			if cur, ok := oldest[e.ObjectID]; !ok || e.Seq < cur.Seq {
				oldest[e.ObjectID] = e
			}
		}
		for _, e := range oldest {
			if !e.NextAttemptAt.After(now) {
				out = append(out, copyEvent(e))
			}
		}
		slices.SortFunc(out, byGSeq)
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

// update replaces one row through fn, or answers ErrNotFound.
func (j journal) update(ctx context.Context, id string, fn func(e *store.Event) bool) error {
	return j.s.write(ctx, func(st *state) error {
		e, ok := st.journal[id]
		if !ok {
			return fmt.Errorf("%w: event %s is not in the journal", store.ErrNotFound, id)
		}
		if fn(&e) {
			st.journal[id] = e
		} else {
			delete(st.journal, id)
		}
		return nil
	})
}

// Acknowledge implements store.Journal.
func (j journal) Acknowledge(ctx context.Context, id string) error {
	return j.update(ctx, id, func(e *store.Event) bool {
		e.AckedAt = j.s.now()
		return true
	})
}

// Defer implements store.Journal.
func (j journal) Defer(ctx context.Context, id string, attempts int, next time.Time) error {
	return j.update(ctx, id, func(e *store.Event) bool {
		e.Attempts, e.NextAttemptAt = attempts, next
		return true
	})
}

// Drop implements store.Journal.
func (j journal) Drop(ctx context.Context, id string) error {
	return j.update(ctx, id, func(*store.Event) bool { return false })
}

// ByObject implements store.Journal.
func (j journal) ByObject(ctx context.Context, objectID string, p store.Page) ([]store.Event, string, error) {
	var out []store.Event
	var next string
	f := store.Filter{IDs: []string{objectID}}
	err := j.s.read(ctx, func(st *state) error {
		var after int64
		if p.Cursor != "" {
			last, err := store.DecodeCursor(p.Cursor, store.JournalKind, f)
			if err != nil {
				return err
			}
			if after, err = strconv.ParseInt(last, 10, 64); err != nil {
				return fmt.Errorf("%w: last %q is not a sequence", store.ErrInvalidCursor, last)
			}
		}
		for _, e := range st.journal {
			if e.ObjectID == objectID && e.Seq > after {
				out = append(out, copyEvent(e))
			}
		}
		slices.SortFunc(out, func(a, b store.Event) int { return int(a.Seq - b.Seq) })
		if p.Limit > 0 && len(out) > p.Limit {
			out = out[:p.Limit]
			next = store.EncodeCursor(store.JournalKind, f, strconv.FormatInt(out[len(out)-1].Seq, 10))
		}
		return nil
	})
	return out, next, err
}

// Since implements store.Journal.
func (j journal) Since(ctx context.Context, afterGSeq int64, limit int) ([]store.Event, error) {
	var out []store.Event
	err := j.s.read(ctx, func(st *state) error {
		for _, e := range st.journal {
			if e.GSeq > afterGSeq {
				out = append(out, copyEvent(e))
			}
		}
		slices.SortFunc(out, byGSeq)
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

// Prune implements store.Journal.
func (j journal) Prune(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := j.s.write(ctx, func(st *state) error {
		for id, e := range st.journal {
			if !e.AckedAt.IsZero() && !e.At.After(before) {
				delete(st.journal, id)
				n++
			}
		}
		return nil
	})
	return n, err
}
