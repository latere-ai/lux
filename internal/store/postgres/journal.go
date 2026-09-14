// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lux/internal/store"
)

type journal struct{ s *Store }

// journalLock is the advisory lock key every Append takes for the length
// of its transaction, so the store-wide sequence is assigned in commit
// order: a sequence would hand out numbers that commit out of order, and
// a replica tailing Since past the later one would never see the
// earlier. Mutations are rare beside the data plane, so one append at a
// time across the installation costs nothing a caller notices.
const journalLock int64 = 0x6c75785f6a726e6c

// eventColumns is the column list every read of the journal scans.
const eventColumns = `id, gseq, object_id, seq, type, at, payload, attempts, next_attempt_at, acked_at`

// scanEvent reads one row of eventColumns.
func scanEvent(row pgx.Row) (store.Event, error) {
	var e store.Event
	var acked *time.Time
	if err := row.Scan(&e.ID, &e.GSeq, &e.ObjectID, &e.Seq, &e.Type, &e.At, &e.Payload, &e.Attempts, &e.NextAttemptAt, &acked); err != nil {
		return store.Event{}, err
	}
	e.At, e.NextAttemptAt = e.At.UTC(), e.NextAttemptAt.UTC()
	if acked != nil {
		e.AckedAt = acked.UTC()
	}
	return e, nil
}

// collect reads every row of a query as events.
func (j journal) collect(ctx context.Context, q querier, sql string, args ...any) ([]store.Event, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Append implements store.Journal.
func (j journal) Append(ctx context.Context, e store.Event) (int64, error) {
	if e.ID == "" {
		return 0, errors.New("postgres: Journal.Append of an event with no id")
	}
	if e.ObjectID == "" {
		return 0, fmt.Errorf("postgres: Journal.Append of %s with no object id", e.ID)
	}
	if e.At.IsZero() {
		e.At = j.s.now()
	}
	if e.NextAttemptAt.IsZero() {
		e.NextAttemptAt = e.At
	}
	var seq int64
	err := j.s.write(ctx, func(q pgx.Tx) error {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, journalLock); err != nil {
			return err
		}
		err := q.QueryRow(ctx, `
			INSERT INTO journal (id, gseq, object_id, seq, type, at, payload, attempts, next_attempt_at)
			VALUES ($1,
				(SELECT COALESCE(MAX(gseq), 0) + 1 FROM journal),
				$2,
				(SELECT COALESCE(MAX(seq), 0) + 1 FROM journal WHERE object_id = $2),
				$3, $4, $5, $6, $7)
			RETURNING seq`, e.ID, e.ObjectID, e.Type, e.At, e.Payload, e.Attempts, e.NextAttemptAt).Scan(&seq)
		if violates(err, "journal_pkey") {
			return fmt.Errorf("event %s is already in the journal", e.ID)
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: Journal.Append: %w", err)
	}
	return seq, nil
}

// Pending implements store.Journal: the due rows that are the oldest
// unacknowledged of their object, oldest first; the anti-join is what
// lets an object's not-yet-due row hold its later rows back.
func (j journal) Pending(ctx context.Context, limit int) ([]store.Event, error) {
	var out []store.Event
	err := j.s.read(ctx, func(q querier) error {
		var err error
		out, err = j.collect(ctx, q, `
			SELECT `+eventColumns+` FROM journal j
			WHERE j.acked_at IS NULL AND j.next_attempt_at <= $2
			  AND NOT EXISTS (SELECT 1 FROM journal o WHERE o.object_id = j.object_id AND o.acked_at IS NULL AND o.seq < j.seq)
			ORDER BY j.gseq LIMIT $1`, nullLimit(limit), j.s.now())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Journal.Pending: %w", err)
	}
	return out, nil
}

// update runs one statement over one row and answers ErrNotFound when
// no row moved.
func (j journal) update(ctx context.Context, what, sql string, args ...any) error {
	err := j.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: event %s is not in the journal", store.ErrNotFound, args[0])
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return err
		}
		return fmt.Errorf("postgres: Journal.%s: %w", what, err)
	}
	return nil
}

// Acknowledge implements store.Journal.
func (j journal) Acknowledge(ctx context.Context, id string) error {
	return j.update(ctx, "Acknowledge", `UPDATE journal SET acked_at = $2 WHERE id = $1`, id, j.s.now())
}

// Defer implements store.Journal.
func (j journal) Defer(ctx context.Context, id string, attempts int, next time.Time) error {
	return j.update(ctx, "Defer", `UPDATE journal SET attempts = $2, next_attempt_at = $3 WHERE id = $1`, id, attempts, next)
}

// Drop implements store.Journal.
func (j journal) Drop(ctx context.Context, id string) error {
	return j.update(ctx, "Drop", `DELETE FROM journal WHERE id = $1`, id)
}

// ByObject implements store.Journal.
func (j journal) ByObject(ctx context.Context, objectID string, p store.Page) ([]store.Event, string, error) {
	f := store.Filter{IDs: []string{objectID}}
	var after int64
	if p.Cursor != "" {
		last, err := store.DecodeCursor(p.Cursor, store.JournalKind, f)
		if err != nil {
			return nil, "", err
		}
		if after, err = strconv.ParseInt(last, 10, 64); err != nil {
			return nil, "", fmt.Errorf("%w: last %q is not a sequence", store.ErrInvalidCursor, last)
		}
	}
	var out []store.Event
	err := j.s.read(ctx, func(q querier) error {
		var err error
		out, err = j.collect(ctx, q, `SELECT `+eventColumns+` FROM journal WHERE object_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`,
			objectID, after, nullLimit(pageLimit(p.Limit)))
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: Journal.ByObject: %w", err)
	}
	next := ""
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
		next = store.EncodeCursor(store.JournalKind, f, strconv.FormatInt(out[len(out)-1].Seq, 10))
	}
	return out, next, nil
}

// Since implements store.Journal.
func (j journal) Since(ctx context.Context, afterGSeq int64, limit int) ([]store.Event, error) {
	var out []store.Event
	err := j.s.read(ctx, func(q querier) error {
		var err error
		out, err = j.collect(ctx, q, `SELECT `+eventColumns+` FROM journal WHERE gseq > $1 ORDER BY gseq LIMIT $2`, afterGSeq, nullLimit(limit))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Journal.Since: %w", err)
	}
	return out, nil
}

// Prune implements store.Journal.
func (j journal) Prune(ctx context.Context, before time.Time) (int, error) {
	n := 0
	err := j.s.read(ctx, func(q querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM journal WHERE acked_at IS NOT NULL AND at <= $1`, before)
		n = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: Journal.Prune: %w", err)
	}
	return n, nil
}

// nullLimit is a LIMIT that is null, no limit, for zero or less.
func nullLimit(limit int) *int {
	if limit <= 0 {
		return nil
	}
	return &limit
}

// pageLimit is one more than a page's limit, so a page learns whether
// rows remain; zero stays zero, every row.
func pageLimit(limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit + 1
}

var _ store.Journal = journal{}
