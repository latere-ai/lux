// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

type usage struct{ s *Store }

// AddRows implements store.Usage: one upsert per row on the primary key,
// the sums added and the labels replaced, in one batch.
func (u usage) AddRows(ctx context.Context, rows []metering.Aggregate) error {
	for _, a := range rows {
		if a.Bucket.IsZero() {
			return errors.New("postgres: Usage.AddRows with a row that has no bucket")
		}
	}
	if len(rows) == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	err := u.s.write(ctx, func(q pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, a := range rows {
			labels := a.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			batch.Queue(`
				INSERT INTO usage_hourly (bucket, key_id, model_id, provider_id, owner, door, status, currency, labels,
					requests, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, cost_micro, unpriced)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
				ON CONFLICT (bucket, key_id, model_id, provider_id, owner, door, status, currency) DO UPDATE SET
					labels = excluded.labels,
					requests = usage_hourly.requests + excluded.requests,
					input_tokens = usage_hourly.input_tokens + excluded.input_tokens,
					output_tokens = usage_hourly.output_tokens + excluded.output_tokens,
					cached_input_tokens = usage_hourly.cached_input_tokens + excluded.cached_input_tokens,
					cache_write_tokens = usage_hourly.cache_write_tokens + excluded.cache_write_tokens,
					cost_micro = usage_hourly.cost_micro + excluded.cost_micro,
					unpriced = usage_hourly.unpriced + excluded.unpriced`,
				a.Bucket.UTC(), a.KeyID, a.ModelID, a.ProviderID, a.Owner, string(a.Door), string(a.Status), a.Currency, labels,
				a.Requests, a.InputTokens, a.OutputTokens, a.CachedInputTokens, a.CacheWriteTokens, a.Cost, a.Unpriced)
		}
		return q.SendBatch(ctx, batch).Close()
	})
	if err != nil {
		return fmt.Errorf("postgres: Usage.AddRows: %w", err)
	}
	return nil
}

