// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The usage group is spec 009 over the two routes of spec 011: one
// request through a door is exactly one record at GET /v1/requests with
// the tokens, the cost, the model, the provider, and no content; a
// refused request has a record too; and GET /v1/usage aggregates them
// under every dimension and interval the parameters admit.
var usageCases = []testCase{
	{group: "usage", name: "case009RefusedRequestHasARecord", spec: 9, bearer: true, key: true, fn: case009RefusedRequestHasARecord},
	{group: "usage", name: "case009RecordHasTheStubsTokens", spec: 9, bearer: true, key: true, stubs: true, fn: case009RecordHasTheStubsTokens},
	{group: "usage", name: "case009UsageAggregates", spec: 9, bearer: true, key: true, fn: case009UsageAggregates},
	{group: "usage", name: "case038RedactUsage", spec: 38, bearer: true, mode: serverMode, fn: case038RedactUsage},
}

// recordTimeout bounds the wait for a record or an aggregate row, which
// the server writes after the response and flushes on
// LUX_METERING_FLUSH.
const recordTimeout = 20 * time.Second

// recordOf polls GET /v1/requests for the Key until the record with the
// request id is there, and asserts it is there once.
func (c *client) recordOf(t testing.TB, keyID, requestID string) map[string]any {
	t.Helper()
	var found map[string]any
	c.eventually(t, recordTimeout, "the record "+requestID, func() (bool, string) {
		resp := c.v1(t, http.MethodGet, "/requests?key="+keyID+"&limit=100", nil)
		if resp.Status != http.StatusOK {
			return false, sprintf("GET /requests: %d %s", resp.Status, excerpt(resp.Body))
		}
		body := resp.json(t)
		if str(body, "source") != "memory" && str(body, "source") != "archive" {
			t.Errorf("source %q", str(body, "source"))
		}
		n := 0
		for _, it := range arr(body, "items") {
			if str(it, "id") == requestID {
				n++
				found, _ = it.(map[string]any)
			}
		}
		if n > 1 {
			t.Fatalf("%d records carry the id %s", n, requestID)
		}
		return n == 1, "no record yet among " + itoa(len(arr(body, "items")))
	})
	return found
}

// case009RefusedRequestHasARecord: a request refused before any provider
// has a record saying refused with the code, no attempt, and zero
// tokens and cost.
func case009RefusedRequestHasARecord(t testing.TB, c *client) {
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("nobody"), false), c.key.value)
	c.expectDoor(t, "openai", resp, "model_not_found")
	rec := c.recordOf(t, c.key.id, resp.ID)
	if str(rec, "status") != "refused" || str(rec, "error") != "model_not_found" || len(arr(rec, "attempts")) != 0 {
		t.Errorf("record %s", canonical(t, rec))
	}
	if num(rec, "tokens.input") != 0 || num(rec, "cost.amount") != 0 || field(rec, "cost.priced") != false {
		t.Errorf("a refused record carries tokens or cost: %s", canonical(t, rec))
	}
	if str(rec, "key.id") != c.key.id || str(rec, "key.prefix") != c.key.prefix || str(rec, "door") != "openai" {
		t.Errorf("record attribution %s", canonical(t, rec))
	}
	if c.cfg.Subject != "" && str(rec, "owner") != c.cfg.Subject {
		t.Errorf("owner %q, want %q", str(rec, "owner"), c.cfg.Subject)
	}
}

// case009RecordHasTheStubsTokens: a served request's record carries the
// stub's fixed token counts, the cost the Model's pricing gives them as
// an integer, the model and the provider by name and id, and none of the
// prompt.
func case009RecordHasTheStubsTokens(t testing.TB, c *client) {
	canary := "conf-canary-" + c.run + "-never-in-a-record"
	body := chat(c.name("tokens"), false)
	body["messages"] = []map[string]any{{"role": "user", "content": canary}}
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", body, c.key.value)
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	rec := c.recordOf(t, c.key.id, resp.ID)
	raw := canonical(t, rec)
	if strings.Contains(raw, canary) || strings.Contains(raw, c.key.value) || strings.Contains(raw, c.credential("openai")) {
		t.Errorf("the record carries content or a credential: %s", raw)
	}
	if str(rec, "status") != "ok" || str(rec, "error") != "" {
		t.Errorf("status %q error %q", str(rec, "status"), str(rec, "error"))
	}
	if num(rec, "tokens.input") != 1000 || num(rec, "tokens.output") != 500 || field(rec, "tokens.estimated") != false {
		t.Errorf("tokens %s", canonical(t, rec["tokens"]))
	}
	// One unit per million tokens in and out: 1500 tokens are 1500
	// micro-units, an integer and never a float.
	if num(rec, "cost.amount") != 1500 || str(rec, "cost.currency") != "USD" || field(rec, "cost.priced") != true {
		t.Errorf("cost %s", canonical(t, rec["cost"]))
	}
	if str(rec, "model.name") != c.name("tokens") || !strings.HasPrefix(str(rec, "model.id"), v1.PrefixModel) {
		t.Errorf("model %s", canonical(t, rec["model"]))
	}
	if str(rec, "provider.name") != c.name("openai") || !strings.HasPrefix(str(rec, "provider.id"), v1.PrefixProvider) || str(rec, "upstreamModel") != "tokens-1000-500" {
		t.Errorf("provider %s upstreamModel %q", canonical(t, rec["provider"]), str(rec, "upstreamModel"))
	}
	if str(rec, "route") != "/openai/v1/chat/completions" || field(rec, "translated") != false || len(arr(rec, "attempts")) != 1 {
		t.Errorf("route %q translated %v attempts %v", str(rec, "route"), rec["translated"], rec["attempts"])
	}
}

