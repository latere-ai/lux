// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The keys group is spec 007 at the doors: a rotate invalidates the old
// value, each Key state refuses with its code, a rate refusal carries
// Retry-After, a spend refusal and a hard Budget refusal carry theirs, an
// unpriced Model under a Budget is model_unpriced, a supplied value
// opens a door by its exact bytes, and a value supplied as its SHA-256
// opens a door by the string the hash is of.
var keysCases = []testCase{
	{group: "keys", name: "case007RotateInvalidatesTheOldValue", spec: 7, bearer: true, key: true, fn: case007RotateInvalidatesTheOldValue},
	{group: "keys", name: "case007KeyStates", spec: 7, bearer: true, key: true, fn: case007KeyStates},
	{group: "keys", name: "case007RateLimited", spec: 7, bearer: true, key: true, fn: case007RateLimited},
	{group: "keys", name: "case007UnpricedUnderABudget", spec: 7, bearer: true, key: true, fn: case007UnpricedUnderABudget},
	{group: "keys", name: "case007SuppliedValue", spec: 7, bearer: true, key: true, fn: case007SuppliedValue},
	{group: "keys", name: "case007HashSuppliedValue", spec: 7, bearer: true, key: true, fn: case007HashSuppliedValue},
	{group: "keys", name: "case007SpendWindow", spec: 7, bearer: true, key: true, stubs: true, fn: case007SpendWindow},
	{group: "keys", name: "case007BudgetExhausted", spec: 7, bearer: true, key: true, stubs: true, fn: case007BudgetExhausted},
	{group: "keys", name: "case037SeveralBudgets", spec: 37, bearer: true, key: true, stubs: true, fn: case037SeveralBudgets},
	{group: "keys", name: "case037AnchoredWindow", spec: 37, bearer: true, key: true, fn: case037AnchoredWindow},
	{group: "keys", name: "case037Restart", spec: 37, bearer: true, key: true, stubs: true, fn: case037Restart},
	{group: "keys", name: "case039DisabledModel", spec: 39, bearer: true, key: true, fn: case039DisabledModel},
}

// revocationTimeout bounds the wait for a rotated, disabled, or deleted
// Key to be refused, which is LUX_KEY_CACHE on the server, ten seconds
// by default, and the journal tail after it.
const revocationTimeout = 30 * time.Second

// suiteSelectors is the selector list every suite Key carries: every
// Model of this run.
func (c *client) suiteSelectors() []string { return []string{c.name("*")} }

// keySpec is a Key spec over the suite's Models with extra members.
func (c *client) keySpec(extra map[string]any) map[string]any {
	spec := map[string]any{"models": c.suiteSelectors()}
	maps.Copy(spec, extra)
	return spec
}

// doorStatus is the code a door answers a model list with for a
// credential, or "" for a 200.
func (c *client) doorStatus(t testing.TB, credential string) string {
	t.Helper()
	resp := c.door(t, c.well.Dialects[0], http.MethodGet, modelsPath(c.well.Dialects[0]), nil, credential)
	if resp.Status == http.StatusOK {
		return ""
	}
	return c.doorCode(t, c.well.Dialects[0], resp)
}

