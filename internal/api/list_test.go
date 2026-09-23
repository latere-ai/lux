// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// names lists the metadata.name of every item of a list response.
func names(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	items, _ := body(t, rec)["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any)["metadata"].(map[string]any)["name"].(string))
	}
	return out
}

// TestListSelectors: repeated ?label= is a conjunction, ?owner= outside
// the authorizer's filter is an empty list and never a 403, the
// filter's labels narrow the list, ?source= and ?provider= select
// Models by name or id, a Provider that does not exist is an empty list,
// an unknown parameter and a bad value are invalid_field at the
// parameter's name, and a Key's value or a Provider's credential appears
// in no list.
func TestListSelectors(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, k := range []struct{ name, labels string }{{"run-1", `{"team": "research", "tier": "gold"}`}, {"run-2", `{"team": "research", "tier": "silver"}`}, {"run-3", `{"team": "ops", "tier": "gold"}`}} {
		if rec := h.request(http.MethodPut, "/v1/keys/"+k.name, `{"metadata": {"labels": `+k.labels+`}, "spec": {"models": ["gpt-5"]}}`); rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", k.name, rec.Code, rec.Body.String())
		}
	}
	if rec := h.request(http.MethodPut, "/v1/keys/bob-1", `{"spec": {"models": ["gpt-5"]}}`, as(h.bob)...); rec.Code != http.StatusCreated {
		t.Fatalf("bob's Key: %d %s", rec.Code, rec.Body.String())
	}
	if got := names(t, h.request(http.MethodGet, "/v1/keys", "")); !reflect.DeepEqual(got, []string{"bob-1", "run-1", "run-2", "run-3"}) {
		t.Errorf("every Key: %v", got)
	}
	if got := names(t, h.request(http.MethodGet, "/v1/keys?label=team%3Dresearch&label=tier%3Dgold", "")); !reflect.DeepEqual(got, []string{"run-1"}) {
		t.Errorf("two labels: %v", got)
	}
	if got := names(t, h.request(http.MethodGet, "/v1/keys?label=tier%3Dgold&label=tier%3Dsilver", "")); len(got) != 0 {
		t.Errorf("two values for one label: %v", got)
	}
	if got := names(t, h.request(http.MethodGet, "/v1/keys?owner="+h.iss.URL()+"%7Cbob", "")); !reflect.DeepEqual(got, []string{"bob-1"}) {
		t.Errorf("owner: %v", got)
	}
	// The authorizer's filter: owners alone, the caller's ?owner= outside
	// it, labels, and several owners.
	h.stub.Allow(stub.Rule{Action: "key.list", Filter: &authz.Filter{Owners: []string{h.subject()}}})
	if got := names(t, h.request(http.MethodGet, "/v1/keys", "")); !reflect.DeepEqual(got, []string{"run-1", "run-2", "run-3"}) {
		t.Errorf("filtered to alice: %v", got)
	}
	rec := h.request(http.MethodGet, "/v1/keys?owner="+h.iss.URL()+"%7Cbob", "")
	if got := names(t, rec); len(got) != 0 {
		t.Errorf("an owner outside the filter: %v", got)
	}
	h.stub.Allow(stub.Rule{Action: "key.list", Filter: &authz.Filter{Owners: []string{h.subject()}, Labels: map[string]string{"team": "research"}}})
	if got := names(t, h.request(http.MethodGet, "/v1/keys", "")); !reflect.DeepEqual(got, []string{"run-1", "run-2"}) {
		t.Errorf("filter labels: %v", got)
	}
	if got := names(t, h.request(http.MethodGet, "/v1/keys?label=team%3Dops", "")); len(got) != 0 {
		t.Errorf("a label the filter contradicts: %v", got)
	}
	h.stub.Allow(stub.Rule{Action: "key.list", Filter: &authz.Filter{Owners: []string{h.iss.URL() + "|bob", h.iss.URL() + "|carol"}}})
	if got := names(t, h.request(http.MethodGet, "/v1/keys", "")); !reflect.DeepEqual(got, []string{"bob-1"}) {
		t.Errorf("several owners: %v", got)
	}
	h.stub.Allow(stub.Rule{Action: "key.list"})

	// Models by source and by Provider.
	pid := h.objectOf(v1.KindProvider, "openai").ID()
	discovered := &v1.Model{Metadata: v1.ObjectMeta{Name: "openai/o5"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: pid, Model: "o5"}}},
		Status: v1.ModelStatus{ID: h.newID(v1.PrefixModel), Owner: h.subject(), Source: v1.SourceDiscovered, Warnings: []string{}}}
	if _, err := h.st.Objects().Put(bg(), discovered, 0); err != nil {
		t.Fatal(err)
	}
	if got := names(t, h.request(http.MethodGet, "/v1/models?source=discovered", "")); !reflect.DeepEqual(got, []string{"openai/o5"}) {
		t.Errorf("discovered: %v", got)
	}
	if got := names(t, h.request(http.MethodGet, "/v1/models?source=declared", "")); !reflect.DeepEqual(got, []string{"gpt-5"}) {
		t.Errorf("declared: %v", got)
	}
	for _, ref := range []string{"openai", pid} {
		if got := names(t, h.request(http.MethodGet, "/v1/models?provider="+ref, "")); !reflect.DeepEqual(got, []string{"gpt-5", "openai/o5"}) {
			t.Errorf("provider %s: %v", ref, got)
		}
	}
	if got := names(t, h.request(http.MethodGet, "/v1/models?provider=nowhere", "")); len(got) != 0 {
		t.Errorf("a Provider that does not exist: %v", got)
	}
	for query, path := range map[string]string{
		"?color=red": "color", "?source=other": "source", "?label=nokey": "label", "?label=%3Dv": "label",
		"?limit=0": "limit", "?limit=201": "limit", "?limit=many": "limit", "?cursor=not-a-cursor": "cursor",
	} {
		rec := h.request(http.MethodGet, "/v1/models"+query, "")
		if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{path}) {
			t.Errorf("%s: paths %v", query, paths(d))
		}
	}
	for _, query := range []string{"?source=declared", "?provider=openai"} {
		rec := h.request(http.MethodGet, "/v1/keys"+query, "")
		wantCode(t, rec, CodeInvalidField)
	}
	if rec := h.request(http.MethodGet, "/v1/providers", ""); strings.Contains(rec.Body.String(), canary) {
		t.Error("a Provider list carries the credential")
	}
}

