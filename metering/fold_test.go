// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// corpus is a deterministic set of records across two days, three
// Keys of two owners, two Models, two Providers, four doors, the three
// statuses, and two currencies, with request labels no Key carries.
func corpus(n int) []Record {
	rng := rand.New(rand.NewPCG(9, 9))
	keys := []struct {
		id, owner string
		labels    map[string]string
	}{
		{"key_A", "https://login.example.com|alice", map[string]string{"team": "red", "env": "prod"}},
		{"key_B", "https://login.example.com|alice", map[string]string{"team": "blue"}},
		{"key_C", "https://login.example.com|bob", map[string]string{"team": "red"}},
	}
	doors := []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux}
	start := time.Date(2026, 9, 13, 22, 15, 0, 0, time.UTC)
	out := make([]Record, 0, n)
	for i := range n {
		k := keys[rng.IntN(len(keys))]
		r := Record{
			ID:            "req_" + strings.Repeat("0", 22) + string(rune('A'+i%26)) + string(rune('A'+i/26%26)),
			At:            start.Add(time.Duration(rng.IntN(26*60)) * time.Minute),
			Key:           KeyRef{ID: k.id, Prefix: "lux_" + k.id},
			Owner:         k.owner,
			Door:          doors[rng.IntN(len(doors))],
			Labels:        k.labels,
			RequestLabels: map[string]string{"run": "r" + string(rune('0'+rng.IntN(3)))},
			Loss:          []string{},
			Attempts:      []Attempt{},
		}
		r.EndedAt = r.At.Add(time.Second)
		switch rng.IntN(4) {
		case 0:
			r.Status, r.Error = StatusRefused, "rate_limited"
		case 1:
			r.Status, r.Error, r.UpstreamStatus = StatusFailed, "upstream_error", 502
			r.Model, r.Provider = Ref{"gpt", "mdl_1"}, Ref{"oai", "prv_1"}
		default:
			r.Status = StatusOK
			if rng.IntN(2) == 0 {
				r.Model, r.Provider = Ref{"gpt", "mdl_1"}, Ref{"oai", "prv_1"}
				r.Tokens = Tokens{Input: int64(rng.IntN(1000)), Output: int64(rng.IntN(300)), CachedInput: int64(rng.IntN(100)), CacheWrite: int64(rng.IntN(50))}
				r.Cost = Charge{Amount: int64(rng.IntN(50_000)), Currency: "USD", Priced: true}
			} else {
				r.Model, r.Provider = Ref{"claude", "mdl_2"}, Ref{"ant", "prv_2"}
				r.Tokens = Tokens{Input: int64(rng.IntN(1000)), Output: int64(rng.IntN(300))}
				if rng.IntN(3) == 0 {
					r.Cost = Charge{} // an unpriced Model
				} else {
					r.Cost = Charge{Amount: int64(rng.IntN(50_000)), Currency: "EUR", Priced: true}
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// groupings is every by list the tests fold under, up to three deep
// and with a label dimension.
var groupings = [][]Dimension{
	nil,
	{DimensionKey},
	{DimensionModel},
	{DimensionProvider},
	{DimensionOwner},
	{DimensionDoor},
	{DimensionStatus},
	{"label:team"},
	{DimensionKey, DimensionModel},
	{DimensionOwner, "label:env", DimensionStatus},
	{DimensionDoor, DimensionModel, DimensionProvider},
}

// naive sums records straight into response rows, one row per bucket,
// currency, and dimension tuple, with none of Aggregates' hourly step,
// so the two paths to a row are independent.
func naive(rs []Record, by []Dimension, in Interval) map[string]Row {
	out := map[string]Row{}
	for _, r := range rs {
		a := AggregateOf(r)
		parts := []string{in.Bucket(r.At).UTC().Format(time.RFC3339)}
		dims := map[string]string{}
		for _, d := range by {
			parts = append(parts, a.value(d))
			dims[string(d)] = a.value(d)
		}
		k := strings.Join(append(parts, r.Cost.Currency), "|")
		row := out[k]
		row.Bucket, row.Dimensions, row.Currency = in.Bucket(r.At), dims, r.Cost.Currency
		row.Requests++
		switch r.Status {
		case StatusOK:
			row.OK++
		case StatusRefused:
			row.Refused++
		case StatusFailed:
			row.Failed++
		}
		row.InputTokens += r.Tokens.Input
		row.OutputTokens += r.Tokens.Output
		row.CachedInputTokens += r.Tokens.CachedInput
		row.CacheWriteTokens += r.Tokens.CacheWrite
		if r.Cost.Priced {
			row.Cost += r.Cost.Amount
		} else {
			row.UnpricedRequests++
		}
		out[k] = row
	}
	return out
}

// before reports whether a sorts before b: bucket, then each by
// dimension's value, then currency.
func before(a, b Row, by []Dimension) bool {
	if c := a.Bucket.Compare(b.Bucket); c != 0 {
		return c < 0
	}
	for _, d := range by {
		if x, y := a.Dimensions[string(d)], b.Dimensions[string(d)]; x != y {
			return x < y
		}
	}
	return a.Currency < b.Currency
}

func rowKey(r Row, by []Dimension) string {
	parts := []string{r.Bucket.UTC().Format(time.RFC3339)}
	for _, d := range by {
		parts = append(parts, r.Dimensions[string(d)])
	}
	return strings.Join(append(parts, r.Currency), "|")
}

// TestAggregatesMatchTheRecords: for every grouping and interval, the
// rows Fold answers equal the rows summed straight from the records,
// Aggregates is one row per hour per tuple whose sums are the records',
// and the order is fixed.
func TestAggregatesMatchTheRecords(t *testing.T) {
	rs := corpus(400)
	as := Aggregates(rs)
	if len(as) == 0 || !reflect.DeepEqual(as, Aggregates(rs)) {
		t.Fatal("Aggregates is not deterministic")
	}
	var total Sums
	seen := map[AggregateKey]bool{}
	for _, a := range as {
		if seen[a.Key()] {
			t.Fatalf("two hourly rows share a key: %+v", a.Key())
		}
		seen[a.Key()] = true
		if !a.Bucket.Equal(a.Bucket.Truncate(time.Hour)) {
			t.Fatalf("a bucket is not on the hour: %s", a.Bucket)
		}
		total.Add(a.Sums)
	}
	if total.Requests != int64(len(rs)) {
		t.Fatalf("the hourly rows hold %d requests, want %d", total.Requests, len(rs))
	}
	for _, by := range groupings {
		for _, in := range []Interval{IntervalNone, "", IntervalHour, IntervalDay, IntervalMonth} {
			got := Fold(rs, by, in)
			want := naive(rs, by, in)
			if len(got) != len(want) {
				t.Fatalf("by %v, %s: %d rows, want %d", by, in, len(got), len(want))
			}
			for i, row := range got {
				w, ok := want[rowKey(row, by)]
				if !ok || !reflect.DeepEqual(row, w) {
					t.Fatalf("by %v, %s: row %d\n got %+v\nwant %+v", by, in, i, row, w)
				}
				if row.OK+row.Refused+row.Failed != row.Requests {
					t.Fatalf("the statuses do not sum to the requests: %+v", row)
				}
				if i > 0 && !before(got[i-1], row, by) {
					t.Fatalf("rows out of order at %d: %s after %s", i, rowKey(row, by), rowKey(got[i-1], by))
				}
			}
			if !reflect.DeepEqual(got, Group(as, by, in)) {
				t.Fatalf("Fold and Group over Aggregates differ under by %v, %s", by, in)
			}
		}
	}
	// A total row under none: one per currency, bucket absent.
	rows := Fold(rs, nil, IntervalNone)
	if len(rows) != 3 {
		t.Fatalf("total rows = %d, want one per currency and one unpriced", len(rows))
	}
	b, _ := json.Marshal(rows[0])
	if strings.Contains(string(b), `"bucket"`) || !strings.Contains(string(b), `"dimensions":{}`) {
		t.Fatalf("a total row renders %s", b)
	}
	b, _ = json.Marshal(Fold(rs, []Dimension{DimensionDoor}, IntervalDay)[0])
	if !strings.Contains(string(b), `"bucket":"2026-09-13T00:00:00Z"`) || !strings.Contains(string(b), `"dimensions":{"door":"`) {
		t.Fatalf("a day row renders %s", b)
	}
}

// TestNoCurrencyIsSummed: two priced records in two currencies in one
// hour under one Key fold to two rows whatever the grouping, each with
// its own currency, and an unpriced record is a third row with an empty
// currency and its request counted as unpriced.
func TestNoCurrencyIsSummed(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 5, 0, 0, time.UTC)
	rs := []Record{
		{ID: "req_1", At: at, Key: KeyRef{ID: "key_1"}, Status: StatusOK, Cost: Charge{Amount: 100, Currency: "USD", Priced: true}},
		{ID: "req_2", At: at, Key: KeyRef{ID: "key_1"}, Status: StatusOK, Cost: Charge{Amount: 200, Currency: "EUR", Priced: true}},
		{ID: "req_3", At: at, Key: KeyRef{ID: "key_1"}, Status: StatusOK, Cost: Charge{Amount: 300, Currency: "USD", Priced: true}},
		{ID: "req_4", At: at, Key: KeyRef{ID: "key_1"}, Status: StatusOK},
	}
	for _, by := range groupings {
		rows := Fold(rs, by, IntervalNone)
		if len(rows) != 3 {
			t.Fatalf("by %v: %d rows, want 3", by, len(rows))
		}
		sums := map[string]int64{}
		for _, r := range rows {
			sums[r.Currency] += r.Cost
			if r.Currency == "" && (r.UnpricedRequests != 1 || r.Cost != 0) {
				t.Fatalf("the unpriced row = %+v", r)
			}
		}
		if sums["USD"] != 400 || sums["EUR"] != 200 || sums[""] != 0 {
			t.Fatalf("by %v: sums %v", by, sums)
		}
	}
}

// TestRequestLabelsAreNotAggregated: a record's requestLabels reach no
// aggregate row, the hourly row's type has no member for them, and
// grouping by a label only the request carried yields one row with an
// empty value, never one row per request label.
func TestRequestLabelsAreNotAggregated(t *testing.T) {
	rs := corpus(200)
	if _, has := reflect.TypeFor[Aggregate]().FieldByName("RequestLabels"); has {
		t.Fatal("Aggregate carries requestLabels")
	}
	for _, a := range Aggregates(rs) {
		if _, has := a.Labels["run"]; has {
			t.Fatalf("a request label reached an aggregate row: %v", a.Labels)
		}
	}
	rows := Fold(rs, []Dimension{"label:run"}, IntervalNone)
	for _, r := range rows {
		if r.Dimensions["label:run"] != "" {
			t.Fatalf("a request label became a dimension value: %+v", r)
		}
	}
	if len(rows) != 3 { // one per currency, the label empty on each
		t.Fatalf("%d rows under label:run, want 3", len(rows))
	}
	b, _ := json.Marshal(rows)
	if strings.Contains(string(b), "requestLabels") || strings.Contains(string(b), `"r0"`) {
		t.Fatalf("a request label reached a response row: %s", b)
	}
}

// TestIntervalBuckets: the four widths in UTC, and none is the zero
// time.
func TestIntervalBuckets(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 17, 42, 0, time.FixedZone("east", 3600))
	if b := IntervalHour.Bucket(at); !b.Equal(time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("hour = %s", b)
	}
	if b := IntervalDay.Bucket(at); !b.Equal(time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day = %s", b)
	}
	if b := IntervalMonth.Bucket(at); !b.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month = %s", b)
	}
	if !IntervalNone.Bucket(at).IsZero() || !Interval("").Bucket(at).IsZero() {
		t.Error("none is not the zero time")
	}
	if Interval("week").Valid() || !Interval("").Valid() {
		t.Error("Valid")
	}
	if !Dimension("label:team").Valid() || Dimension("label:").Valid() || Dimension("currency").Valid() {
		t.Error("Dimension.Valid")
	}
	if v := (Aggregate{}).value("currency"); v != "" {
		t.Errorf("an unknown dimension's value = %q", v)
	}
}

// TestUsageQueryValidation: a range past ninety days, more than three
// by dimensions, an unknown dimension, a dimension named twice, an
// unknown interval, and a to before from are refused at their
// parameter; a cost in a row is an int64; the defaults are the last
// day.
func TestUsageQueryValidation(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	base := Query{From: now.Add(-time.Hour), To: now}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		q     Query
		field string
	}{
		{"no range", Query{}, "from"},
		{"to before from", Query{From: now, To: now.Add(-time.Hour)}, "to"},
		{"to equal to from", Query{From: now, To: now}, "to"},
		{"ninety-one days", Query{From: now.Add(-91 * 24 * time.Hour), To: now}, "to"},
		{"four dimensions", Query{From: base.From, To: base.To, By: []Dimension{DimensionKey, DimensionModel, DimensionProvider, DimensionOwner}}, "by"},
		{"an unknown dimension", Query{From: base.From, To: base.To, By: []Dimension{"currency"}}, "by"},
		{"a bare label", Query{From: base.From, To: base.To, By: []Dimension{"label:"}}, "by"},
		{"a dimension twice", Query{From: base.From, To: base.To, By: []Dimension{DimensionKey, DimensionKey}}, "by"},
		{"an unknown interval", Query{From: base.From, To: base.To, Interval: "week"}, "interval"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.q.Validate()
			var qe *QueryError
			if !errors.As(err, &qe) || qe.Field != c.field || qe.Detail == "" || !strings.HasPrefix(err.Error(), c.field+": ") {
				t.Fatalf("Validate = %v, want a QueryError at %s", err, c.field)
			}
		})
	}
	ninety := Query{From: now.Add(-MaxRange), To: now, By: []Dimension{DimensionKey, "label:team", DimensionStatus}, Interval: IntervalMonth}
	if err := ninety.Validate(); err != nil {
		t.Fatalf("ninety days and three dimensions: %v", err)
	}
	q := Query{}.WithDefaults(now)
	if !q.To.Equal(now) || !q.From.Equal(now.Add(-DefaultRange)) {
		t.Fatalf("defaults = %s .. %s", q.From, q.To)
	}
	q = Query{To: now.Add(-time.Hour)}.WithDefaults(now)
	if !q.To.Equal(now.Add(-time.Hour)) || !q.From.Equal(q.To.Add(-DefaultRange)) {
		t.Fatalf("defaults with to = %s .. %s", q.From, q.To)
	}
	if f, _ := reflect.TypeFor[Row]().FieldByName("Cost"); f.Type.Kind() != reflect.Int64 {
		t.Fatalf("Row.Cost is a %s", f.Type)
	}
	if f, _ := reflect.TypeFor[Charge]().FieldByName("Amount"); f.Type.Kind() != reflect.Int64 {
		t.Fatalf("Charge.Amount is a %s", f.Type)
	}
	b, _ := json.Marshal(Row{Cost: 1_250_000, Currency: "USD"})
	if !strings.Contains(string(b), `"cost":1250000`) {
		t.Fatalf("a row renders its cost as %s", b)
	}
}