// case007RotateInvalidatesTheOldValue: after a rotate the old value is
// unauthenticated on a door within the cache window and the new value
// is served.
func case007RotateInvalidatesTheOldValue(t testing.TB, c *client) {
	k := c.object(t, v1.KindKey, "rotate", c.keySpec(nil))
	id, old := str(k, "status.id"), str(k, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	if code := c.doorStatus(t, old); code != "" {
		t.Fatalf("the fresh value is %s on a door", code)
	}
	resp := c.v1(t, http.MethodPost, "/keys/"+id+"/rotate", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("rotate: %d %s", resp.Status, excerpt(resp.Body))
	}
	fresh := str(resp.json(t), "status.value")
	c.eventually(t, revocationTimeout, "the old value is refused", func() (bool, string) {
		code := c.doorStatus(t, old)
		return code == "unauthenticated", "the old value answers " + strconv.Quote(code)
	})
	if code := c.doorStatus(t, fresh); code != "" {
		t.Errorf("the new value is %s on a door", code)
	}
}

// case007KeyStates: a Key disabled by an update is key_disabled within
// the cache window, and one whose expiresAt passes is key_expired at that
// instant with no write.
func case007KeyStates(t testing.TB, c *client) {
	d := c.object(t, v1.KindKey, "disabled", c.keySpec(nil))
	id, value := str(d, "status.id"), str(d, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	if code := c.doorStatus(t, value); code != "" {
		t.Fatalf("the fresh value is %s on a door", code)
	}
	spec := obj(d, "spec")
	spec["disabled"] = true
	if resp := c.apply(t, v1.KindKey, c.name("disabled"), map[string]any{"metadata": d["metadata"], "spec": spec}); resp.Status != http.StatusOK {
		t.Fatalf("disabling: %d %s", resp.Status, excerpt(resp.Body))
	}
	c.eventually(t, revocationTimeout, "the disabled Key is refused", func() (bool, string) {
		code := c.doorStatus(t, value)
		return code == "key_disabled", "the disabled Key answers " + strconv.Quote(code)
	})
	if read := c.read(t, v1.KindKey, id); str(read.json(t), "status.state") != string(v1.KeyDisabled) {
		t.Errorf("status.state %q after disabling", str(read.json(t), "status.state"))
	}

	expires := time.Now().Add(3 * time.Second).UTC().Truncate(time.Second)
	e := c.object(t, v1.KindKey, "expiring", c.keySpec(map[string]any{"expiresAt": expires.Format(time.RFC3339)}))
	defer c.mustDelete(t, v1.KindKey, str(e, "status.id"))
	if str(e, "status.expiresAt") == "" {
		t.Errorf("status.expiresAt is absent: %s", canonical(t, e["status"]))
	}
	c.eventually(t, 15*time.Second, "the expired Key is refused", func() (bool, string) {
		code := c.doorStatus(t, str(e, "status.value"))
		return code == "key_expired", "the Key answers " + strconv.Quote(code) + " at " + time.Now().UTC().Format(time.RFC3339)
	})
}

// case007RateLimited: a Key with requestsPerMinute 1 admits one request
// and refuses the next with rate_limited and a Retry-After of at least
// one second.
func case007RateLimited(t testing.TB, c *client) {
	k := c.object(t, v1.KindKey, "rate", c.keySpec(map[string]any{"limits": map[string]any{"requestsPerMinute": 1}}))
	defer c.mustDelete(t, v1.KindKey, str(k, "status.id"))
	value := str(k, "status.value")
	// The first request is admitted at the windows and, without stubs,
	// refused at the codec before any provider is dialed: a Messages
	// body whose messages member is no list.
	first := c.door(t, "anthropic", http.MethodPost, "/v1/messages", map[string]any{"model": c.name("gpt"), "messages": "nope"}, value)
	if first.Status == http.StatusTooManyRequests {
		t.Fatalf("the first request was rate limited: %s", excerpt(first.Body))
	}
	second := c.door(t, "anthropic", http.MethodPost, "/v1/messages", map[string]any{"model": c.name("gpt"), "messages": "nope"}, value)
	c.expectDoor(t, "anthropic", second, "rate_limited")
	retry, err := strconv.Atoi(second.Header.Get("Retry-After"))
	if err != nil || retry < 1 || retry > 60 {
		t.Errorf("Retry-After %q on a rate refusal", second.Header.Get("Retry-After"))
	}
}

// case007UnpricedUnderABudget: a Key under a hard Budget naming an
// unpriced Model is model_unpriced before any provider is reached, and
// allowUnpriced lifts the refusal.
func case007UnpricedUnderABudget(t testing.TB, c *client) {
	b := c.object(t, v1.KindBudget, "hard", budgetSpec("10"))
	defer c.mustDelete(t, v1.KindBudget, str(b, "status.id"))
	strict := c.object(t, v1.KindKey, "strict", c.keySpec(map[string]any{"budget": c.name("hard")}))
	defer c.mustDelete(t, v1.KindKey, str(strict, "status.id"))
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("gpt"), false), str(strict, "status.value")), "model_unpriced")
	lenient := c.object(t, v1.KindKey, "lenient", c.keySpec(map[string]any{"budget": c.name("hard"), "allowUnpriced": true}))
	defer c.mustDelete(t, v1.KindKey, str(lenient, "status.id"))
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("gpt"), false), str(lenient, "status.value"))
	if resp.Status != http.StatusOK && c.doorCode(t, "openai", resp) == "model_unpriced" {
		t.Error("allowUnpriced did not lift model_unpriced")
	}
}

