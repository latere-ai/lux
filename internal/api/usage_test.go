// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// countingStore counts the questions the two usage routes put to the
// store, so a test can hold an empty intersection to asking it none.
type countingStore struct {
	store.Store
	rows    int
	records int
	fail    error // what the two collections answer instead
}

func (s *countingStore) Usage() store.Usage { return &countingUsage{Usage: s.Store.Usage(), s: s} }

type countingUsage struct {
	store.Usage
	s *countingStore
}

func (u *countingUsage) QueryRows(ctx context.Context, q metering.Query) ([]metering.Row, error) {
	u.s.rows++
	if u.s.fail != nil {
		return nil, u.s.fail
	}
	return u.Usage.QueryRows(ctx, q)
}

func (u *countingUsage) Records(ctx context.Context, q metering.RecordQuery, p store.Page) ([]metering.Record, string, error) {
	u.s.records++
	if u.s.fail != nil {
		return nil, "", u.s.fail
	}
	return u.Usage.Records(ctx, q, p)
}

// usageHarness is the surface over a counting store with two Keys,
// alice's and bob's, an hourly aggregate row and three records each.
type usageHarness struct {
	*harness
	counts              *countingStore
	keyID, bobKeyID     string
	modelID, providerID string
	bob                 string // bob's rendered subject
	at                  time.Time
}