// TestUsageFilterNarrows is the function half of the row: the
// authorizer's filter of one owner leaves a query naming another
// owner's Key selecting nothing, a filter's labels join the query's,
// and a label the query names with another value selects nothing; the
// route half is spec 011's.
func TestUsageFilterNarrows(t *testing.T) {
	const alice, bob = "https://login.example.com|alice", "https://login.example.com|bob"
	rs := corpus(300)
	as := Aggregates(rs)
	count := func(q Query) int {
		n := 0
		for _, a := range as {
			if q.Matches(a) {
				n++
			}
		}
		return n
	}
	all := Query{From: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)}
	if count(all) != len(as) {
		t.Fatal("the open query does not select every row")
	}
	// Bob's Key under alice's filter: nothing.
	q, ok := Intersect(Query{From: all.From, To: all.To, Keys: []string{"key_C"}}, []string{alice}, nil)
	if !ok || count(q) != 0 {
		t.Fatalf("bob's Key under alice's filter selects %d rows (ok %v)", count(q), ok)
	}
	// Alice's Keys under her filter: the same as without it.
	q, _ = Intersect(Query{From: all.From, To: all.To, Keys: []string{"key_A", "key_B"}}, []string{alice}, nil)
	if count(q) != count(Query{From: all.From, To: all.To, Keys: []string{"key_A", "key_B"}}) || count(q) == 0 {
		t.Fatal("alice's filter narrowed her own Keys")
	}
	// A query naming bob under alice's filter has nothing in common.
	if _, ok := Intersect(Query{Owners: []string{bob}}, []string{alice}, nil); ok {
		t.Fatal("an owners list with nothing in common is not empty")
	}
	q, ok = Intersect(Query{Owners: []string{bob, alice}}, []string{alice}, nil)
	if !ok || !reflect.DeepEqual(q.Owners, []string{alice}) {
		t.Fatalf("the intersection = %v, %v", q.Owners, ok)
	}
	// Labels join, and a conflicting value is empty.
	q, ok = Intersect(Query{Labels: map[string]string{"team": "red"}}, nil, map[string]string{"env": "prod"})
	if !ok || !reflect.DeepEqual(q.Labels, map[string]string{"team": "red", "env": "prod"}) {
		t.Fatalf("labels = %v, %v", q.Labels, ok)
	}
	if _, ok := Intersect(Query{Labels: map[string]string{"team": "red"}}, nil, map[string]string{"team": "blue"}); ok {
		t.Fatal("a conflicting label is not empty")
	}
	q, _ = Intersect(Query{From: all.From, To: all.To}, nil, map[string]string{"team": "red", "env": "prod"})
	for _, a := range as {
		if q.Matches(a) && a.KeyID != "key_A" {
			t.Fatalf("the label filter admitted %s", a.KeyID)
		}
	}
	if count(q) == 0 {
		t.Fatal("the label filter selects nothing")
	}
	// An empty filter changes nothing.
	q, ok = Intersect(all, nil, nil)
	if !ok || !reflect.DeepEqual(q, all) {
		t.Fatal("an empty filter changed the query")
	}
	// The other filters and the range.
	hour := as[0].Bucket
	inHour := Query{From: hour.Add(30 * time.Minute), To: hour.Add(45 * time.Minute)}
	for _, a := range as {
		if inHour.Matches(a) != a.Bucket.Equal(hour) {
			t.Fatalf("a from inside the hour reads %s under %s", a.Bucket, hour)
		}
	}
	if count(Query{From: all.From, To: all.To, Models: []string{"mdl_2"}, Providers: []string{"prv_2"}, Owners: []string{bob}}) == 0 {
		t.Fatal("the model, provider, and owner filters select nothing")
	}
	if count(Query{From: all.From, To: all.To, Models: []string{"mdl_2"}, Providers: []string{"prv_1"}}) != 0 {
		t.Fatal("a Model on the wrong Provider matched")
	}
	// The record query: the range on At, the filters, and the record's
	// own three parameters.
	stream := true
	rq := RecordQuery{Query: all, Status: StatusFailed, Error: "upstream_error"}
	n := 0
	for _, r := range rs {
		if rq.Matches(r) {
			n++
			if r.Status != StatusFailed {
				t.Fatalf("the status filter admitted %s", r.Status)
			}
		}
	}
	if n == 0 {
		t.Fatal("the record query selects nothing")
	}
	rq.Stream = &stream
	for _, r := range rs {
		if rq.Matches(r) {
			t.Fatal("a stream filter admitted a non-stream record")
		}
	}
	if (RecordQuery{Query: Query{From: rs[0].At.Add(time.Second), To: rs[0].At.Add(time.Hour)}}).Matches(rs[0]) {
		t.Fatal("a record before from matched")
	}
	if (RecordQuery{Query: Query{From: rs[0].At.Add(-time.Hour), To: rs[0].At}}).Matches(rs[0]) {
		t.Fatal("a record at to matched; to is exclusive")
	}
	if !(RecordQuery{Query: Query{From: rs[0].At, To: rs[0].At.Add(time.Second)}}).Matches(rs[0]) {
		t.Fatal("a record at from did not match; from is inclusive")
	}
	if (RecordQuery{Query: Query{From: all.From, To: all.To, Keys: []string{"key_Z"}}}).Matches(rs[0]) {
		t.Fatal("an unknown Key matched")
	}
}