// QueryRows implements store.Usage: the hourly rows inside the range
// that pass the filters, read from the table, grouped by metering.Group
// so the answer equals the memory store's and the pure fold's.
func (u usage) QueryRows(ctx context.Context, q metering.Query) ([]metering.Row, error) {
	// An hour that overlaps the range is inside it: bucket < to and
	// bucket + 1h > from, the latter spelled so the bucket index serves it.
	where := []string{"bucket < $1", "bucket > $2"}
	args := []any{q.To, q.From.Add(-time.Hour)}
	add := func(column string, values []string) {
		if len(values) > 0 {
			args = append(args, values)
			where = append(where, column+" = ANY($"+strconv.Itoa(len(args))+"::text[])")
		}
	}
	add("key_id", q.Keys)
	add("model_id", q.Models)
	add("provider_id", q.Providers)
	add("owner", q.Owners)
	if len(q.Labels) > 0 {
		args = append(args, q.Labels)
		where = append(where, "labels @> $"+strconv.Itoa(len(args))+"::jsonb")
	}
	var matched []metering.Aggregate
	err := u.s.read(ctx, func(qr querier) error {
		hourly, err := scanAggregates(qr.Query(ctx, `SELECT `+aggregateColumns+` FROM usage_hourly WHERE `+strings.Join(where, " AND "), args...))
		if err != nil {
			return err
		}
		matched = append(matched, hourly...)
		// A monthly row overlaps the range when its month starts before
		// to and on or after the first instant of from's month; the same
		// filters, and MonthOverlaps, hold it to the rest.
		monthArgs := slices.Clone(args)
		monthArgs[1] = metering.IntervalMonth.Bucket(q.From).Add(-time.Nanosecond)
		monthly, err := scanAggregates(qr.Query(ctx, `SELECT `+aggregateColumns+` FROM usage_monthly WHERE `+strings.Join(where, " AND "), monthArgs...))
		if err != nil {
			return err
		}
		for _, a := range monthly {
			if q.MonthOverlaps(a) {
				matched = append(matched, a)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Usage.QueryRows: %w", err)
	}
	return metering.Group(matched, q.By, q.Interval), nil
}

// aggregateColumns are the columns of a usage row, in the order
// scanAggregates reads them.
const aggregateColumns = `bucket, key_id, model_id, provider_id, owner, door, status, currency, labels,
	requests, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, cost_micro, unpriced`

// scanAggregates reads every row of a query over aggregateColumns.
func scanAggregates(rows pgx.Rows, err error) ([]metering.Aggregate, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metering.Aggregate
	for rows.Next() {
		var a metering.Aggregate
		var door, status string
		if err := rows.Scan(&a.Bucket, &a.KeyID, &a.ModelID, &a.ProviderID, &a.Owner, &door, &status, &a.Currency, &a.Labels,
			&a.Requests, &a.InputTokens, &a.OutputTokens, &a.CachedInputTokens, &a.CacheWriteTokens, &a.Cost, &a.Unpriced); err != nil {
			return nil, err
		}
		a.Bucket, a.Door, a.Status = a.Bucket.UTC(), v1.Dialect(door), metering.Status(status)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Hourly implements store.Usage.
func (u usage) Hourly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	return u.page(ctx, "usage_hourly", before, after, limit)
}

// Monthly implements store.Usage.
func (u usage) Monthly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	return u.page(ctx, "usage_monthly", before, after, limit)
}

// page reads one table's rows before before, after after in key order,
// by the primary key's own order, which the keyset comparison follows.
func (u usage) page(ctx context.Context, table string, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	var out []metering.Aggregate
	err := u.s.read(ctx, func(qr querier) error {
		var err error
		out, err = scanAggregates(qr.Query(ctx, `SELECT `+aggregateColumns+` FROM `+table+`
			WHERE bucket < $1 AND (bucket, key_id, model_id, provider_id, owner, door, status, currency) > ($2, $3, $4, $5, $6, $7, $8, $9)
			ORDER BY bucket, key_id, model_id, provider_id, owner, door, status, currency LIMIT $10`,
			before, time.Unix(after.Bucket, 0).UTC(), after.KeyID, after.ModelID, after.ProviderID, after.Owner, string(after.Door), string(after.Status), after.Currency,
			nullLimit(pageLimit(limit))))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Usage page of %s: %w", table, err)
	}
	return out, nil
}

// upsertMonthly is the monthly table's add, the hourly table's upsert.
const upsertMonthly = `
	INSERT INTO usage_monthly (bucket, key_id, model_id, provider_id, owner, door, status, currency, labels,
		requests, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, cost_micro, unpriced)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	ON CONFLICT (bucket, key_id, model_id, provider_id, owner, door, status, currency) DO UPDATE SET
		labels = excluded.labels,
		requests = usage_monthly.requests + excluded.requests,
		input_tokens = usage_monthly.input_tokens + excluded.input_tokens,
		output_tokens = usage_monthly.output_tokens + excluded.output_tokens,
		cached_input_tokens = usage_monthly.cached_input_tokens + excluded.cached_input_tokens,
		cache_write_tokens = usage_monthly.cache_write_tokens + excluded.cache_write_tokens,
		cost_micro = usage_monthly.cost_micro + excluded.cost_micro,
		unpriced = usage_monthly.unpriced + excluded.unpriced`

// upsertHourly is AddRows's upsert, for a redaction's rewrite.
const upsertHourly = `
	INSERT INTO usage_hourly (bucket, key_id, model_id, provider_id, owner, door, status, currency, labels,
		requests, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, cost_micro, unpriced)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	ON CONFLICT (bucket, key_id, model_id, provider_id, owner, door, status, currency) DO UPDATE SET
		labels = excluded.labels,
		requests = usage_hourly.requests + excluded.requests,
		input_tokens = usage_hourly.input_tokens + excluded.input_tokens,
		output_tokens = usage_hourly.output_tokens + excluded.output_tokens,
		cached_input_tokens = usage_hourly.cached_input_tokens + excluded.cached_input_tokens,
		cache_write_tokens = usage_hourly.cache_write_tokens + excluded.cache_write_tokens,
		cost_micro = usage_hourly.cost_micro + excluded.cost_micro,
		unpriced = usage_hourly.unpriced + excluded.unpriced`

// upsertArgs are an upsert's sixteen arguments for a.
func upsertArgs(a metering.Aggregate) []any {
	labels := a.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return []any{a.Bucket.UTC(), a.KeyID, a.ModelID, a.ProviderID, a.Owner, string(a.Door), string(a.Status), a.Currency, labels,
		a.Requests, a.InputTokens, a.OutputTokens, a.CachedInputTokens, a.CacheWriteTokens, a.Cost, a.Unpriced}
}

// keyArgs are the eight primary key columns of k.
func keyArgs(k metering.AggregateKey) []any {
	return []any{time.Unix(k.Bucket, 0).UTC(), k.KeyID, k.ModelID, k.ProviderID, k.Owner, string(k.Door), string(k.Status), k.Currency}
}

const byKey = `bucket = $1 AND key_id = $2 AND model_id = $3 AND provider_id = $4 AND owner = $5 AND door = $6 AND status = $7 AND currency = $8`

// Fold implements store.Usage: each hourly row is deleted and its sums,
// as the delete returns them, are added into the monthly row, so a row
// another pass already folded adds nothing.
func (u usage) Fold(ctx context.Context, moves []metering.Move, expired []metering.AggregateKey) (folded, deleted int, err error) {
	err = u.s.write(ctx, func(tx pgx.Tx) error {
		folded, deleted = 0, 0
		for _, mv := range moves {
			var s metering.Sums
			err := tx.QueryRow(ctx, `DELETE FROM usage_hourly WHERE `+byKey+`
				RETURNING requests, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, cost_micro, unpriced`, keyArgs(mv.From)...).
				Scan(&s.Requests, &s.InputTokens, &s.OutputTokens, &s.CachedInputTokens, &s.CacheWriteTokens, &s.Cost, &s.Unpriced)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			to := mv.To
			to.Sums = s
			if _, err := tx.Exec(ctx, upsertMonthly, upsertArgs(to)...); err != nil {
				return err
			}
			folded++
		}
		for _, k := range expired {
			tag, err := tx.Exec(ctx, `DELETE FROM usage_monthly WHERE `+byKey, keyArgs(k)...)
			if err != nil {
				return err
			}
			deleted += int(tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("postgres: Usage.Fold: %w", err)
	}
	return folded, deleted, nil
}

// RedactOwner implements store.Usage: every row of the owner in both
// tables is deleted and its sums added into the row without an owner,
// and the ring's records lose the owner.
func (u usage) RedactOwner(ctx context.Context, owner string) (int, error) {
	if owner == "" {
		return 0, errors.New("postgres: Usage.RedactOwner with no owner")
	}
	n := 0
	err := u.s.write(ctx, func(tx pgx.Tx) error {
		n = 0
		for _, t := range []struct{ table, upsert string }{{"usage_hourly", upsertHourly}, {"usage_monthly", upsertMonthly}} {
			rows, err := scanAggregates(tx.Query(ctx, `DELETE FROM `+t.table+` WHERE owner = $1 RETURNING `+aggregateColumns, owner))
			if err != nil {
				return err
			}
			for _, a := range rows {
				a.Owner = ""
				if _, err := tx.Exec(ctx, t.upsert, upsertArgs(a)...); err != nil {
					return err
				}
			}
			n += len(rows)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: Usage.RedactOwner: %w", err)
	}
	if _, err := u.s.ring.Usage().RedactOwner(ctx, owner); err != nil {
		return 0, err
	}
	return n, nil
}

// AppendRecord implements store.Usage: the process's own ring, which is
// the memory store's.
func (u usage) AppendRecord(ctx context.Context, r metering.Record) error {
	return u.s.ring.Usage().AppendRecord(ctx, r)
}

// Records implements store.Usage over the same ring.
func (u usage) Records(ctx context.Context, q metering.RecordQuery, p store.Page) ([]metering.Record, string, error) {
	return u.s.ring.Usage().Records(ctx, q, p)
}

var _ store.Usage = usage{}
