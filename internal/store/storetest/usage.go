// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The usage cases of spec 009 the contract can prove: the hourly rows
// upsert and answer every grouping as the pure fold does, no row sums
// two currencies, and the ring per Key is bounded and pages newest
// first.

// usageStart is the first record's instant; the corpus spans three
// hours from it.
var usageStart = time.Date(2026, 9, 14, 8, 10, 0, 0, time.UTC)

// usageRecords is a fixed corpus: n records over three hours, two Keys
// of two owners, two Models on two Providers, two currencies and an
// unpriced Model, the three statuses, and request labels no Key
// carries.
func usageRecords(n int) []metering.Record {
	out := make([]metering.Record, 0, n)
	for i := range n {
		r := metering.Record{
			ID:            "req_" + strconv.Itoa(1000+i),
			At:            usageStart.Add(time.Duration(i*7) * time.Minute % (3 * time.Hour)),
			Door:          v1.DialectOpenAI,
			Route:         "/openai/v1/chat/completions",
			Loss:          []string{},
			Attempts:      []metering.Attempt{},
			RequestLabels: map[string]string{"run": "r" + strconv.Itoa(i%3)},
		}
		r.EndedAt = r.At.Add(time.Second)
		if i%2 == 0 {
			r.Key, r.Owner, r.Labels = metering.KeyRef{ID: "key_A", Prefix: "lux_aaaaaaaa"}, subject, map[string]string{"team": "red"}
		} else {
			r.Key, r.Owner, r.Labels = metering.KeyRef{ID: "key_B", Prefix: "lux_bbbbbbbb"}, "https://login.example.com|bob", map[string]string{"team": "blue"}
		}
		switch i % 5 {
		case 0:
			r.Status, r.Error = metering.StatusRefused, "rate_limited"
		case 1:
			r.Status, r.Error, r.UpstreamStatus = metering.StatusFailed, "upstream_error", 502
			r.Model, r.Provider = metering.Ref{Name: "gpt", ID: "mdl_1"}, metering.Ref{Name: "oai", ID: "prv_1"}
		case 2:
			r.Status = metering.StatusOK
			r.Model, r.Provider = metering.Ref{Name: "gpt", ID: "mdl_1"}, metering.Ref{Name: "oai", ID: "prv_1"}
			r.Tokens = metering.Tokens{Input: int64(100 + i), Output: int64(10 + i), CachedInput: int64(i % 7)}
			r.Cost = metering.Charge{Amount: int64(1000 * (i + 1)), Currency: "USD", Priced: true}
		case 3:
			r.Status = metering.StatusOK
			r.Model, r.Provider = metering.Ref{Name: "claude", ID: "mdl_2"}, metering.Ref{Name: "ant", ID: "prv_2"}
			r.Tokens = metering.Tokens{Input: int64(50 + i), Output: int64(5 + i), CacheWrite: 3}
			r.Cost = metering.Charge{Amount: int64(700 * (i + 1)), Currency: "EUR", Priced: true}
		default:
			r.Status = metering.StatusOK
			r.Model, r.Provider = metering.Ref{Name: "free", ID: "mdl_3"}, metering.Ref{Name: "ant", ID: "prv_2"}
			r.Tokens = metering.Tokens{Input: 10, Output: 1, Estimated: true}
		}
		out = append(out, r)
	}
	return out
}