// case007SuppliedValue: a Key created with spec.value opens a door by
// that exact string, its create answer carries no status.value, and its
// prefix is sup_ and eight hex characters.
func case007SuppliedValue(t testing.TB, c *client) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	value := "conf-supplied-" + hex.EncodeToString(raw)
	k := c.object(t, v1.KindKey, "supplied", c.keySpec(map[string]any{"value": value}))
	defer c.mustDelete(t, v1.KindKey, str(k, "status.id"))
	if str(k, "status.value") != "" || strings.Contains(canonical(t, k), value) {
		t.Errorf("the create answer carries the supplied value: %s", canonical(t, k))
	}
	prefix := str(k, "status.prefix")
	if len(prefix) != 12 || !strings.HasPrefix(prefix, "sup_") {
		t.Errorf("status.prefix %q", prefix)
	}
	if _, err := hex.DecodeString(prefix[4:]); err != nil {
		t.Errorf("status.prefix %q is not sup_ and hex", prefix)
	}
	if code := c.doorStatus(t, value); code != "" {
		t.Errorf("the supplied value is %s on a door", code)
	}
	// The exact bytes, through the query parameter form, which keeps a
	// trailing space a header parser would trim.
	d := c.well.Dialects[0]
	if resp := c.door(t, d, http.MethodGet, modelsPath(d)+"?key="+url.QueryEscape(value), nil, ""); resp.Status != http.StatusOK {
		t.Errorf("the supplied value as the key parameter: %d %s", resp.Status, excerpt(resp.Body))
	}
	c.expectDoor(t, d, c.door(t, d, http.MethodGet, modelsPath(d)+"?key="+url.QueryEscape(value+" "), nil, ""), "unauthenticated")
}

// case007HashSuppliedValue: a Key created with spec.valueSHA256 opens a
// door by the string the hash is of, its create answer carries neither
// the hash nor a value, its prefix is sup_ and the hash's first eight
// characters, a second create with the same hash and one with that
// string as spec.value are each invalid_field naming no Key, and after
// a rotate the string is unauthenticated.
func case007HashSuppliedValue(t testing.TB, c *client) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	// The value an importer's holders present and its source kept only
	// as this hash.
	value := "conf-hash-supplied-" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(sum[:])
	k := c.object(t, v1.KindKey, "hash-supplied", c.keySpec(map[string]any{"valueSHA256": hash}))
	id := str(k, "status.id")
	defer c.mustDelete(t, v1.KindKey, id)
	if body := canonical(t, k); str(k, "status.value") != "" || strings.Contains(body, hash) || strings.Contains(body, value) {
		t.Errorf("the create answer carries the hash or its value: %s", body)
	}
	if prefix, want := str(k, "status.prefix"), "sup_"+hash[:8]; prefix != want {
		t.Errorf("status.prefix %q, want %q", prefix, want)
	}
	if code := c.doorStatus(t, value); code != "" {
		t.Errorf("the value behind the hash is %s on a door", code)
	}
	// The hash is taken, in whichever form a second create names it.
	for suffix, spec := range map[string]map[string]any{
		"hash-taken":  {"valueSHA256": hash},
		"value-taken": {"value": value},
	} {
		name := c.name(suffix)
		resp := c.apply(t, v1.KindKey, name, map[string]any{"metadata": c.meta(name), "spec": c.keySpec(spec)})
		if resp.Status == http.StatusCreated {
			c.mustDelete(t, v1.KindKey, name)
			t.Errorf("a second create as %s was accepted", suffix)
			continue
		}
		if e := c.expect(t, resp, "invalid_field"); strings.Contains(e.detail, id) {
			t.Errorf("the refusal names the Key holding the value: %s", e.detail)
		}
	}
	// A rotate mints a value of the gateway's and the string stops
	// opening the Key within the cache window.
	resp := c.v1(t, http.MethodPost, "/keys/"+id+"/rotate", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("rotate: %d %s", resp.Status, excerpt(resp.Body))
	}
	fresh := str(resp.json(t), "status.value")
	c.eventually(t, revocationTimeout, "the string behind the hash is refused", func() (bool, string) {
		code := c.doorStatus(t, value)
		return code == "unauthenticated", "the string answers " + strconv.Quote(code)
	})
	if code := c.doorStatus(t, fresh); code != "" {
		t.Errorf("the minted value is %s on a door", code)
	}
}