// TestPagination: a list pages through items and next_cursor at the
// default and the maximum, next_cursor is absent on the last page, the
// pages together are every object once in name order, and a cursor from
// another kind is invalid_field at cursor.
func TestPagination(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RequestsPerMinute = 0 })
	h.seed()
	const n = 230
	for i := range n {
		name := "b-" + strconv.Itoa(1000+i)
		b := &v1.Budget{Metadata: v1.ObjectMeta{Name: name}, Spec: v1.BudgetSpec{Currency: "USD", Window: v1.WindowMonth},
			Status: v1.BudgetStatus{ID: h.newID(v1.PrefixBudget), Owner: h.subject(), Warnings: []string{}}}
		amount := v1.Money(1_000_000)
		b.Spec.Amount = &amount
		if _, err := h.st.Objects().Put(bg(), b, 0); err != nil {
			t.Fatal(err)
		}
	}
	var all []string
	cursor := ""
	pages := 0
	for {
		path := "/v1/budgets"
		if cursor != "" {
			path += "?cursor=" + cursor
		}
		rec := h.request(http.MethodGet, path, "")
		got := names(t, rec)
		pages++
		all = append(all, got...)
		next, _ := body(t, rec)["next_cursor"].(string)
		if next == "" {
			if _, present := body(t, rec)["next_cursor"]; present {
				t.Error("next_cursor is present on the last page")
			}
			break
		}
		if len(got) != defaultLimit {
			t.Errorf("page %d has %d items, want %d", pages, len(got), defaultLimit)
		}
		cursor = next
	}
	if len(all) != n+1 || pages != 5 {
		t.Errorf("%d items over %d pages", len(all), pages)
	}
	if !sortedUnique(all) {
		t.Error("the pages are not every object once in name order")
	}
	rec := h.request(http.MethodGet, "/v1/budgets?limit=200", "")
	if got := names(t, rec); len(got) != maxLimit || body(t, rec)["next_cursor"] == nil {
		t.Errorf("the maximum: %d items, next_cursor %v", len(got), body(t, rec)["next_cursor"])
	}
	next := body(t, rec)["next_cursor"].(string)
	rec = h.request(http.MethodGet, "/v1/keys?cursor="+next, "")
	if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{"cursor"}) || !strings.Contains(d["detail"].(string), "cursor is over Budget") {
		t.Errorf("a cursor from another kind: %v", d)
	}
	rec = h.request(http.MethodGet, "/v1/budgets?cursor="+next+"&label=a%3Db", "")
	wantCode(t, rec, CodeInvalidField)
	foreign := store.EncodeCursor(v1.KindBudget, store.Filter{}, "zzz")
	if got := names(t, h.request(http.MethodGet, "/v1/budgets?cursor="+foreign, "")); len(got) != 0 {
		t.Errorf("a cursor past the end: %v", got)
	}
}

// TestUnknownQueryParameter: a query parameter no route defines is
// invalid_field at that parameter's name, on every list route.
func TestUnknownQueryParameter(t *testing.T) {
	h := newHarness(t, nil)
	for _, plural := range []string{"providers", "models", "keys", "budgets"} {
		rec := h.request(http.MethodGet, "/v1/"+plural+"?owners=me", "")
		if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{"owners"}) {
			t.Errorf("%s: paths %v", plural, paths(d))
		}
	}
}

func sortedUnique(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] <= s[i-1] {
			return false
		}
	}
	return true
}

func TestFilteredPaginationDoesNotDiscardVisibleOverflow(t *testing.T) {
	h := newHarness(t, nil)
	for i, owner := range []string{"denied", h.subject(), h.subject(), h.subject()} {
		b := &v1.Budget{Metadata: v1.ObjectMeta{Name: "page-" + strconv.Itoa(i)}, Spec: v1.BudgetSpec{Currency: "USD", Window: v1.WindowMonth}, Status: v1.BudgetStatus{ID: h.newID(v1.PrefixBudget), Owner: owner}}
		if _, err := h.st.Objects().Put(t.Context(), b, 0); err != nil {
			t.Fatal(err)
		}
	}
	h.stub.Allow(stub.Rule{Action: "budget.list", Filter: &authz.Filter{Owners: []string{h.subject(), "another"}}})
	first := h.request(http.MethodGet, "/v1/budgets?limit=2", "")
	if got := names(t, first); !reflect.DeepEqual(got, []string{"page-1", "page-2"}) {
		t.Fatal(got)
	}
	next, _ := body(t, first)["next_cursor"].(string)
	if next == "" {
		t.Fatal("permitted overflow was dropped: no next cursor")
	}
	second := h.request(http.MethodGet, "/v1/budgets?limit=2&cursor="+next, "")
	if got := names(t, second); !reflect.DeepEqual(got, []string{"page-3"}) {
		t.Fatal("permitted overflow was dropped", got)
	}
}
