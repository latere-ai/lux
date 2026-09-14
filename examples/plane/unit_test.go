// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The parts of the front a whole run does not reach: the query
// grammars, the refusal mapping, the preconditions, the windows, and
// the store's own rules.

// TestParseListReadsTheParameters: each parameter of a list route, and
// a refusal at the name of anything else.
func TestParseListReadsTheParameters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   string
		query  string
		refuse string // the path a refusal names, or ""
	}{
		{"the default page", v1.KindKey, "", ""},
		{"a label", v1.KindKey, "label=team%3Dresearch", ""},
		{"a label that is not k=v", v1.KindKey, "label=team", "label"},
		{"two values for one label", v1.KindKey, "label=a%3D1&label=a%3D2", ""},
		{"an owner", v1.KindKey, "owner=http%3A%2F%2Fi%7Cme", ""},
		{"a limit", v1.KindKey, "limit=7", ""},
		{"a limit of zero", v1.KindKey, "limit=0", "limit"},
		{"a limit above the cap", v1.KindKey, "limit=201", "limit"},
		{"a cursor", v1.KindKey, "cursor=" + cursorOf(v1.KindKey, "a"), ""},
		{"a source on Models", v1.KindModel, "source=declared", ""},
		{"a source elsewhere", v1.KindKey, "source=declared", "source"},
		{"an unknown source", v1.KindModel, "source=invented", "source"},
		{"a provider on Models", v1.KindModel, "provider=prv_1", ""},
		{"a provider elsewhere", v1.KindBudget, "provider=prv_1", "provider"},
		{"a parameter no route defines", v1.KindKey, "bogus=1", "bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			q, refusal := parseList(tc.kind, values)
			switch {
			case tc.refuse == "" && refusal != nil:
				t.Fatalf("refused with %v", refusal)
			case tc.refuse == "":
				return
			case refusal == nil:
				t.Fatalf("admitted %v", q)
			case len(refusal.paths) != 1 || refusal.paths[0] != tc.refuse:
				t.Errorf("refusal names %v, want %s", refusal.paths, tc.refuse)
			case refusal.code != codeInvalidField:
				t.Errorf("code %s", refusal.code)
			}
		})
	}
	// Two values for one label match nothing at all.
	values, err := url.ParseQuery("label=a%3D1&label=a%3D2")
	if err != nil {
		t.Fatal(err)
	}
	q, refusal := parseList(v1.KindKey, values)
	if refusal != nil || !q.nothing || q.admits(&v1.Key{}) {
		t.Errorf("two values for one label admit %v", q)
	}
}