// spendWindow is a Key spend limit of amount USD over an hour.
func spendWindow(amount string) map[string]any {
	return map[string]any{"limits": map[string]any{"spend": map[string]any{"amount": amount, "currency": "USD", "window": "1h"}}}
}

// case007SpendWindow: a priced request is served under a spend limit,
// the next that would cross it is spend_exceeded with Retry-After, and
// status.usage.window reads the spend the stub's tokens cost.
func case007SpendWindow(t testing.TB, c *client) {
	k := c.object(t, v1.KindKey, "spend", c.keySpec(spendWindow("0.002")))
	id, value := str(k, "status.id"), str(k, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	first := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value)
	if first.Status != http.StatusOK {
		t.Fatalf("the first request: %d %s", first.Status, excerpt(first.Body))
	}
	second := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value)
	c.expectDoor(t, "openai", second, "spend_exceeded")
	if retry, err := strconv.Atoi(second.Header.Get("Retry-After")); err != nil || retry < 1 {
		t.Errorf("Retry-After %q on a spend refusal", second.Header.Get("Retry-After"))
	}
	c.eventually(t, 15*time.Second, "status.usage.window carries the spend", func() (bool, string) {
		read := c.read(t, v1.KindKey, id).json(t)
		spend := str(read, "status.usage.window.spend")
		return spend == "0.0015", "status.usage.window.spend is " + strconv.Quote(spend)
	})
}

// case007BudgetExhausted: the same under a hard Budget is
// budget_exhausted with Retry-After.
func case007BudgetExhausted(t testing.TB, c *client) {
	b := c.object(t, v1.KindBudget, "exhaust", map[string]any{"amount": "0.002", "currency": "USD", "window": "1h", "hard": true})
	defer c.mustDelete(t, v1.KindBudget, str(b, "status.id"))
	k := c.object(t, v1.KindKey, "drawer", c.keySpec(map[string]any{"budget": c.name("exhaust")}))
	defer c.mustDelete(t, v1.KindKey, str(k, "status.id"))
	value := str(k, "status.value")
	if first := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value); first.Status != http.StatusOK {
		t.Fatalf("the first request: %d %s", first.Status, excerpt(first.Body))
	}
	second := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value)
	c.expectDoor(t, "openai", second, "budget_exhausted")
	if retry, err := strconv.Atoi(second.Header.Get("Retry-After")); err != nil || retry < 1 {
		t.Errorf("Retry-After %q on a Budget refusal", second.Header.Get("Retry-After"))
	}
	c.eventually(t, 15*time.Second, "the Budget's status.spent carries the spend", func() (bool, string) {
		read := c.read(t, v1.KindBudget, str(b, "status.id")).json(t)
		return str(read, "status.spent") == "0.0015", "status.spent is " + strconv.Quote(str(read, "status.spent"))
	})
}