// aggregatesMatchTheRecords: rows added in two halves upsert on their
// key, every grouping and interval the store answers equals the pure
// fold of the records, a range inside an hour reads that hour, the
// filters select, a second add replaces the labels, and a row without a
// bucket is refused.
func aggregatesMatchTheRecords(t *testing.T, s store.Store) {
	ctx := t.Context()
	rs := usageRecords(120)
	noErr(t, s.Usage().AddRows(ctx, metering.Aggregates(rs[:70])), "AddRows first half")
	noErr(t, s.Usage().AddRows(ctx, metering.Aggregates(rs[70:])), "AddRows second half")
	noErr(t, s.Usage().AddRows(ctx, nil), "AddRows nothing")
	all := metering.Query{From: usageStart.Add(-time.Hour), To: usageStart.Add(4 * time.Hour)}
	for _, by := range [][]metering.Dimension{
		nil, {metering.DimensionKey}, {metering.DimensionModel, metering.DimensionStatus},
		{metering.DimensionOwner, "label:team", metering.DimensionDoor}, {metering.DimensionProvider}, {"label:run"},
	} {
		for _, in := range []metering.Interval{metering.IntervalNone, metering.IntervalHour, metering.IntervalDay, metering.IntervalMonth} {
			q := all
			q.By, q.Interval = by, in
			got, err := s.Usage().QueryRows(ctx, q)
			noErr(t, err, "QueryRows")
			want := metering.Fold(rs, by, in)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("by %v, %s:\n got %+v\nwant %+v", by, in, got, want)
			}
		}
	}
	// A from inside the second hour reads the second and third hours.
	q := all
	q.From, q.Interval = usageStart.Add(90*time.Minute), metering.IntervalHour
	rows, err := s.Usage().QueryRows(ctx, q)
	noErr(t, err, "QueryRows inside an hour")
	var later []metering.Record
	for _, r := range rs {
		if !r.At.Before(usageStart.Add(time.Hour).Truncate(time.Hour)) {
			later = append(later, r)
		}
	}
	if !reflect.DeepEqual(rows, metering.Fold(later, nil, metering.IntervalHour)) {
		t.Fatalf("a range inside an hour answered %+v", rows)
	}
	// The filters.
	q = all
	q.Keys, q.Models, q.Owners, q.Labels = []string{"key_A"}, []string{"mdl_1"}, []string{subject}, map[string]string{"team": "red"}
	rows, err = s.Usage().QueryRows(ctx, q)
	noErr(t, err, "QueryRows filtered")
	var filtered []metering.Record
	for _, r := range rs {
		if r.Key.ID == "key_A" && r.Model.ID == "mdl_1" {
			filtered = append(filtered, r)
		}
	}
	if !reflect.DeepEqual(rows, metering.Fold(filtered, nil, metering.IntervalNone)) || len(rows) == 0 {
		t.Fatalf("filtered rows = %+v", rows)
	}
	q = all
	q.Providers = []string{"prv_9"}
	rows, err = s.Usage().QueryRows(ctx, q)
	noErr(t, err, "QueryRows unknown provider")
	equal(t, len(rows), 0, "rows of an unknown Provider")
	// A second add of a row replaces its labels and adds its sums.
	relabelled := metering.Aggregates(rs[:1])[0]
	relabelled.Labels = map[string]string{"team": "green"}
	noErr(t, s.Usage().AddRows(ctx, []metering.Aggregate{relabelled}), "AddRows relabelled")
	q = all
	q.By = []metering.Dimension{"label:team"}
	q.Keys = []string{"key_A"}
	rows, err = s.Usage().QueryRows(ctx, q)
	noErr(t, err, "QueryRows relabelled")
	var green, red int64
	for _, r := range rows {
		switch r.Dimensions["label:team"] {
		case "green":
			green += r.Requests
		case "red":
			red += r.Requests
		}
	}
	// The hour's row of that tuple now reads green, with one more request.
	truth(t, green > 1 && red > 0, "the relabelled row reads its new label with its sums added")
	// A row without a bucket is a caller's mistake.
	truth(t, s.Usage().AddRows(ctx, []metering.Aggregate{{KeyID: "key_A"}}) != nil, "AddRows without a bucket is refused")
}

// noCurrencyIsSummed: rows in two currencies in one hour under one Key
// answer as two rows whatever the grouping, and an unpriced row is a
// third with an empty currency.
func noCurrencyIsSummed(t *testing.T, s store.Store) {
	ctx := t.Context()
	hour := usageStart.Truncate(time.Hour)
	base := metering.Aggregate{Bucket: hour, KeyID: "key_1", ModelID: "mdl_1", ProviderID: "prv_1", Owner: subject, Door: v1.DialectOpenAI, Status: metering.StatusOK}
	usd, eur, none := base, base, base
	usd.Currency, usd.Sums = "USD", metering.Sums{Requests: 2, Cost: 300}
	eur.Currency, eur.Sums = "EUR", metering.Sums{Requests: 1, Cost: 200}
	none.Sums = metering.Sums{Requests: 1, Unpriced: 1}
	noErr(t, s.Usage().AddRows(ctx, []metering.Aggregate{usd, eur, none}), "AddRows")
	for _, by := range [][]metering.Dimension{nil, {metering.DimensionKey}, {metering.DimensionModel, metering.DimensionProvider, metering.DimensionStatus}} {
		rows, err := s.Usage().QueryRows(ctx, metering.Query{From: hour, To: hour.Add(time.Hour), By: by})
		noErr(t, err, "QueryRows")
		equal(t, len(rows), 3, "rows per currency")
		sums := map[string]int64{}
		for _, r := range rows {
			sums[r.Currency] += r.Cost
			if r.Currency == "" {
				equal(t, r.UnpricedRequests, 1, "the unpriced row's requests")
			}
		}
		equal(t, sums["USD"], 300, "USD")
		equal(t, sums["EUR"], 200, "EUR")
		equal(t, sums[""], 0, "unpriced cost")
	}
}

