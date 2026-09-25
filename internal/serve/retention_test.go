// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/metering"
)

// usageRow is one hourly row of a Key labeled context, hours before now.
func usageRow(now time.Time, hoursAgo int, key, owner, context string) metering.Aggregate {
	return metering.Aggregate{
		Bucket: now.Add(-time.Duration(hoursAgo) * time.Hour).Truncate(time.Hour), KeyID: key, Owner: owner,
		Labels: map[string]string{"context": context}, Requests: 1, Cost: 100,
	}
}

// TestRetentionPass is spec 038's roll-up: on the replica holding the
// usage lease, the hourly rows past their rule's hourly period fold into
// their month with the rule's drops, rows kept by their rule or by no
// rule stay hourly, monthly rows past their period are deleted, the
// usage totals are the same before and after, and a second replica's
// pass does nothing while the first holds the lease.
func TestRetentionPass(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	now := h.clock()
	personal := metering.RetentionRule{Match: map[string]string{"context": "personal"}, Hourly: metering.Retention(48 * time.Hour), Monthly: metering.Retention(720 * time.Hour), Drop: []metering.Dimension{metering.DropOwner}}
	rules := metering.RetentionRules{personal}
	rows := []metering.Aggregate{
		usageRow(now, 24*40, "key_old", "https://login.example.com|alice", "personal"),
		usageRow(now, 72, "key_due", "https://login.example.com|alice", "personal"),
		usageRow(now, 10, "key_fresh", "https://login.example.com|alice", "personal"),
		usageRow(now, 72, "key_team", "https://login.example.com|bob", "team"),
	}
	if err := h.st.Usage().AddRows(ctx, rows); err != nil {
		t.Fatal(err)
	}
	all := metering.Query{From: now.AddDate(0, 0, -89), To: now.Add(time.Hour)}
	before, err := Usage(ctx, h.st, all, now)
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	a := NewRetention(RetentionOptions{Store: h.st, Rules: rules, Batch: 1, Holder: "a", Metrics: reg, Logger: h.logger, Now: h.clock})
	b := NewRetention(RetentionOptions{Store: h.st, Rules: rules, Batch: 1, Holder: "b", Logger: h.logger, Now: h.clock})
	if err := a.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	hourly, err := h.st.Usage().Hourly(ctx, now.Add(time.Hour), metering.AggregateKey{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, r := range hourly {
		left[r.KeyID] = true
	}
	if len(left) != 2 || !left["key_fresh"] || !left["key_team"] {
		t.Fatalf("hourly rows left: %v", left)
	}
	monthly, err := h.st.Usage().Monthly(ctx, now.AddDate(0, 1, 0), metering.AggregateKey{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range monthly {
		if m.Owner != "" || m.Labels["context"] != "personal" {
			t.Fatalf("a monthly row kept the owner or lost the match: %+v", m)
		}
	}
	after, err := Usage(ctx, h.st, all, now)
	if err != nil {
		t.Fatal(err)
	}
	if sum(before) != sum(after) {
		t.Fatalf("the fold changed the totals: %d, then %d", sum(before), sum(after))
	}
	// The second replica does not hold the lease and does nothing.
	if err := h.st.Usage().AddRows(ctx, []metering.Aggregate{usageRow(now, 72, "key_later", "https://login.example.com|carol", "personal")}); err != nil {
		t.Fatal(err)
	}
	if err := b.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	stillHourly := false
	rest, _ := h.st.Usage().Hourly(ctx, now.Add(-71*time.Hour), metering.AggregateKey{}, 0)
	for _, r := range rest {
		if r.KeyID == "key_later" {
			stillHourly = true
		}
	}
	if !stillHourly {
		t.Fatal("the replica without the lease folded a row")
	}
	// Months later the monthly rows of the first fold have expired.
	h.advance(24 * 70 * time.Hour)
	if err := a.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	expiredMonths, err := h.st.Usage().Monthly(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), metering.AggregateKey{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(expiredMonths) != 0 {
		t.Fatalf("%d monthly rows of August 2026 and before survived their period", len(expiredMonths))
	}
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	for _, want := range []string{`lux_usage_rolled_up_total{result="folded"}`, `lux_usage_rolled_up_total{result="deleted"}`, `lux_usage_rolled_up_total{result="error"} 0`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the exposition lacks %s:\n%s", want, buf.String())
		}
	}
}

// sum is the requests of usage rows.
func sum(rows []metering.Row) int64 {
	var n int64
	for _, r := range rows {
		n += r.Requests
	}
	return n
}

// TestRetentionRun: with no rule Run returns at once; with rules it
// passes at start, and a store that fails the pass counts an error and
// keeps running until its context ends.
func TestRetentionRun(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewRetention(RetentionOptions{Store: h.st, Logger: h.logger}).Run(t.Context())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run with no rule did not return")
	}
	down := &atomic.Bool{}
	down.Store(true)
	reg := metrics.NewRegistry()
	rules := metering.RetentionRules{{Hourly: metering.Retention(24 * time.Hour)}}
	r := NewRetention(RetentionOptions{Store: &failingLeases{outage: &outage{Store: h.st, down: down}}, Rules: rules, Interval: 10 * time.Millisecond, Metrics: reg, Logger: h.logger})
	ctx, stop := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); r.Run(ctx) }()
	waitFor(t, func() bool {
		return reg.Counter(MetricUsageRolledUp, "").Value(map[string]string{"result": "error"}) > 0
	})
	stop()
	<-stopped
}

// failingLeases is a store whose lease is granted and whose usage reads
// fail while its outage is down.
type failingLeases struct{ *outage }

func (f *failingLeases) Usage() store.Usage { return failingHourly{f.Store.Usage(), f.down} }

type failingHourly struct {
	store.Usage
	down *atomic.Bool
}

func (u failingHourly) Hourly(ctx context.Context, before time.Time, after metering.AggregateKey, limit int) ([]metering.Aggregate, error) {
	if u.down.Load() {
		return nil, errOutage
	}
	return u.Usage.Hourly(ctx, before, after, limit)
}