// TestTheListFiltersSelect: the query's own filters and the
// authorizer's over one object.
func TestTheListFiltersSelect(t *testing.T) {
	key := &v1.Key{Metadata: v1.ObjectMeta{Name: "k", Labels: map[string]string{"team": "research"}}}
	key.Status.Owner = "https://login.example.com|alice"
	model := &v1.Model{Metadata: v1.ObjectMeta{Name: "m"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: "prv_1"}}}}
	model.Status.Source = v1.SourceDeclared
	for _, tc := range []struct {
		name  string
		q     listQuery
		obj   v1.Object
		admit bool
	}{
		{"the owner it names", listQuery{owner: "https://login.example.com|alice"}, key, true},
		{"another owner", listQuery{owner: "https://login.example.com|bob"}, key, false},
		{"a label it carries", listQuery{labels: map[string]string{"team": "research"}}, key, true},
		{"a label it lacks", listQuery{labels: map[string]string{"team": "ops"}}, key, false},
		{"the source", listQuery{source: "declared"}, model, true},
		{"another source", listQuery{source: "discovered"}, model, false},
		{"a source on another kind", listQuery{source: "declared"}, key, false},
		{"the provider it targets", listQuery{provider: "prv_1"}, model, true},
		{"a provider it does not", listQuery{provider: "prv_2"}, model, false},
		{"a provider on another kind", listQuery{provider: "prv_1"}, key, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.admits(tc.obj); got != tc.admit {
				t.Errorf("admits = %v", got)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		f     *authz.Filter
		admit bool
	}{
		{"no filter", nil, true},
		{"the owner", &authz.Filter{Owners: []string{"https://login.example.com|alice"}}, true},
		{"another owner", &authz.Filter{Owners: []string{"https://login.example.com|bob"}}, false},
		{"a label it carries", &authz.Filter{Labels: map[string]string{"team": "research"}}, true},
		{"a label it lacks", &authz.Filter{Labels: map[string]string{"team": "ops"}}, false},
	} {
		t.Run("the filter: "+tc.name, func(t *testing.T) {
			if got := allowedBy(tc.f, key); got != tc.admit {
				t.Errorf("allowedBy = %v", got)
			}
		})
	}
}

// TestPreconditionsAreOneFormEach: the two headers, their one accepted
// form each, and what they hold an object to.
func TestPreconditionsAreOneFormEach(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  http.Header
		refuse  string
		want    precondition
		exists  bool
		version int64
		then    string // the code check answers, or ""
	}{
		{name: "none", header: http.Header{}},
		{name: "If-Match at the version", header: http.Header{"If-Match": {`"2"`}}, want: precondition{version: 2}, exists: true, version: 2},
		{name: "If-Match at another version", header: http.Header{"If-Match": {`"1"`}}, want: precondition{version: 1}, exists: true, version: 2, then: codeConflict},
		{name: "If-Match on a free name", header: http.Header{"If-Match": {"*"}}, want: precondition{exists: true}, then: codeNotFound},
		{name: "If-None-Match on a taken name", header: http.Header{"If-None-Match": {"*"}}, want: precondition{free: true}, exists: true, version: 1, then: codeAlreadyExists},
		{name: "a weak validator", header: http.Header{"If-Match": {`W/"2"`}}, refuse: "If-Match"},
		{name: "an unquoted version", header: http.Header{"If-Match": {"2"}}, refuse: "If-Match"},
		{name: "a version of zero", header: http.Header{"If-Match": {`"0"`}}, refuse: "If-Match"},
		{name: "If-None-Match with a validator", header: http.Header{"If-None-Match": {`"2"`}}, refuse: "If-None-Match"},
		{name: "both headers", header: http.Header{"If-Match": {"*"}, "If-None-Match": {"*"}}, refuse: "If-Match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, refusal := parsePrecondition(tc.header)
			if tc.refuse != "" {
				if refusal == nil || refusal.paths[0] != tc.refuse {
					t.Fatalf("refusal %v, want one at %s", refusal, tc.refuse)
				}
				return
			}
			if refusal != nil {
				t.Fatalf("refused with %v", refusal)
			}
			if p != tc.want {
				t.Errorf("precondition %+v, want %+v", p, tc.want)
			}
			err := p.check(tc.exists, tc.version)
			switch {
			case tc.then == "" && err != nil:
				t.Errorf("check refused with %v", err)
			case tc.then != "" && (err == nil || err.code != tc.then):
				t.Errorf("check = %v, want %s", err, tc.then)
			}
		})
	}
}

// TestRefusalsCarryTheContractsCodes: every error a handler meets maps
// to the code the contract names, and every code has a status and one
// sentence.
func TestRefusalsCarryTheContractsCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"a refusal of this front's", refuse(codeNotFound, "gone"), codeNotFound},
		{"a manifest refusal", &manifest.Error{Code: manifest.CodeInvalidField, Paths: []string{"spec.ttl"}}, codeInvalidField},
		{"an authorizer that did not decide", &authz.Unavailable{Err: errors.New("refused")}, codeAuthorizerUnavailable},
		{"an object that is gone", errNotFound, codeNotFound},
		{"a version that moved", errVersionConflict, codeConflict},
		{"a value another Key holds", errValueTaken, codeInvalidField},
		{"anything else", errors.New("the store is on fire"), codeStoreUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mapErr(tc.err)
			if got.code != tc.code {
				t.Errorf("code %s, want %s", got.code, tc.code)
			}
			if got.Error() == "" {
				t.Error("the refusal renders no developer line")
			}
		})
	}
	if statusOf("nonesuch") != http.StatusInternalServerError || messageOf("nonesuch") == "" {
		t.Error("a code outside the table is not answered as internal")
	}
	for code, status := range codeStatus {
		message, ok := codeMessage[code]
		if !ok || !strings.HasSuffix(message, ".") || strings.Contains(message, ". ") {
			t.Errorf("%s: the sentence %q is not one sentence", code, message)
		}
		if status < 400 || status > 599 {
			t.Errorf("%s: status %d", code, status)
		}
	}
	if refuse(codeNotFound, "").Error() != codeNotFound {
		t.Error("a refusal with no detail renders more than its code")
	}
}

// TestTheCursorNamesItsKind: a page's cursor resumes its own kind's
// list and no other's.
func TestTheCursorNamesItsKind(t *testing.T) {
	c := cursorOf(v1.KindBudget, "b-2")
	if name, ok := afterCursor(v1.KindBudget, c); !ok || name != "b-2" {
		t.Errorf("the cursor resumes after %q, %v", name, ok)
	}
	if _, ok := afterCursor(v1.KindKey, c); ok {
		t.Error("a cursor of another kind was accepted")
	}
	if _, ok := afterCursor(v1.KindBudget, "not base64 at all!"); ok {
		t.Error("a cursor that is not this front's was accepted")
	}
	if _, ok := afterCursor(v1.KindBudget, "bm90IGEgY3Vyc29y"); ok {
		t.Error("a cursor with no kind was accepted")
	}
}