// recordsRingIsBounded: a Key's ring keeps the newest RecordsPerKey
// records, Records answers newest first and pages by cursor, the
// filters apply, a cursor from another query is ErrInvalidCursor, and a
// record without an id is refused.
func recordsRingIsBounded(t *testing.T, s store.Store) {
	ctx := t.Context()
	const extra = 5
	for i := range metering.RecordsPerKey + extra {
		r := metering.Record{ID: "req_" + strconv.Itoa(100000+i), At: usageStart.Add(time.Duration(i) * time.Second), Key: metering.KeyRef{ID: "key_ring"}, Owner: subject, Status: metering.StatusOK}
		noErr(t, s.Usage().AppendRecord(ctx, r), "AppendRecord")
	}
	other := metering.Record{ID: "req_other", At: usageStart.Add(time.Hour), Key: metering.KeyRef{ID: "key_other"}, Owner: "https://login.example.com|bob", Status: metering.StatusFailed, Error: "upstream_error", Stream: true}
	noErr(t, s.Usage().AppendRecord(ctx, other), "AppendRecord other")
	all := metering.RecordQuery{From: usageStart.Add(-time.Hour), To: usageStart.Add(2 * time.Hour)}
	recs, next, err := s.Usage().Records(ctx, all, store.Page{})
	noErr(t, err, "Records")
	equal(t, len(recs), metering.RecordsPerKey+1, "the ring plus the other Key's record")
	equal(t, next, "", "no cursor when every row is answered")
	equal(t, recs[0].ID, "req_other", "newest first")
	equal(t, recs[1].ID, "req_"+strconv.Itoa(100000+metering.RecordsPerKey+extra-1), "the newest of the ring")
	equal(t, recs[len(recs)-1].ID, "req_"+strconv.Itoa(100000+extra), "the oldest kept is the first past the drop")
	// Paging: three pages of 400 cover the rows once, in order.
	var paged []metering.Record
	cursor := ""
	for range 4 {
		page, n, err := s.Usage().Records(ctx, all, store.Page{Limit: 400, Cursor: cursor})
		noErr(t, err, "Records page")
		paged = append(paged, page...)
		if n == "" {
			break
		}
		cursor = n
	}
	truth(t, reflect.DeepEqual(paged, recs), "the pages equal the whole, in order")
	// Two records at one instant page without a gap or a repeat.
	tie := usageStart.Add(90 * time.Minute)
	for _, id := range []string{"req_tie_a", "req_tie_b", "req_tie_c"} {
		noErr(t, s.Usage().AppendRecord(ctx, metering.Record{ID: id, At: tie, Key: metering.KeyRef{ID: "key_tie"}, Status: metering.StatusOK}), "AppendRecord tie")
	}
	tied := metering.RecordQuery{From: tie, To: tie.Add(time.Second)}
	first, n, err := s.Usage().Records(ctx, tied, store.Page{Limit: 2})
	noErr(t, err, "Records tie page 1")
	second, n2, err := s.Usage().Records(ctx, tied, store.Page{Limit: 2, Cursor: n})
	noErr(t, err, "Records tie page 2")
	equal(t, len(first)+len(second), 3, "the tied records over two pages")
	equal(t, first[0].ID+first[1].ID+second[0].ID, "req_tie_creq_tie_breq_tie_a", "tied records by id descending")
	equal(t, n2, "", "no cursor after the last tied record")
	// The filters and the record's own parameters.
	stream := true
	filtered, _, err := s.Usage().Records(ctx, metering.RecordQuery{Query: all.Query, Status: metering.StatusFailed, Error: "upstream_error", Stream: &stream}, store.Page{})
	noErr(t, err, "Records filtered")
	equal(t, len(filtered), 1, "the failed stream")
	equal(t, filtered[0].ID, "req_other", "the failed stream's id")
	byKey, _, err := s.Usage().Records(ctx, metering.RecordQuery{From: all.From, To: all.To, Keys: []string{"key_other"}, Owners: []string{"https://login.example.com|bob"}}, store.Page{})
	noErr(t, err, "Records by Key")
	equal(t, len(byKey), 1, "the other Key's records")
	// A cursor over another query is refused; so is garbage.
	_, _, err = s.Usage().Records(ctx, metering.RecordQuery{From: all.From, To: all.To, Keys: []string{"key_ring"}}, store.Page{Limit: 10, Cursor: n})
	wantErr(t, err, store.ErrInvalidCursor, "a cursor from another query")
	_, _, err = s.Usage().Records(ctx, all, store.Page{Limit: 10, Cursor: "not-a-cursor!"})
	wantErr(t, err, store.ErrInvalidCursor, "garbage")
	_, _, err = s.Usage().Records(ctx, all, store.Page{Limit: 10, Cursor: store.EncodeCursor(store.JournalKind, store.Filter{}, "x")})
	wantErr(t, err, store.ErrInvalidCursor, "a journal cursor")
	truth(t, s.Usage().AppendRecord(ctx, metering.Record{Key: metering.KeyRef{ID: "key_ring"}}) != nil, "a record without an id is refused")
}