// case037SeveralBudgets: a Key listing a wide monthly Budget and a tight
// daily one is refused budget_exhausted when the tight one would pass
// its amount, with the tight one's reset as Retry-After, and the one
// request it served counts in both Budgets' status.spent and keys.
func case037SeveralBudgets(t testing.TB, c *client) {
	wide := c.object(t, v1.KindBudget, "wide", map[string]any{"amount": "1", "currency": "USD", "window": "month"})
	defer c.mustDelete(t, v1.KindBudget, str(wide, "status.id"))
	tight := c.object(t, v1.KindBudget, "tight", map[string]any{"amount": "0.002", "currency": "USD", "window": "24h"})
	defer c.mustDelete(t, v1.KindBudget, str(tight, "status.id"))
	k := c.object(t, v1.KindKey, "layered", c.keySpec(map[string]any{"budgets": []string{c.name("wide"), c.name("tight")}}))
	defer c.mustDelete(t, v1.KindKey, str(k, "status.id"))
	if refs := arr(k, "status.budgets"); len(refs) != 2 {
		t.Fatalf("status.budgets %v", refs)
	}
	value := str(k, "status.value")
	if first := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value); first.Status != http.StatusOK {
		t.Fatalf("the first request: %d %s", first.Status, excerpt(first.Body))
	}
	second := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value)
	c.expectDoor(t, "openai", second, "budget_exhausted")
	if retry, err := strconv.Atoi(second.Header.Get("Retry-After")); err != nil || retry < 1 || retry > 24*3600 {
		t.Errorf("Retry-After %q is not the daily Budget's reset", second.Header.Get("Retry-After"))
	}
	for _, b := range []map[string]any{wide, tight} {
		id := str(b, "status.id")
		c.eventually(t, 15*time.Second, "status.spent of "+id+" carries the one request", func() (bool, string) {
			read := c.read(t, v1.KindBudget, id).json(t)
			return str(read, "status.spent") == "0.0015" && num(read, "status.keys") == 1, "status.spent is " + strconv.Quote(str(read, "status.spent"))
		})
	}
}

// case037AnchoredWindow: a Budget whose window is anchored resets on the
// anchor's grid, a daily one at the anchor's time of day and a monthly
// one on the anchor's day, and an anchor under window none is refused.
func case037AnchoredWindow(t testing.TB, c *client) {
	daily := c.object(t, v1.KindBudget, "daily-at-noon", map[string]any{"amount": "1", "currency": "USD", "window": "24h", "anchor": "2026-01-01T12:30:00Z"})
	defer c.mustDelete(t, v1.KindBudget, str(daily, "status.id"))
	monthly := c.object(t, v1.KindBudget, "monthly-on-15th", map[string]any{"amount": "1", "currency": "USD", "window": "month", "anchor": "2026-01-15T06:00:00Z"})
	defer c.mustDelete(t, v1.KindBudget, str(monthly, "status.id"))
	for _, tc := range []struct {
		obj   map[string]any
		check func(time.Time) bool
		want  string
	}{
		{daily, func(r time.Time) bool { return r.Hour() == 12 && r.Minute() == 30 && r.Second() == 0 }, "12:30:00 on some day"},
		{monthly, func(r time.Time) bool { return r.Day() == 15 && r.Hour() == 6 && r.Minute() == 0 }, "the 15th at 06:00"},
	} {
		read := c.read(t, v1.KindBudget, str(tc.obj, "status.id")).json(t)
		resets, err := time.Parse(time.RFC3339, str(read, "status.resetsAt"))
		if err != nil || !tc.check(resets.UTC()) || !resets.After(time.Now().Add(-time.Minute)) {
			t.Errorf("Budget %s resets at %q, want %s", str(tc.obj, "metadata.name"), str(read, "status.resetsAt"), tc.want)
		}
	}
	resp := c.apply(t, v1.KindBudget, c.name("lifetime-anchored"), map[string]any{"metadata": c.meta(c.name("lifetime-anchored")), "spec": map[string]any{"amount": "1", "window": "none", "anchor": "2026-01-01T00:00:00Z"}})
	c.expect(t, resp, "invalid_field")
}