func newUsageHarness(t *testing.T) *usageHarness {
	t.Helper()
	counts := &countingStore{}
	h := newHarness(t, func(o *Options) {
		counts.Store = o.Store
		o.Store = counts
	})
	h.seed()
	if rec := h.request(http.MethodPut, "/v1/keys/run-42", `{"metadata": {"labels": {"team": "research"}}, "spec": {"models": ["gpt-5"]}}`); rec.Code != http.StatusCreated {
		t.Fatalf("alice's Key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.request(http.MethodPut, "/v1/keys/bob-1", `{"metadata": {"labels": {"team": "ops"}}, "spec": {"models": ["gpt-5"]}}`, as(h.bob)...); rec.Code != http.StatusCreated {
		t.Fatalf("bob's Key: %d %s", rec.Code, rec.Body.String())
	}
	u := &usageHarness{
		harness: h, counts: counts,
		keyID: h.objectOf(v1.KindKey, "run-42").ID(), bobKeyID: h.objectOf(v1.KindKey, "bob-1").ID(),
		modelID: h.objectOf(v1.KindModel, "gpt-5").ID(), providerID: h.objectOf(v1.KindProvider, "openai").ID(),
		bob: h.iss.URL() + "|bob", at: h.clock().Add(-time.Hour),
	}
	rows := []metering.Aggregate{
		{Bucket: u.at.Truncate(time.Hour), KeyID: u.keyID, ModelID: u.modelID, ProviderID: u.providerID, Owner: h.subject(),
			Door: v1.DialectOpenAI, Status: metering.StatusOK, Currency: "USD", Labels: map[string]string{"team": "research"},
			Sums: metering.Sums{Requests: 3, InputTokens: 30, OutputTokens: 60, Cost: 1250000}},
		{Bucket: u.at.Truncate(time.Hour), KeyID: u.bobKeyID, ModelID: u.modelID, ProviderID: u.providerID, Owner: u.bob,
			Door: v1.DialectOpenAI, Status: metering.StatusOK, Currency: "USD", Labels: map[string]string{"team": "ops"},
			Sums: metering.Sums{Requests: 1, InputTokens: 10, OutputTokens: 20, Cost: 500000}},
	}
	if err := h.st.Usage().AddRows(bg(), rows); err != nil {
		t.Fatal(err)
	}
	for i, r := range []struct {
		id     string
		key    string
		owner  string
		labels map[string]string
		status metering.Status
		code   string
		stream bool
	}{
		{"req_A", u.keyID, h.subject(), map[string]string{"team": "research"}, metering.StatusOK, "", false},
		{"req_B", u.keyID, h.subject(), map[string]string{"team": "research"}, metering.StatusRefused, "model_not_allowed", false},
		{"req_C", u.keyID, h.subject(), map[string]string{"team": "research"}, metering.StatusOK, "", true},
		{"req_D", u.bobKeyID, u.bob, map[string]string{"team": "ops"}, metering.StatusOK, "", false},
	} {
		at := u.at.Add(time.Duration(i) * time.Minute)
		rec := metering.Record{
			ID: r.id, At: at, EndedAt: at.Add(time.Second),
			Key:      metering.KeyRef{ID: r.key, Prefix: "lux_AAAAAAAA"},
			Owner:    r.owner,
			Model:    metering.Ref{Name: "gpt-5", ID: u.modelID},
			Provider: metering.Ref{Name: "openai", ID: u.providerID},
			Door:     v1.DialectOpenAI, Status: r.status, Error: r.code, Stream: r.stream,
			Tokens: metering.Tokens{Input: 10, Output: 20},
			Cost:   metering.Charge{Amount: 1250000, Currency: "USD", Priced: true},
			Labels: r.labels,
		}
		if err := h.st.Usage().AppendRecord(bg(), rec); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

// items reads the items of a usage or a requests response.
func items(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	raw, ok := body(t, rec)["items"].([]any)
	if !ok {
		t.Fatalf("items is not a list: %s", rec.Body.String())
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

// ids reads the record ids of a requests response, in order.
func ids(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	out := []string{}
	for _, it := range items(t, rec) {
		out = append(out, it["id"].(string))
	}
	return out
}

// TestUsageRoutesResolveNamesAndIntersectFilter: GET /v1/usage resolves
// a Key, Model, or Provider name to its id before the authorizer sees
// the resource, an id passes through, an unknown name is an empty items
// and no question for the store, and the authorizer's filter is
// intersected with the query rather than refusing it. GET /v1/requests
// resolves the same names. Spec 011's row.
func TestUsageRoutesResolveNamesAndIntersectFilter(t *testing.T) {
	h := newUsageHarness(t)
	h.stub.ClearRequests()
	rows := items(t, h.request(http.MethodGet, "/v1/usage?key=run-42&by=key", ""))
	if len(rows) != 1 || rows[0]["dimensions"].(map[string]any)["key"] != h.keyID {
		t.Fatalf("by key: %v", rows)
	}
	req := h.stub.Requests()[0]
	if req.Action != "usage.read" || req.Resource.Kind != "Usage" {
		t.Errorf("asked %s on %v", req.Action, req.Resource)
	}
	if !reflect.DeepEqual(req.Resource.Fields["keys"], []any{h.keyID}) || !reflect.DeepEqual(req.Resource.Fields["owners"], []any{}) {
		t.Errorf("the resource carries %v", req.Resource.Fields)
	}

	// An id is its own resolution, and a Model and a Provider resolve too.
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?key="+h.keyID+"&model=gpt-5&provider=openai", "")); len(rows) != 1 || rows[0]["requests"] != float64(3) {
		t.Errorf("by id: %v", rows)
	}
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?model="+h.modelID+"&provider="+h.providerID, "")); len(rows) != 1 || rows[0]["requests"] != float64(4) {
		t.Errorf("both Keys: %v", rows)
	}

	// A name that names nothing is an empty items, never not_found, and
	// the store is asked nothing.
	for _, query := range []string{"?key=no-such-key", "?model=no-such-model", "?provider=no-such-provider", "?key=" + v1.PrefixBudget + "01J9"} {
		before := h.counts.rows
		if rows := items(t, h.request(http.MethodGet, "/v1/usage"+query, "")); len(rows) != 0 || h.counts.rows != before {
			t.Errorf("%s: %v, %d queries", query, rows, h.counts.rows-before)
		}
	}

	// The authorizer's filter narrows the query; one outside it is empty
	// and never a 403.
	h.stub.Allow(stub.Rule{Action: "usage.read", Filter: &authz.Filter{Owners: []string{h.subject()}}})
	before := h.counts.rows
	rec := h.request(http.MethodGet, "/v1/usage?owner="+url.QueryEscape(h.bob), "")
	if rows := items(t, rec); len(rows) != 0 || h.counts.rows != before {
		t.Errorf("an owner outside the filter: %d %v, %d queries", rec.Code, rows, h.counts.rows-before)
	}
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?by=owner", "")); len(rows) != 1 || rows[0]["dimensions"].(map[string]any)["owner"] != h.subject() {
		t.Errorf("the filter's owner: %v", rows)
	}
	h.stub.Allow(stub.Rule{Action: "usage.read"})

	// The record route resolves the same names.
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?key=run-42", "")); !reflect.DeepEqual(got, []string{"req_C", "req_B", "req_A"}) {
		t.Errorf("by name: %v", got)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?key="+h.bobKeyID, "")); !reflect.DeepEqual(got, []string{"req_D"}) {
		t.Errorf("by id: %v", got)
	}
	before = h.counts.records
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?key=no-such-key", "")); len(got) != 0 || h.counts.records != before {
		t.Errorf("an unknown name: %v, %d queries", got, h.counts.records-before)
	}
}

