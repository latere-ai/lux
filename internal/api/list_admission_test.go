// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/base64"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
	v1 "latere.ai/x/lux/manifest/v1"
)

func TestObjectListAdmissionPagesAndFilters(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.AuthorizeListItems = true })
	for i := range 9 {
		b := &v1.Budget{Metadata: v1.ObjectMeta{Name: "page-" + strconv.Itoa(i), Labels: map[string]string{"tenant": "a"}}, Spec: v1.BudgetSpec{Currency: "USD", Window: v1.WindowMonth}, Status: v1.BudgetStatus{ID: h.newID(v1.PrefixBudget), Owner: h.subject()}}
		if i == 7 {
			b.Metadata.Labels["tenant"] = "b"
		}
		if _, err := h.st.Objects().Put(t.Context(), b, 0); err != nil {
			t.Fatal(err)
		}
	}
	s := stub.New(t, stub.WithAction(authorizer.ActionBudgetRead, func(req authz.Request) any {
		return map[string]any{"allow": strings.Contains("page-1,page-3,page-4,page-7", req.Resource.String("name")), "reason": "private", "limits": map[string]any{"requests_per_minute": 1}}
	}))
	s.Allow(stub.Rule{Action: authorizer.ActionBudgetList, Filter: &authz.Filter{Labels: map[string]string{"tenant": "a"}}, Limits: map[string]any{"requests_per_minute": 600}})
	client, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(client)
	var all []string
	cursor := ""
	for range 4 {
		rec := h.request(http.MethodGet, "/v1/budgets?limit=2&cursor="+cursor, "")
		got := names(t, rec)
		all = append(all, got...)
		cursor, _ = body(t, rec)["next_cursor"].(string)
		if cursor == "" {
			break
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "page-2") || strings.Contains(string(raw), "page-5") {
			t.Fatal("cursor leaked denied name", string(raw))
		}
	}
	if !reflect.DeepEqual(all, []string{"page-1", "page-3", "page-4"}) {
		t.Fatal("visibility/pagination", all)
	}
	if cursor != "" {
		t.Fatal("pagination never finished")
	}
	if got := names(t, h.request(http.MethodGet, "/v1/budgets?label=tenant%3Db", "")); len(got) != 0 {
		t.Fatal("query widened decision filter", got)
	}
}

func TestObjectListAdmissionFailsWithoutPartialResults(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.AuthorizeListItems = true })
	h.seed()
	h.stub.Fail(http.StatusServiceUnavailable)
	wantCode(t, h.request(http.MethodGet, "/v1/models", ""), CodeAuthorizerUnavailable)
	s := stub.New(t, stub.WithAction(authorizer.ActionModelRead, func(authz.Request) any { return map[string]any{"unreadable": true} }))
	client, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(client)
	rec := h.request(http.MethodGet, "/v1/models", "")
	wantCode(t, rec, CodeAuthorizerUnavailable)
	if strings.Contains(rec.Body.String(), `"items"`) {
		t.Fatal("partial list leaked")
	}
}

func TestObjectListAdmissionIsOptIn(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	h.stub.Deny(stub.Rule{Action: authorizer.ActionModelRead}, "private")
	if got := names(t, h.request(http.MethodGet, "/v1/models", "")); !reflect.DeepEqual(got, []string{"gpt-5"}) {
		t.Fatal(got)
	}
	h.h.o.AuthorizeListItems = true
	if got := names(t, h.request(http.MethodGet, "/v1/models", "")); len(got) != 0 {
		t.Fatal(got)
	}
}