// case037Restart: a Budget refusing budget_exhausted serves again once
// spec.restartedAt restarts its window, with status.spent counting from
// the restart and status.resetsAt unchanged.
func case037Restart(t testing.TB, c *client) {
	spec := map[string]any{"amount": "0.002", "currency": "USD", "window": "24h"}
	b := c.object(t, v1.KindBudget, "restartable", spec)
	id := str(b, "status.id")
	defer c.mustDelete(t, v1.KindBudget, id)
	k := c.object(t, v1.KindKey, "restarted", c.keySpec(map[string]any{"budgets": []string{c.name("restartable")}}))
	defer c.mustDelete(t, v1.KindKey, str(k, "status.id"))
	value := str(k, "status.value")
	if first := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value); first.Status != http.StatusOK {
		t.Fatalf("the first request: %d %s", first.Status, excerpt(first.Body))
	}
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value), "budget_exhausted")
	before := str(c.read(t, v1.KindBudget, id).json(t), "status.resetsAt")
	spec["restartedAt"] = time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second).Format(time.RFC3339)
	c.object(t, v1.KindBudget, "restartable", spec)
	c.eventually(t, revocationTimeout, "the restarted Budget serves", func() (bool, string) {
		resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value)
		return resp.Status == http.StatusOK, "the door answers " + strconv.Itoa(resp.Status)
	})
	c.eventually(t, 15*time.Second, "status.spent counts from the restart", func() (bool, string) {
		read := c.read(t, v1.KindBudget, id).json(t)
		return str(read, "status.spent") == "0.0015" && str(read, "status.resetsAt") == before,
			"status.spent is " + strconv.Quote(str(read, "status.spent")) + ", status.resetsAt " + str(read, "status.resetsAt") + " was " + before
	})
}

// case039DisabledModel: a Model with spec.disabled true is refused
// model_disabled on a call and on its one-entry read through a Key that
// names it by a glob, and is left out of the Key's model list; set back
// to false, the same Key's call is no longer refused for it, and the Key
// never changed.
func case039DisabledModel(t testing.TB, c *client) {
	if !slices.Contains(c.well.Dialects, "openai") {
		t.Skip("the server has no openai door")
	}
	spec := c.modelSpec(nil, [2]string{"openai", "stub-gpt"})
	m := c.object(t, v1.KindModel, "gate-one", spec)
	defer c.mustDelete(t, v1.KindModel, str(m, "status.id"))
	k := c.object(t, v1.KindKey, "gated", c.keySpec(map[string]any{"models": []string{c.name("gate-*")}}))
	id, value := str(k, "status.id"), str(k, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	version := str(c.read(t, v1.KindKey, id).json(t), "status.version")
	// call is the code a chat to the Model is refused with, "" when it
	// is served.
	call := func() string {
		resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("gate-one"), false), value)
		if resp.Status == http.StatusOK {
			return ""
		}
		return c.doorCode(t, "openai", resp)
	}
	if code := call(); code == "model_disabled" {
		t.Fatal("an enabled Model is refused model_disabled")
	}
	spec["disabled"] = true
	if got := c.object(t, v1.KindModel, "gate-one", spec); field(got, "spec.disabled") != true {
		t.Fatalf("spec.disabled reads %v", field(got, "spec.disabled"))
	}
	c.eventually(t, revocationTimeout, "the disabled Model is refused", func() (bool, string) {
		code := call()
		return code == "model_disabled", "the door answers " + strconv.Quote(code)
	})
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodGet, "/v1/models/"+c.name("gate-one"), nil, value), "model_disabled")
	if names := listedNames(t, "openai", c.door(t, "openai", http.MethodGet, "/v1/models", nil, value)); slices.Contains(names, c.name("gate-one")) {
		t.Errorf("the disabled Model is listed: %v", names)
	}
	delete(spec, "disabled")
	c.object(t, v1.KindModel, "gate-one", spec)
	c.eventually(t, revocationTimeout, "the re-enabled Model is served to the same Key", func() (bool, string) {
		code := call()
		return code != "model_disabled", "the door answers " + strconv.Quote(code)
	})
	if one := c.door(t, "openai", http.MethodGet, "/v1/models/"+c.name("gate-one"), nil, value); one.Status != http.StatusOK {
		t.Errorf("the re-enabled Model's read: %d %s", one.Status, excerpt(one.Body))
	}
	if after := str(c.read(t, v1.KindKey, id).json(t), "status.version"); after != version {
		t.Errorf("the Key changed from version %s to %s", version, after)
	}
}