// TestUsageFilterNarrows: the authorizer's filter of one owner leaves a
// query naming another owner's Key an empty result, its labels narrow
// the rows, and an intersection that selects nothing writes an empty
// items without asking the store. Spec 009's row, the route's half.
func TestUsageFilterNarrows(t *testing.T) {
	h := newUsageHarness(t)
	h.stub.Allow(stub.Rule{Action: "usage.read", Filter: &authz.Filter{Owners: []string{h.subject()}}})
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?key="+h.bobKeyID, "")); len(rows) != 0 {
		t.Errorf("another owner's Key: %v", rows)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?key="+h.bobKeyID, "")); len(got) != 0 {
		t.Errorf("another owner's records: %v", got)
	}
	if rows := items(t, h.request(http.MethodGet, "/v1/usage", "")); len(rows) != 1 || rows[0]["cost"] != float64(1250000) {
		t.Errorf("alice's own: %v", rows)
	}

	// Owners with nothing in common: an empty items and no query.
	before, beforeRecords := h.counts.rows, h.counts.records
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?owner="+url.QueryEscape(h.bob), "")); len(rows) != 0 {
		t.Errorf("owners that do not meet: %v", rows)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?owner="+url.QueryEscape(h.bob), "")); len(got) != 0 {
		t.Errorf("owners that do not meet, records: %v", got)
	}
	if h.counts.rows != before || h.counts.records != beforeRecords {
		t.Errorf("the store was asked %d rows and %d records", h.counts.rows-before, h.counts.records-beforeRecords)
	}

	// A label the filter names with another value is empty too; a label
	// the query and the filter agree on narrows.
	h.stub.Allow(stub.Rule{Action: "usage.read", Filter: &authz.Filter{Labels: map[string]string{"team": "research"}}})
	before = h.counts.rows
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?label=team%3Dops", "")); len(rows) != 0 || h.counts.rows != before {
		t.Errorf("a label the filter contradicts: %v, %d queries", rows, h.counts.rows-before)
	}
	if rows := items(t, h.request(http.MethodGet, "/v1/usage", "")); len(rows) != 1 || rows[0]["requests"] != float64(3) {
		t.Errorf("the filter's label: %v", rows)
	}
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?label=team%3Dresearch&label=team%3Dops", "")); len(rows) != 0 {
		t.Errorf("two values for one label: %v", rows)
	}
}