// sumOf is the requests and cost of rows, the totals a fold keeps.
func sumOf(rows []metering.Row) (requests, cost int64) {
	for _, r := range rows {
		requests += r.Requests
		cost += r.Cost
	}
	return requests, cost
}

// usageRollUp is spec 038's store half: the hourly rows page in key
// order after a key; a fold deletes each hourly row and adds the sums it
// held into its monthly row, once however often it runs; the usage
// query reads the monthly rows beside the hourly ones with the same
// totals; and expired monthly rows are deleted.
func usageRollUp(t *testing.T, s store.Store) {
	ctx := t.Context()
	rs := usageRecords(120)
	noErr(t, s.Usage().AddRows(ctx, metering.Aggregates(rs)), "AddRows")
	all := metering.Query{From: usageStart.Add(-time.Hour), To: usageStart.Add(4 * time.Hour)}
	before, err := s.Usage().QueryRows(ctx, all)
	noErr(t, err, "QueryRows before")
	wantRequests, wantCost := sumOf(before)

	// Paging: two rows at a time reads every row once, in key order.
	var paged []metering.Aggregate
	var after metering.AggregateKey
	for {
		page, err := s.Usage().Hourly(ctx, usageStart.Add(4*time.Hour), after, 2)
		noErr(t, err, "Hourly")
		paged = append(paged, page...)
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1].Key()
	}
	if len(paged) != len(metering.Aggregates(rs)) {
		t.Fatalf("paged %d hourly rows, want %d", len(paged), len(metering.Aggregates(rs)))
	}
	for i := 1; i < len(paged); i++ {
		if paged[i-1].Key().Compare(paged[i].Key()) >= 0 {
			t.Fatalf("rows %d and %d are out of key order", i-1, i)
		}
	}
	if early, err := s.Usage().Hourly(ctx, usageStart.Truncate(time.Hour), metering.AggregateKey{}, 0); err != nil || len(early) != 0 {
		t.Fatalf("rows before the first hour = %d, %v", len(early), err)
	}

	// Fold every row of team red into its month without the owner.
	rule := metering.RetentionRule{Match: map[string]string{"team": "red"}, Drop: []metering.Dimension{metering.DropOwner}}
	var moves []metering.Move
	for _, a := range paged {
		if a.Labels["team"] == "red" {
			moves = append(moves, metering.Move{From: a.Key(), To: rule.MonthlyRow(a)})
		}
	}
	folded, deleted, err := s.Usage().Fold(ctx, moves, nil)
	noErr(t, err, "Fold")
	if folded != len(moves) || deleted != 0 {
		t.Fatalf("Fold = %d folded, %d deleted, want %d and 0", folded, deleted, len(moves))
	}
	again, _, err := s.Usage().Fold(ctx, moves, nil)
	noErr(t, err, "Fold again")
	if again != 0 {
		t.Fatalf("a second fold of the same rows folded %d", again)
	}
	after2, err := s.Usage().QueryRows(ctx, all)
	noErr(t, err, "QueryRows after")
	if r, c := sumOf(after2); r != wantRequests || c != wantCost {
		t.Fatalf("after the fold the totals are %d requests and %d cost, want %d and %d", r, c, wantRequests, wantCost)
	}
	monthly, err := s.Usage().Monthly(ctx, usageStart.AddDate(0, 1, 0), metering.AggregateKey{}, 0)
	noErr(t, err, "Monthly")
	if len(monthly) == 0 {
		t.Fatal("no monthly row after the fold")
	}
	for _, a := range monthly {
		if a.Owner != "" || !a.Bucket.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || a.Labels["team"] != "red" {
			t.Fatalf("a monthly row = %+v", a)
		}
	}
	byOwner := all
	byOwner.By = []metering.Dimension{metering.DimensionOwner}
	grouped, err := s.Usage().QueryRows(ctx, byOwner)
	noErr(t, err, "QueryRows by owner")
	for _, row := range grouped {
		if row.Dimensions["owner"] == subject {
			t.Fatalf("the dropped owner is still a group: %+v", row)
		}
	}
	// A range that starts later in the month still reads the month's row.
	late := metering.Query{From: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)}
	lateRows, err := s.Usage().QueryRows(ctx, late)
	noErr(t, err, "QueryRows later in the month")
	if r, _ := sumOf(lateRows); r == 0 {
		t.Fatal("a range later in the month does not read the month's row")
	}
	expired := make([]metering.AggregateKey, 0, len(monthly))
	for _, a := range monthly {
		expired = append(expired, a.Key())
	}
	_, deleted, err = s.Usage().Fold(ctx, nil, expired)
	noErr(t, err, "Fold expiring")
	if deleted != len(monthly) {
		t.Fatalf("deleted %d monthly rows, want %d", deleted, len(monthly))
	}
	if left, err := s.Usage().Monthly(ctx, usageStart.AddDate(1, 0, 0), metering.AggregateKey{}, 0); err != nil || len(left) != 0 {
		t.Fatalf("monthly rows after the expiry = %d, %v", len(left), err)
	}
}