// TestTheStoreHoldsItsRules: the version, the name, the hash index, and
// what a delete takes with it.
func TestTheStoreHoldsItsRules(t *testing.T) {
	s := newStore()
	first := &v1.Budget{Metadata: v1.ObjectMeta{Name: "team"}}
	first.Status.ID = "bud_1"
	version, err := s.put(first, 0)
	if err != nil || version != 1 {
		t.Fatalf("the create = %d %v", version, err)
	}
	if _, err := s.put(first, 0); !errors.Is(err, errVersionConflict) {
		t.Errorf("a write at a version that moved = %v", err)
	}
	second := &v1.Budget{Metadata: v1.ObjectMeta{Name: "team"}}
	second.Status.ID = "bud_2"
	if _, err := s.put(second, 0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("a second object of one name = %v", err)
	}
	read, version, err := s.get(v1.KindBudget, "bud_1")
	if err != nil || version != 1 || read.Name() != "team" {
		t.Fatalf("the read = %v %d %v", read, version, err)
	}
	if _, _, err := s.get(v1.KindBudget, "bud_9"); !errors.Is(err, errNotFound) {
		t.Errorf("a read of nothing = %v", err)
	}
	if _, _, err := s.byName(v1.KindBudget, "nobody"); !errors.Is(err, errNotFound) {
		t.Errorf("a read by a name nobody has = %v", err)
	}
	// The hash index is unique, and a rotate replaces the Key's own row.
	if err := s.putHash("key_1", hashValue("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.putHash("key_2", hashValue("one")); !errors.Is(err, errValueTaken) {
		t.Errorf("a value another Key holds = %v", err)
	}
	if err := s.putHash("key_1", hashValue("two")); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.ByHash(t.Context(), hashValue("one")); k != nil {
		t.Error("the rotated value still names a Key")
	}
	// A Key row the store no longer holds is no Key.
	if k, err := s.ByHash(t.Context(), hashValue("two")); k != nil || err != nil {
		t.Errorf("a hash whose Key was never stored = %v %v", k, err)
	}
	s.putCredential("prv_1", []byte("sk-secret"))
	if value, _ := s.Credential(t.Context(), "prv_1"); string(value) != "sk-secret" {
		t.Errorf("the credential reads %q", value)
	}
	s.remove(v1.KindBudget, "bud_1")
	s.remove(v1.KindProvider, "prv_1")
	if _, _, err := s.get(v1.KindBudget, "bud_1"); !errors.Is(err, errNotFound) {
		t.Errorf("the deleted object = %v", err)
	}
	if value, _ := s.Credential(t.Context(), "prv_1"); value != nil {
		t.Error("the deleted Provider's credential is still held")
	}
	if p, err := s.Provider(t.Context(), "prv_1"); p != nil || err != nil {
		t.Errorf("a Provider that is gone = %v %v", p, err)
	}
	if b, err := s.Budget(t.Context(), ""); b != nil || err != nil {
		t.Errorf("an empty reference = %v %v", b, err)
	}
	if m, err := s.Model(t.Context(), "nobody"); m != nil || err != nil {
		t.Errorf("a Model that is gone = %v %v", m, err)
	}
}

// TestTheKeyValueIsShapedAsTheContractSays: the minted value, its
// handle, and the handle of a supplied one.
func TestTheKeyValueIsShapedAsTheContractSays(t *testing.T) {
	value, err := mintKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(value, "lux_") || len(value) != 44 {
		t.Errorf("the value %q is not lux_ and forty characters", value)
	}
	if keyPrefix(value, false) != value[:12] {
		t.Errorf("the handle %q is not the value's first twelve characters", keyPrefix(value, false))
	}
	supplied := keyPrefix("a token the platform issued", true)
	if !strings.HasPrefix(supplied, "sup_") || len(supplied) != 12 {
		t.Errorf("the supplied handle %q", supplied)
	}
	if strings.Contains(supplied, "a token") {
		t.Error("the handle carries the value")
	}
	second, err := mintKeyValue()
	if err != nil || second == value {
		t.Errorf("two mints yield %q twice", value)
	}
}

// TestTheWindowsRefuseInThePipelinesOrder drives the Limiter directly:
// the rate, the price that cannot be counted, the currency, the Key's
// spend window, and the Budget's, each with the contract's code.
func TestTheWindowsRefuseInThePipelinesOrder(t *testing.T) {
	s := newStore()
	l := newLimiter(s, manifest.Defaults{}, time.Second, time.Now)
	priced := &v1.Model{Metadata: v1.ObjectMeta{Name: "priced"}}
	usd := v1.Money(1_000_000)
	priced.Spec.Pricing = &v1.Pricing{Currency: "USD", Per: 1_000_000, Input: &usd, Output: &usd}
	unpriced := &v1.Model{Metadata: v1.ObjectMeta{Name: "unpriced"}}

	// A Key with no limit at all is admitted and counted.
	plain := newKey("key_plain")
	lease, err := l.Reserve(t.Context(), gateway.Reservation{Key: plain, Model: priced, InputTokens: 10, OutputTokens: 10})
	if err != nil {
		t.Fatalf("a Key with no limit was refused: %v", err)
	}
	lease.Settle(t.Context(), gateway.Tokens{Input: 10, Output: 10})
	if got := l.counters.Total(metering.TotalKey(metering.ScopeKeyRequests, "key_plain")); got != 1 {
		t.Errorf("the request count is %d", got)
	}
	if got := l.counters.Total(metering.TotalKey(metering.ScopeKeyTokens, "key_plain")); got != 20 {
		t.Errorf("the token count is %d", got)
	}

	// One request a minute admits one and refuses the next.
	rate := 1
	limited := newKey("key_rate")
	limited.Spec.Limits.RequestsPerMinute = &rate
	if _, err := l.Reserve(t.Context(), gateway.Reservation{Key: limited, Model: priced}); err != nil {
		t.Fatalf("the first request: %v", err)
	}
	assertRefusal(t, l, gateway.Reservation{Key: limited, Model: priced}, gateway.CodeRateLimited)

	// A token rate a reservation cannot fit refuses and gives the
	// request bucket back.
	tokens := 10
	tokenLimited := newKey("key_tokens")
	tokenLimited.Spec.Limits.TokensPerMinute = &tokens
	if _, err := l.Reserve(t.Context(), gateway.Reservation{Key: tokenLimited, Model: priced, InputTokens: 5, OutputTokens: 5}); err != nil {
		t.Fatalf("the first token reservation: %v", err)
	}
	assertRefusal(t, l, gateway.Reservation{Key: tokenLimited, Model: priced, InputTokens: 5, OutputTokens: 5}, gateway.CodeRateLimited)

	// A Model with no price under a spend limit is refused, and
	// allowUnpriced lifts it.
	spender := newKey("key_spend")
	amount := v1.Money(2_000)
	spender.Spec.Limits.Spend = &v1.Spend{Amount: &amount, Currency: "USD", Window: v1.Window("1h")}
	assertRefusal(t, l, gateway.Reservation{Key: spender, Model: unpriced}, gateway.CodeModelUnpriced)
	assertRefusal(t, l, gateway.Reservation{Key: spender, Opaque: true}, gateway.CodeModelUnpriced)
	lenient := newKey("key_lenient")
	lenient.Spec.Limits.Spend = &v1.Spend{Amount: &amount, Currency: "USD", Window: v1.Window("1h")}
	lenient.Spec.AllowUnpriced = true
	if _, err := l.Reserve(t.Context(), gateway.Reservation{Key: lenient, Model: unpriced}); err != nil {
		t.Errorf("allowUnpriced did not lift the refusal: %v", err)
	}

	// A price in another currency is refused before it is counted.
	euro := newKey("key_euro")
	euro.Spec.Limits.Spend = &v1.Spend{Amount: &amount, Currency: "EUR", Window: v1.Window("1h")}
	assertRefusal(t, l, gateway.Reservation{Key: euro, Model: priced, InputTokens: 1}, gateway.CodeCurrencyMismatch)

	// The spend window: one request at a thousand micro-units each way
	// leaves nothing for the next.
	if _, err := l.Reserve(t.Context(), gateway.Reservation{Key: spender, Model: priced, InputTokens: 1000, OutputTokens: 1000}); err != nil {
		t.Fatalf("the first priced request: %v", err)
	}
	assertRefusal(t, l, gateway.Reservation{Key: spender, Model: priced, InputTokens: 1000, OutputTokens: 1000}, gateway.CodeSpendExceeded)

	// A hard Budget refuses the same way, and a soft one never does.
	hard := &v1.Budget{Metadata: v1.ObjectMeta{Name: "hard"}, Spec: v1.BudgetSpec{Amount: &amount, Currency: "USD", Window: v1.Window("1h")}}
	hard.Status.ID = "bud_hard"
	if _, err := s.put(hard, 0); err != nil {
		t.Fatal(err)
	}
	drawer := newKey("key_drawer")
	drawer.Status.Budget = &v1.BudgetRef{Name: "hard", ID: "bud_hard"}
	if _, err := l.Reserve(t.Context(), gateway.Reservation{Key: drawer, Model: priced, InputTokens: 1000, OutputTokens: 1000}); err != nil {
		t.Fatalf("the first draw: %v", err)
	}
	assertRefusal(t, l, gateway.Reservation{Key: drawer, Model: priced, InputTokens: 1000, OutputTokens: 1000}, gateway.CodeBudgetExhausted)
	assertRefusal(t, l, gateway.Reservation{Key: drawer, Model: unpriced}, gateway.CodeModelUnpriced)
	if _, err := l.Reserve(t.Context(), gateway.Reservation{Key: nil}); err == nil {
		t.Error("a reservation without a Key was admitted")
	}
}

// newKey is one Key of an id, with a created time the windows are
// measured from.
func newKey(id string) *v1.Key {
	k := &v1.Key{Metadata: v1.ObjectMeta{Name: id}}
	k.Status.ID, k.Status.CreatedAt = id, time.Now().Add(-time.Minute)
	return k
}

// assertRefusal holds one reservation to the code it is refused with.
func assertRefusal(t *testing.T, l *limiter, r gateway.Reservation, code gateway.Code) {
	t.Helper()
	_, err := l.Reserve(t.Context(), r)
	var refusal *gateway.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the reservation was not refused: %v", err)
	}
	if refusal.Code != code {
		t.Errorf("code %s, want %s", refusal.Code, code)
	}
	if code == gateway.CodeRateLimited || code == gateway.CodeSpendExceeded || code == gateway.CodeBudgetExhausted {
		if refusal.RetryAfter <= 0 {
			t.Errorf("%s carries no Retry-After", code)
		}
	}
}

