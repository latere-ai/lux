// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// The roll-up of spec 038: on the replica holding the usage lease, every
// hour, the hourly rows past their rule's hourly retention are folded
// into one row per month and the monthly rows past their rule's monthly
// retention are deleted.
const (
	// MetricUsageRolledUp counts hourly rows folded, monthly rows
	// deleted, and passes that failed.
	MetricUsageRolledUp = "lux_usage_rolled_up_total"
	// DefaultRetentionInterval is how often a pass runs.
	DefaultRetentionInterval = time.Hour
	// retentionBatch is the rows one page reads and one transaction
	// folds.
	retentionBatch = 1000
)

// RetentionOptions is what the roll-up runs under.
type RetentionOptions struct {
	Store store.Store
	// Rules are LUX_USAGE_RETENTION; none makes every pass a no-op.
	Rules metering.RetentionRules
	// Interval is how often a pass runs; zero is DefaultRetentionInterval.
	Interval time.Duration
	// Batch is the rows of one page; zero is a thousand.
	Batch int
	// Holder names this replica in the lease row; empty is the host and
	// the process id.
	Holder string
	// Metrics receives MetricUsageRolledUp; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Retention is the roll-up job on one replica.
type Retention struct {
	o      RetentionOptions
	rolled *metrics.Counter
}

// NewRetention constructs the job; Run starts it.
func NewRetention(o RetentionOptions) *Retention {
	if o.Interval <= 0 {
		o.Interval = DefaultRetentionInterval
	}
	if o.Batch <= 0 {
		o.Batch = retentionBatch
	}
	if o.Holder == "" {
		o.Holder = defaultHolder()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	r := &Retention{o: o}
	if o.Metrics != nil {
		r.rolled = o.Metrics.Counter(MetricUsageRolledUp, "Hourly usage rows folded into monthly ones, monthly rows deleted, and roll-up passes that failed.")
		for _, result := range []string{"folded", "deleted", "error"} {
			r.rolled.Add(map[string]string{"result": result}, 0)
		}
	}
	return r
}

// Run passes at once and then every Interval until ctx ends, on the
// replica that holds the usage lease; with no rule it returns at once.
func (r *Retention) Run(ctx context.Context) {
	if len(r.o.Rules) == 0 {
		return
	}
	t := time.NewTicker(r.o.Interval)
	defer t.Stop()
	for {
		if err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.count("error", 1)
			r.o.Logger.ErrorContext(ctx, "retention: a roll-up pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass is one roll-up, when this replica holds the usage lease: every
// hourly row past its rule's hourly retention folded, then every
// monthly row past its rule's monthly retention deleted. The lease is
// renewed before every page, and a pass that loses it stops.
func (r *Retention) Pass(ctx context.Context) error {
	now := r.o.Now()
	if d, ok := r.o.Rules.ShortestHourly(); ok {
		err := r.pages(ctx, now.Add(-d), r.o.Store.Usage().Hourly, func(rows []metering.Aggregate) error {
			var moves []metering.Move
			for _, a := range rows {
				if rule := r.o.Rules.For(a.Labels); rule.HourlyDue(a, now) {
					moves = append(moves, metering.Move{From: a.Key(), To: rule.MonthlyRow(a)})
				}
			}
			if len(moves) == 0 {
				return nil
			}
			folded, _, err := r.o.Store.Usage().Fold(ctx, moves, nil)
			r.count("folded", folded)
			return err
		})
		if err != nil {
			return fmt.Errorf("folding the hourly rows: %w", err)
		}
	}
	if d, ok := r.o.Rules.ShortestMonthly(); ok {
		err := r.pages(ctx, now.Add(-d), r.o.Store.Usage().Monthly, func(rows []metering.Aggregate) error {
			var expired []metering.AggregateKey
			for _, a := range rows {
				if r.o.Rules.For(a.Labels).MonthlyDue(a, now) {
					expired = append(expired, a.Key())
				}
			}
			if len(expired) == 0 {
				return nil
			}
			_, deleted, err := r.o.Store.Usage().Fold(ctx, nil, expired)
			r.count("deleted", deleted)
			return err
		})
		if err != nil {
			return fmt.Errorf("deleting the monthly rows: %w", err)
		}
	}
	return nil
}

// pages reads one table's rows before before, a page at a time in key
// order, and hands each page to act while this replica holds the lease.
func (r *Retention) pages(ctx context.Context, before time.Time, read func(context.Context, time.Time, metering.AggregateKey, int) ([]metering.Aggregate, error), act func([]metering.Aggregate) error) error {
	var after metering.AggregateKey
	for {
		held, err := r.o.Store.Leases().Acquire(ctx, store.LeaseUsage, r.o.Holder, store.LeaseTTL)
		if err != nil {
			return fmt.Errorf("acquiring the usage lease: %w", err)
		}
		if !held {
			return nil
		}
		rows, err := read(ctx, before, after, r.o.Batch)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := act(rows); err != nil {
				return err
			}
			after = rows[len(rows)-1].Key()
		}
		if len(rows) < r.o.Batch {
			return nil
		}
	}
}

// count adds n to the metric under result.
func (r *Retention) count(result string, n int) {
	if r.rolled != nil && n > 0 {
		r.rolled.Add(map[string]string{"result": result}, uint64(n))
	}
}