// usageRedactOwner is spec 038's redaction: every hourly and monthly row
// of the owner is summed into the row without an owner, the totals kept,
// the owner gone from every grouping and from the ring's records, and
// an owner no row carries rewrites nothing.
func usageRedactOwner(t *testing.T, s store.Store) {
	ctx := t.Context()
	rs := usageRecords(60)
	noErr(t, s.Usage().AddRows(ctx, metering.Aggregates(rs)), "AddRows")
	for _, r := range rs {
		noErr(t, s.Usage().AppendRecord(ctx, r), "AppendRecord")
	}
	// Some of the owner's rows are monthly already.
	var moves []metering.Move
	rule := metering.RetentionRule{}
	page, err := s.Usage().Hourly(ctx, usageStart.Add(time.Hour).Truncate(time.Hour), metering.AggregateKey{}, 0)
	noErr(t, err, "Hourly")
	for _, a := range page {
		moves = append(moves, metering.Move{From: a.Key(), To: rule.MonthlyRow(a)})
	}
	_, _, err = s.Usage().Fold(ctx, moves, nil)
	noErr(t, err, "Fold")
	all := metering.Query{From: usageStart.Add(-time.Hour), To: usageStart.Add(4 * time.Hour)}
	before, err := s.Usage().QueryRows(ctx, all)
	noErr(t, err, "QueryRows before")
	wantRequests, wantCost := sumOf(before)

	n, err := s.Usage().RedactOwner(ctx, subject)
	noErr(t, err, "RedactOwner")
	if n == 0 {
		t.Fatal("the redaction rewrote no row")
	}
	after, err := s.Usage().QueryRows(ctx, all)
	noErr(t, err, "QueryRows after")
	if r, c := sumOf(after); r != wantRequests || c != wantCost {
		t.Fatalf("after the redaction the totals are %d requests and %d cost, want %d and %d", r, c, wantRequests, wantCost)
	}
	byOwner := all
	byOwner.By = []metering.Dimension{metering.DimensionOwner}
	grouped, err := s.Usage().QueryRows(ctx, byOwner)
	noErr(t, err, "QueryRows by owner")
	for _, row := range grouped {
		if row.Dimensions["owner"] == subject {
			t.Fatalf("the redacted owner is still a group: %+v", row)
		}
	}
	recs, _, err := s.Usage().Records(ctx, metering.RecordQuery{Query: all}, store.Page{})
	noErr(t, err, "Records")
	for _, r := range recs {
		if r.Owner == subject {
			t.Fatalf("record %s still carries the owner", r.ID)
		}
	}
	if n, err := s.Usage().RedactOwner(ctx, "https://login.example.com|nobody"); err != nil || n != 0 {
		t.Fatalf("an owner no row carries = %d, %v", n, err)
	}
	if _, err := s.Usage().RedactOwner(ctx, ""); err == nil {
		t.Fatal("an empty owner was redacted")
	}
}