// TestTheUsageRoutesReadTheirParameters drives the two read routes over
// HTTP: the filters, the groupings, and a refusal at the name of
// anything outside the table.
func TestTheUsageRoutesReadTheirParameters(t *testing.T) {
	h := start(t, "", "")
	token := h.issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux"}})
	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"a range", "/v1/usage?from=2026-01-01T00%3A00%3A00Z&to=2026-01-02T00%3A00%3A00Z", http.StatusOK},
		{"a from that is no time", "/v1/usage?from=yesterday", http.StatusBadRequest},
		{"a to that is no time", "/v1/usage?to=tomorrow", http.StatusBadRequest},
		{"a range of more than ninety days", "/v1/usage?from=2020-01-01T00%3A00%3A00Z&to=2026-01-01T00%3A00%3A00Z", http.StatusBadRequest},
		{"four dimensions", "/v1/usage?by=key,model,provider,owner", http.StatusBadRequest},
		{"a dimension that is none", "/v1/usage?by=phase", http.StatusBadRequest},
		{"an interval that is none", "/v1/usage?interval=week", http.StatusBadRequest},
		{"an hour interval", "/v1/usage?interval=hour&by=key", http.StatusOK},
		{"the filters", "/v1/usage?key=key_1&model=mdl_1&provider=prv_1&owner=someone&label=team%3Dops", http.StatusOK},
		{"a label that is not k=v", "/v1/usage?label=team", http.StatusBadRequest},
		{"a parameter no route defines", "/v1/usage?bogus=1", http.StatusBadRequest},
		{"the records", "/v1/requests?limit=10&status=ok&error=none&from=2026-01-01T00%3A00%3A00Z", http.StatusOK},
		{"the records by filter", "/v1/requests?key=key_1&model=mdl_1&provider=prv_1&owner=someone", http.StatusOK},
		{"a records limit above the cap", "/v1/requests?limit=1001", http.StatusBadRequest},
		{"a records parameter no route defines", "/v1/requests?source=memory", http.StatusBadRequest},
		{"a records from that is no time", "/v1/requests?from=yesterday", http.StatusBadRequest},
		{"a records to that is no time", "/v1/requests?to=tomorrow", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(t, http.MethodGet, tc.path, token, "")
			if resp.status != tc.status {
				t.Errorf("GET %s = %d, want %d: %s", tc.path, resp.status, tc.status, resp.body)
			}
			if tc.status == http.StatusBadRequest && !strings.Contains(resp.body, `"invalid_field"`) {
				t.Errorf("GET %s did not name the parameter: %s", tc.path, resp.body)
			}
		})
	}
}

