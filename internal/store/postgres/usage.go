// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
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
		rows, err := qr.Query(ctx, `
			SELECT bucket, key_id, model_id, provider_id, owner, door, status, currency, labels,
				requests, input_tokens, output_tokens, cached_input_tokens, cache_write_tokens, cost_micro, unpriced
			FROM usage_hourly WHERE `+strings.Join(where, " AND "), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a metering.Aggregate
			var door, status string
			if err := rows.Scan(&a.Bucket, &a.KeyID, &a.ModelID, &a.ProviderID, &a.Owner, &door, &status, &a.Currency, &a.Labels,
				&a.Requests, &a.InputTokens, &a.OutputTokens, &a.CachedInputTokens, &a.CacheWriteTokens, &a.Cost, &a.Unpriced); err != nil {
				return err
			}
			a.Bucket, a.Door, a.Status = a.Bucket.UTC(), v1.Dialect(door), metering.Status(status)
			matched = append(matched, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: Usage.QueryRows: %w", err)
	}
	return metering.Group(matched, q.By, q.Interval), nil
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