// TestUsageQueryValidation: GET /v1/usage rejects a range past 90 days,
// more than three by dimensions, an unknown dimension, an interval and
// an instant it cannot read, and a parameter the route does not define,
// each invalid_field at the parameter's name; a cost in a row is an
// integer. Spec 009's row, the route's half.
func TestUsageQueryValidation(t *testing.T) {
	h := newUsageHarness(t)
	for _, tc := range []struct{ query, path string }{
		{"?from=2026-01-01T00:00:00Z&to=2026-09-14T00:00:00Z", "to"},
		{"?from=2026-09-14T12:00:00Z&to=2026-09-14T11:00:00Z", "to"},
		{"?from=yesterday", "from"},
		{"?to=soon", "to"},
		{"?by=key,model,provider,owner", "by"},
		{"?by=colour", "by"},
		{"?by=key&by=key", "by"},
		{"?by=label:", "by"},
		{"?interval=week", "interval"},
		{"?label=nokey", "label"},
		{"?label=%3Dv", "label"},
		{"?colour=red", "colour"},
		{"?limit=10", "limit"},
		{"?cursor=x", "cursor"},
		{"?status=ok", "status"},
		{"?source=archive", "source"},
	} {
		rec := h.request(http.MethodGet, "/v1/usage"+tc.query, "")
		if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{tc.path}) {
			t.Errorf("%s: paths %v, detail %v", tc.query, paths(d), d["detail"])
		}
	}
	// The range the query leaves open is the last day, and a cost is an
	// integer count of micro-units in the JSON, never a float.
	rec := h.request(http.MethodGet, "/v1/usage", "")
	if rows := items(t, rec); len(rows) != 1 || rows[0]["cost"] != float64(1750000) {
		t.Fatalf("the default range: %v", rows)
	}
	if !strings.Contains(rec.Body.String(), `"cost":1750000,`) {
		t.Errorf("cost is not an integer: %s", rec.Body.String())
	}
	// A range the query names is honoured on both sides.
	if rows := items(t, h.request(http.MethodGet, "/v1/usage?from=2026-09-14T00:00:00Z&to=2026-09-14T01:00:00Z", "")); len(rows) != 0 {
		t.Errorf("a range before the rows: %v", rows)
	}
	// An interval buckets the rows.
	rows := items(t, h.request(http.MethodGet, "/v1/usage?interval=hour", ""))
	if len(rows) != 1 || rows[0]["bucket"] != h.at.Truncate(time.Hour).Format(time.RFC3339) {
		t.Errorf("hourly: %v", rows)
	}
}

// TestRequestsSource: GET /v1/requests answers the replica's own ring
// with source memory, newest first, paged by limit and cursor, and
// refuses a limit above 1000, a cursor from another query, and the
// source parameter, which is a member of the response. The archive half
// of the row, source archive with an exporter configured, waits for
// spec 012's request log. Spec 009's row.
func TestRequestsSource(t *testing.T) {
	h := newUsageHarness(t)
	rec := h.request(http.MethodGet, "/v1/requests", "")
	doc := body(t, rec)
	if doc["source"] != sourceMemory || doc["next_cursor"] != nil {
		t.Errorf("source %v, next_cursor %v", doc["source"], doc["next_cursor"])
	}
	if got := ids(t, rec); !reflect.DeepEqual(got, []string{"req_D", "req_C", "req_B", "req_A"}) {
		t.Errorf("newest first: %v", got)
	}

	// One page at a time: next_cursor resumes, and is absent on the last
	// page.
	const window = "from=2026-09-14T10:00:00Z&to=2026-09-14T12:00:00Z"
	var seen []string
	cursor := ""
	for range 5 {
		q := "/v1/requests?" + window + "&limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := h.request(http.MethodGet, q, "")
		seen = append(seen, ids(t, rec)...)
		next, _ := body(t, rec)["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if !reflect.DeepEqual(seen, []string{"req_D", "req_C", "req_B", "req_A"}) {
		t.Errorf("paged: %v", seen)
	}

	// The filters of the record table.
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?status=refused", "")); !reflect.DeepEqual(got, []string{"req_B"}) {
		t.Errorf("status: %v", got)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?error=model_not_allowed", "")); !reflect.DeepEqual(got, []string{"req_B"}) {
		t.Errorf("error: %v", got)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?stream=true", "")); !reflect.DeepEqual(got, []string{"req_C"}) {
		t.Errorf("stream: %v", got)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?stream=false", "")); !reflect.DeepEqual(got, []string{"req_D", "req_B", "req_A"}) {
		t.Errorf("not streamed: %v", got)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?label=team%3Dops", "")); !reflect.DeepEqual(got, []string{"req_D"}) {
		t.Errorf("label: %v", got)
	}
	if got := ids(t, h.request(http.MethodGet, "/v1/requests?limit=1000", "")); len(got) != 4 {
		t.Errorf("the cap: %v", got)
	}

	for _, tc := range []struct{ query, path string }{
		{"?limit=1001", "limit"},
		{"?limit=0", "limit"},
		{"?limit=many", "limit"},
		{"?status=elsewhere", "status"},
		{"?stream=perhaps", "stream"},
		{"?cursor=not-a-cursor", "cursor"},
		{"?" + window + "&cursor=" + url.QueryEscape(cursorOfAnotherQuery(t, h)), "cursor"},
		{"?source=archive", "source"},
		{"?source=memory", "source"},
		{"?by=key", "by"},
		{"?interval=hour", "interval"},
		{"?colour=red", "colour"},
	} {
		rec := h.request(http.MethodGet, "/v1/requests"+tc.query, "")
		if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{tc.path}) {
			t.Errorf("%s: paths %v, detail %v", tc.query, paths(d), d["detail"])
		}
	}
	if d := wantCode(t, h.request(http.MethodGet, "/v1/requests?source=archive", ""), CodeInvalidField); !strings.Contains(d["detail"].(string), sourceMemory) {
		t.Errorf("the source detail %v", d["detail"])
	}
}