// TestTheRecordsAreFoldedByTheQuery: one record kept by the front is
// read back by every filter that admits it and by none that does not.
func TestTheRecordsAreFoldedByTheQuery(t *testing.T) {
	h := start(t, "", "")
	now := time.Now()
	h.plane.store.append(metering.Record{
		ID: "req_1", At: now, EndedAt: now,
		Key: metering.KeyRef{ID: "key_1", Prefix: "lux_abcdefgh"}, Owner: "https://login.example.com|alice",
		Model: metering.Ref{ID: "mdl_1", Name: "gpt"}, Provider: metering.Ref{ID: "prv_1", Name: "openai"},
		Status: metering.StatusOK, Door: v1.DialectOpenAI, Labels: map[string]string{"team": "research"},
		Cost: metering.Charge{Amount: 1500, Currency: "USD", Priced: true},
	})
	token := h.issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux"}})
	for _, tc := range []struct {
		query string
		rows  int
	}{
		{"", 1},
		{"?key=key_1", 1},
		{"?key=key_2", 0},
		{"?model=mdl_1", 1},
		{"?model=mdl_2", 0},
		{"?provider=prv_1", 1},
		{"?provider=prv_2", 0},
		{"?owner=https%3A%2F%2Flogin.example.com%7Calice", 1},
		{"?owner=someone-else", 0},
		{"?label=team%3Dresearch", 1},
		{"?label=team%3Dops", 0},
	} {
		resp := h.do(t, http.MethodGet, "/v1/usage"+tc.query, token, "")
		if resp.status != http.StatusOK {
			t.Fatalf("GET /v1/usage%s = %d %s", tc.query, resp.status, resp.body)
		}
		var answer struct {
			Items []metering.Row `json:"items"`
		}
		if err := json.Unmarshal([]byte(resp.body), &answer); err != nil {
			t.Fatal(err)
		}
		if len(answer.Items) != tc.rows {
			t.Errorf("GET /v1/usage%s has %d rows, want %d: %s", tc.query, len(answer.Items), tc.rows, resp.body)
		}
	}
	// A Key's id keeps working after the Key is gone, which is what
	// makes a run's ledger line outlive its Key.
	resp := h.do(t, http.MethodGet, "/v1/requests?key=key_1", token, "")
	if !strings.Contains(resp.body, `"req_1"`) || !strings.Contains(resp.body, `"source":"memory"`) {
		t.Errorf("GET /v1/requests: %s", resp.body)
	}
}