// case009UsageAggregates: a dedicated Key's requests sum to the same
// count under every by dimension and every interval, every cost is an
// integer, and no row sums two currencies.
func case009UsageAggregates(t testing.TB, c *client) {
	k := c.object(t, v1.KindKey, "usage", c.keySpec(nil))
	id, value := str(k, "status.id"), str(k, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	n := 3
	for range n {
		resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("nobody"), false), value)
		c.expectDoor(t, "openai", resp, "model_not_found")
	}
	if c.stubs != nil {
		if resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("tokens"), false), value); resp.Status == http.StatusOK {
			n++
		}
	}
	c.eventually(t, recordTimeout, "the aggregates carry the requests", func() (bool, string) {
		resp := c.v1(t, http.MethodGet, "/usage?key="+id, nil)
		if resp.Status != http.StatusOK {
			return false, sprintf("GET /usage: %d %s", resp.Status, excerpt(resp.Body))
		}
		return sumRequests(resp.json(t)) == n, sprintf("%d of %d requests aggregated", sumRequests(resp.json(t)), n)
	})
	for _, by := range []string{"", "key", "model", "provider", "owner", "door", "status", "key,door,status"} {
		for _, interval := range []string{"", "none", "hour", "day", "month"} {
			query := "key=" + id
			if by != "" {
				query += "&by=" + by
			}
			if interval != "" {
				query += "&interval=" + interval
			}
			resp := c.v1(t, http.MethodGet, "/usage?"+query, nil)
			if resp.Status != http.StatusOK {
				t.Errorf("GET /usage?%s: %d %s", query, resp.Status, excerpt(resp.Body))
				continue
			}
			body := resp.json(t)
			if got := sumRequests(body); got != n {
				t.Errorf("GET /usage?%s sums %d requests, want %d", query, got, n)
			}
			for _, row := range arr(body, "items") {
				if !integral(row, "cost") {
					t.Errorf("GET /usage?%s: cost %v is not an integer", query, field(row, "cost"))
				}
				if by == "key" && str(row, "dimensions.key") != id {
					t.Errorf("GET /usage?%s: dimensions %v", query, field(row, "dimensions"))
				}
			}
		}
	}
	// The Key by name reads the same, a name that names nothing reads an
	// empty items rather than a refusal, and the parameters outside the
	// table are invalid_field at their names.
	if byName := c.v1(t, http.MethodGet, "/usage?key="+c.name("usage"), nil); byName.Status != http.StatusOK || sumRequests(byName.json(t)) != n {
		t.Errorf("GET /usage by the Key's name: %d %s", byName.Status, excerpt(byName.Body))
	}
	if none := c.v1(t, http.MethodGet, "/usage?key="+c.name("nobody"), nil); none.Status != http.StatusOK || len(arr(none.json(t), "items")) != 0 {
		t.Errorf("GET /usage by a name that names nothing: %d %s", none.Status, excerpt(none.Body))
	}
	e := c.expect(t, c.v1(t, http.MethodGet, "/usage?key="+id+"&by=a,b,c,d", nil), "invalid_field")
	if len(e.paths) != 1 || e.paths[0] != "by" {
		t.Errorf("four dimensions name %v", e.paths)
	}
	c.expect(t, c.v1(t, http.MethodGet, "/usage?key="+id+"&interval=week", nil), "invalid_field")
	c.expect(t, c.v1(t, http.MethodGet, "/requests?key="+id+"&source=memory", nil), "invalid_field")
	c.expect(t, c.v1(t, http.MethodGet, "/requests?key="+id+"&limit=1001", nil), "invalid_field")
}

// sumRequests adds the requests member over a usage answer's rows.
func sumRequests(body map[string]any) int {
	total := 0
	for _, row := range arr(body, "items") {
		total += int(num(row, "requests"))
	}
	return total
}

// integral reports whether the number at a path was written without a
// fraction.
func integral(m any, path string) bool {
	data, err := json.Marshal(field(m, path))
	return err == nil && !strings.ContainsAny(string(data), ".eE")
}

// case038RedactUsage: POST /v1/usage/redact takes a rendered owner, and
// answers the rows it rewrote, zero for an owner no row carries, or
// forbidden when the server's authorizer keeps redaction from this
// subject; an owner that is no rendered subject is invalid_field, an
// unknown member invalid_request, and a body that is not JSON
// unsupported_media_type, each before the authorizer is asked.
func case038RedactUsage(t testing.TB, c *client) {
	owner := "https://conformance.invalid|" + c.run
	resp := c.v1(t, http.MethodPost, "/usage/redact", map[string]any{"owner": owner})
	switch resp.Status {
	case http.StatusOK:
		if body := resp.json(t); num(body, "rows") != 0 || !integral(body, "rows") {
			t.Errorf("redacting an owner no row carries answered %s", excerpt(resp.Body))
		}
	case http.StatusForbidden:
		c.expect(t, resp, "forbidden")
	default:
		t.Errorf("POST /usage/redact: %d %s", resp.Status, excerpt(resp.Body))
	}
	c.expect(t, c.v1(t, http.MethodPost, "/usage/redact", map[string]any{"owner": "not-a-subject"}), "invalid_field")
	c.expect(t, c.v1(t, http.MethodPost, "/usage/redact", map[string]any{"owner": owner, "keys": []string{}}), "invalid_request")
	c.expect(t, c.v1(t, http.MethodPost, "/usage/redact", "owner: x", header("Content-Type", "application/yaml")), "unsupported_media_type")
}