// cursorOfAnotherQuery is a cursor minted over a query that selects one
// Key, so it is invalid on a query that selects every Key.
func cursorOfAnotherQuery(t *testing.T, h *usageHarness) string {
	t.Helper()
	rec := h.request(http.MethodGet, "/v1/requests?key=run-42&limit=1", "")
	next, _ := body(t, rec)["next_cursor"].(string)
	if next == "" {
		t.Fatal("no cursor to carry over")
	}
	return next
}

// TestUsageEnvelopes: GET /v1/usage answers items alone with the row
// shape of spec 009, GET /v1/requests answers items, next_cursor, and
// source, an empty answer carries an empty list and never null, and no
// record or row carries a Key's value or a Provider's credential.
func TestUsageEnvelopes(t *testing.T) {
	h := newUsageHarness(t)
	rec := h.request(http.MethodGet, "/v1/usage?by=key,model&interval=day", "")
	doc := body(t, rec)
	if _, paged := doc["next_cursor"]; paged || doc["source"] != nil || len(doc) != 1 {
		t.Errorf("the usage envelope carries %v", doc)
	}
	row := items(t, rec)[0]
	for _, member := range []string{"bucket", "dimensions", "requests", "ok", "refused", "failed", "inputTokens", "outputTokens", "cachedInputTokens", "cacheWriteTokens", "cost", "currency", "unpricedRequests"} {
		if _, ok := row[member]; !ok {
			t.Errorf("the row has no %s: %v", member, row)
		}
	}
	if d := row["dimensions"].(map[string]any); d["key"] != h.keyID || d["model"] != h.modelID {
		t.Errorf("the dimensions carry names, not ids: %v", d)
	}

	rec = h.request(http.MethodGet, "/v1/requests?limit=1", "")
	doc = body(t, rec)
	if doc["source"] != sourceMemory || doc["next_cursor"] == "" || len(doc) != 3 {
		t.Errorf("the requests envelope carries %v", doc)
	}
	record := items(t, rec)[0]
	if record["id"] != "req_D" || record["key"].(map[string]any)["prefix"] != "lux_AAAAAAAA" || record["cost"].(map[string]any)["amount"] != float64(1250000) {
		t.Errorf("the record renders %v", record)
	}
	if strings.Contains(rec.Body.String(), canary) {
		t.Error("a record carries the canary")
	}

	// An empty answer is an empty list on both routes.
	for _, path := range []string{"/v1/usage?key=nothing", "/v1/requests?key=nothing"} {
		rec := h.request(http.MethodGet, path, "")
		if items := items(t, rec); len(items) != 0 || !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("%s: %s", path, rec.Body.String())
		}
	}
	if doc := body(t, h.request(http.MethodGet, "/v1/requests?key=nothing", "")); doc["source"] != sourceMemory {
		t.Errorf("an empty page has no source: %v", doc)
	}
}

// TestUsageStoreErrors: a store that cannot answer is store_unavailable
// on both routes, the developer detail carrying the store's own line.
func TestUsageStoreErrors(t *testing.T) {
	h := newUsageHarness(t)
	h.counts.fail = errors.New("dial tcp 198.51.100.7:5432: connection refused")
	for _, path := range []string{"/v1/usage", "/v1/requests"} {
		d := wantCode(t, h.request(http.MethodGet, path, ""), CodeStoreUnavailable)
		if !strings.Contains(d["detail"].(string), "connection refused") {
			t.Errorf("%s: detail %v", path, d["detail"])
		}
	}
}