// TestTheCeilingsAreDecodedFromTheDecision: the authorizer's limits
// object in the contract's wire names, and a figure that does not read
// as no ceiling at all.
func TestTheCeilingsAreDecodedFromTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  manifest.Limits
		wrong bool
	}{
		{"none", "", manifest.Limits{}, false},
		{"the four", `{"max_key_requests_per_minute":10,"max_key_tokens_per_minute":20,"max_key_spend":"5","max_key_ttl":"720h"}`,
			manifest.Limits{MaxRequestsPerMinute: 10, MaxTokensPerMinute: 20, MaxSpend: v1.Money(5_000_000), MaxTTL: 720 * time.Hour}, false},
		{"a spend that is no money", `{"max_key_spend":"a lot"}`, manifest.Limits{}, true},
		{"a ttl that is no duration", `{"max_key_ttl":"a while"}`, manifest.Limits{}, true},
		{"limits that are not an object", `"none"`, manifest.Limits{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := authz.Decision{Allow: true}
			if tc.raw != "" {
				d.Limits = json.RawMessage(tc.raw)
			}
			got, err := decodeLimits(d)
			if tc.wrong {
				if err == nil {
					t.Fatalf("the ceilings read as %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("the ceilings did not read: %v", err)
			}
			if got != tc.want {
				t.Errorf("the ceilings are %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTheOpenAPIDocumentDescribesWhatIsServed: the document parses, and
// it names every route of this front and the five schemas.
func TestTheOpenAPIDocumentDescribesWhatIsServed(t *testing.T) {
	var doc struct {
		OpenAPI    string                    `json:"openapi"`
		Paths      map[string]map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIDocument("http://localhost:8080"), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi %q", doc.OpenAPI)
	}
	for _, path := range []string{
		"/v1/providers", "/v1/providers/{name}", "/v1/models", "/v1/models/{name}", "/v1/keys",
		"/v1/keys/{name}", "/v1/keys/{name}/rotate", "/v1/budgets", "/v1/budgets/{name}",
		"/v1/usage", "/v1/requests", "/v1/self", "/v1/openapi.json", "/.well-known/lux",
	} {
		if doc.Paths[path] == nil {
			t.Errorf("the document names no %s", path)
		}
	}
	for _, schema := range []string{"Provider", "Model", "Key", "Budget", "Error"} {
		if doc.Components.Schemas[schema] == nil {
			t.Errorf("the document has no %s schema", schema)
		}
	}
	for path, item := range doc.Paths {
		for method, op := range item {
			responses, _ := op.(map[string]any)["responses"].(map[string]any)
			if responses["default"] == nil {
				t.Errorf("%s %s describes no refusal", strings.ToUpper(method), path)
			}
		}
	}
}

// TestTheFrontRefusesWhatItCannotBuild: New says what it needs.
func TestTheFrontRefusesWhatItCannotBuild(t *testing.T) {
	base, err := url.Parse("http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"no public URL", Options{IssuerURL: "http://i", AuthorizerURL: "http://a"}},
		{"no issuer", Options{PublicURL: base, AuthorizerURL: "http://a"}},
		{"no authorizer", Options{PublicURL: base, IssuerURL: "http://i"}},
		{"an issuer that does not answer", Options{PublicURL: base, IssuerURL: "http://127.0.0.1:1", AuthorizerURL: "http://a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(t.Context(), tc.o); err == nil {
				t.Error("the front was built")
			}
		})
	}
}

// TestPrintableASCIIIsTheEchoRule: what a caller's own request id may
// carry to be echoed.
func TestPrintableASCIIIsTheEchoRule(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", false},
		{"conf-1", true},
		{strings.Repeat("x", 128), true},
		{strings.Repeat("x", 129), false},
		{"a\nb", false},
		{"é", false},
	} {
		if got := printableASCII(tc.in); got != tc.want {
			t.Errorf("printableASCII(%q) = %v", tc.in, got)
		}
	}
	first, second := newName(), newName()
	if first == second || !strings.HasPrefix(first, "key-") {
		t.Errorf("two generated names are %q and %q", first, second)
	}
	if last(nil) != "" {
		t.Error("the last of no values is not empty")
	}
}

// TestTheLookupAnswersAsTheCallerSees: a reference the authorizer
// refuses is not found, and one that cannot be decided is the
// authorizer's own error.
func TestTheLookupAnswersAsTheCallerSees(t *testing.T) {
	h := start(t, "", "")
	c := &call{p: h.plane, id: "req_1", r: &http.Request{Header: http.Header{}, URL: &url.URL{}}}
	c.caller = caller{subject: "https://login.example.com|alice"}
	l := c.lookup()
	if _, err := l.Provider(t.Context(), "nobody"); !errors.Is(err, manifest.ErrNotFound) {
		t.Errorf("a Provider that does not exist = %v", err)
	}
	if _, err := l.Budget(t.Context(), "nobody"); !errors.Is(err, manifest.ErrNotFound) {
		t.Errorf("a Budget that does not exist = %v", err)
	}
	models, err := l.Models(t.Context(), "*")
	if err != nil || len(models) != 0 {
		t.Errorf("a selector matching nothing = %v %v", models, err)
	}
	// With the endpoint refusing every decision, a reference is the
	// refusal and never an allow.
	h.authz.Fail(http.StatusServiceUnavailable)
	if _, err := l.Models(context.WithoutCancel(t.Context()), "*"); err == nil {
		t.Error("a selector was resolved while the authorizer was down")
	}
	h.authz.Fail(0)
}

// TestTheFrontRefusesWhatItCannotServe covers the corners a whole run
// does not walk: a forbidden decision, a body larger than the bound,
// an object of another kind's id, a delete of what is drawn from, and
// the seams the gateway calls with nothing to report.
func TestTheFrontRefusesWhatItCannotServe(t *testing.T) {
	h := start(t, "", "")
	token := h.issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux"}})

	// A body above the bound is refused before it is read.
	huge := `{"spec":{"amount":"1","currency":"USD","window":"month"}` + strings.Repeat(" ", int(maxManifestBytes)+1) + `}`
	if resp := h.do(t, http.MethodPut, "/v1/budgets/huge", token, huge); resp.status != http.StatusRequestEntityTooLarge {
		t.Errorf("a body above the bound = %d %s", resp.status, resp.body)
	}
	// An id of another kind names nothing, and an apply by id is a
	// refusal at the name.
	if resp := h.do(t, http.MethodGet, "/v1/keys/bud_01", token, ""); resp.status != http.StatusNotFound {
		t.Errorf("an id of another kind = %d %s", resp.status, resp.body)
	}
	if resp := h.do(t, http.MethodPut, "/v1/keys/key_01", token, `{"spec":{"models":["*"]}}`); resp.status != http.StatusBadRequest {
		t.Errorf("an apply by id = %d %s", resp.status, resp.body)
	}
	// A path the table does not carry, and a method it does not list.
	if resp := h.do(t, http.MethodGet, "/v1/nonesuch", token, ""); resp.status != http.StatusNotFound {
		t.Errorf("a path outside the table = %d", resp.status)
	}
	if resp := h.do(t, http.MethodPatch, "/v1/keys/dev", token, `{}`); resp.status != http.StatusNotFound {
		t.Errorf("a method the table does not list = %d", resp.status)
	}

	// A Budget a Key draws from, and the Provider a Model targets, are
	// refused until what names them is gone.
	h.mustApply(t, token, "/v1/budgets/team", `{"spec":{"amount":"10","currency":"USD","window":"month"}}`)
	h.mustApply(t, token, "/v1/keys/drawer", `{"spec":{"models":["*"],"budget":"team"}}`)
	if resp := h.do(t, http.MethodDelete, "/v1/budgets/team", token, ""); resp.status != http.StatusConflict {
		t.Errorf("a Budget a Key draws from = %d %s", resp.status, resp.body)
	}
	h.mustApply(t, token, "/v1/providers/openai", `{"spec":{"dialect":"openai","baseURL":"https://openai.provider.example.com/v1","credential":{"value":"sk-x"},"discovery":{"mode":"none"},"health":{"mode":"none"}}}`)
	h.mustApply(t, token, "/v1/models/gpt", `{"spec":{"targets":[{"provider":"openai"}]}}`)
	if resp := h.do(t, http.MethodDelete, "/v1/providers/openai", token, ""); resp.status != http.StatusConflict {
		t.Errorf("a Provider a Model targets = %d %s", resp.status, resp.body)
	}
	// A rotate keeps everything but the value, and refuses a
	// precondition that has no meaning on it.
	rotated := h.do(t, http.MethodPost, "/v1/keys/drawer/rotate", token, "")
	if rotated.status != http.StatusOK || !strings.Contains(rotated.body, `"value":"lux_`) {
		t.Errorf("the rotate = %d %s", rotated.status, rotated.body)
	}
	if resp := h.do(t, http.MethodPost, "/v1/keys/nobody/rotate", token, ""); resp.status != http.StatusNotFound {
		t.Errorf("a rotate of nothing = %d", resp.status)
	}

	// A decision the authorizer denies is forbidden, and one it cannot
	// make is the permission service being unavailable.
	h.authz.Deny(stub.Rule{Action: authorizer.ActionKeyRead}, "not yours")
	if resp := h.do(t, http.MethodGet, "/v1/keys/drawer", token, ""); resp.status != http.StatusForbidden {
		t.Errorf("a denied read = %d %s", resp.status, resp.body)
	}
	h.authz.SetRules()
	h.authz.Fail(http.StatusServiceUnavailable)
	if resp := h.do(t, http.MethodGet, "/v1/keys", token, ""); resp.status != http.StatusServiceUnavailable {
		t.Errorf("a decision nobody made = %d %s", resp.status, resp.body)
	}
	h.authz.Fail(0)

	// The seams the data plane calls: health is observed and kept
	// nowhere, and the counter table answers a read of keys it has
	// never seen.
	noHealth{}.Observe("prv_1", true)
	if healthy("prv_1") != v1.HealthHealthy {
		t.Error("the front's health view is not healthy")
	}
	totals, err := h.plane.store.Read(t.Context(), []string{"a", "b"})
	if err != nil || len(totals) != 2 || totals["a"] != 0 {
		t.Errorf("the counter table reads %v %v", totals, err)
	}
}

// mustApply PUTs one manifest and requires a create or an update.
func (h *harness) mustApply(t *testing.T, token, path, body string) {
	t.Helper()
	resp := h.do(t, http.MethodPut, path, token, body)
	if resp.status != http.StatusCreated && resp.status != http.StatusOK {
		t.Fatalf("PUT %s = %d %s", path, resp.status, resp.body)
	}
}

// TestTheLeaseGivesBackWhatARefusalTook: a reservation refused after
// the buckets were charged leaves them as it found them.
func TestTheLeaseGivesBackWhatARefusalTook(t *testing.T) {
	s := newStore()
	l := newLimiter(s, manifest.Defaults{}, time.Second, time.Now)
	rate, tokens := 2, 10
	k := newKey("key_refund")
	k.Spec.Limits.RequestsPerMinute = &rate
	k.Spec.Limits.TokensPerMinute = &tokens
	big := gateway.Reservation{Key: k, Model: &v1.Model{}, InputTokens: 100, OutputTokens: 100}
	if _, err := l.Reserve(t.Context(), big); err != nil {
		t.Fatalf("the first reservation: %v", err)
	}
	// The token bucket cannot cover the second, so it is refused and
	// the request bucket is given back what the refusal took: one
	// request of the two a minute is still there.
	assertRefusal(t, l, big, gateway.CodeRateLimited)
	if a := l.buckets.Allow("requests:key_refund"); !a.OK {
		t.Error("the request bucket was not given back what the refusal took")
	}
	if a := l.buckets.Allow("requests:key_refund"); a.OK {
		t.Error("the request bucket admits more than its rate")
	}

	// A settle that measured nothing refunds the estimate whole.
	priced := &v1.Model{Metadata: v1.ObjectMeta{Name: "priced"}}
	unit := v1.Money(1_000_000)
	priced.Spec.Pricing = &v1.Pricing{Currency: "USD", Per: 1_000_000, Input: &unit, Output: &unit}
	plain := newKey("key_settle")
	lease, err := l.Reserve(t.Context(), gateway.Reservation{Key: plain, Model: priced, InputTokens: 10, OutputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	lease.Settle(t.Context(), gateway.Tokens{})
	if got := l.counters.Total(metering.TotalKey(metering.ScopeKeySpend, "key_settle")); got != 0 {
		t.Errorf("a settle of nothing left %d micro-units", got)
	}
	// A settle twice is one settle: the lease is spent.
	lease.Settle(t.Context(), gateway.Tokens{Input: 10, Output: 10})
}
