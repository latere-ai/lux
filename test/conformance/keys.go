// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The keys group is spec 007 at the doors: a rotate invalidates the old
// value, each Key state refuses with its code, a rate refusal carries
// Retry-After, a spend refusal and a hard Budget refusal carry theirs, an
// unpriced Model under a Budget is model_unpriced, and a supplied value
// opens a door by its exact bytes.
var keysCases = []testCase{
	{group: "keys", name: "case007RotateInvalidatesTheOldValue", spec: 7, bearer: true, key: true, fn: case007RotateInvalidatesTheOldValue},
	{group: "keys", name: "case007KeyStates", spec: 7, bearer: true, key: true, fn: case007KeyStates},
	{group: "keys", name: "case007RateLimited", spec: 7, bearer: true, key: true, fn: case007RateLimited},
	{group: "keys", name: "case007UnpricedUnderABudget", spec: 7, bearer: true, key: true, fn: case007UnpricedUnderABudget},
	{group: "keys", name: "case007SuppliedValue", spec: 7, bearer: true, key: true, fn: case007SuppliedValue},
	{group: "keys", name: "case007SpendWindow", spec: 7, bearer: true, key: true, stubs: true, fn: case007SpendWindow},
	{group: "keys", name: "case007BudgetExhausted", spec: 7, bearer: true, key: true, stubs: true, fn: case007BudgetExhausted},
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
