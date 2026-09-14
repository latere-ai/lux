// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestCommandTable is spec 014's first row: every command calls the route
// in its row with the method, the addressing, and the flags named, and
// apply dispatches on the document's kind and metadata.name.
func TestCommandTable(t *testing.T) {
	f := newFake(t)
	for _, p := range []string{
		"PUT /v1/providers/openai", "PUT /v1/keys/run-42", "GET /v1/keys/run-42", "GET /v1/models/openai/gpt-5", "GET /v1/keys",
		"GET /v1/models", "DELETE /v1/budgets/team", "POST /v1/keys/run-42/rotate", "GET /v1/usage", "GET /v1/requests",
		"GET /lux/v1/models", "GET /v1/self", "PUT /v1/keys/agent", "PUT /v1/providers/anthropic", "PUT /v1/models/sonnet", "PUT /v1/budgets/q4",
	} {
		method, path, _ := strings.Cut(p, " ")
		body := `{"kind":"x"}` + "\n"
		if strings.HasSuffix(path, "s") && method == "GET" && !strings.Contains(path, "/v1/models/") {
			body = `{"items":[]}` + "\n"
		}
		if method == "DELETE" {
			f.on(method, path, 204, "")
			continue
		}
		f.on(method, path, 200, body)
	}
	provider := write(t, "provider.yaml", providerYAML)
	key := write(t, "key.yaml", keyYAML)
	cases := []struct {
		args    []string
		env     map[string]string
		method  string
		path    string
		query   url.Values
		auth    string
		ifMatch string
		body    string
		ctype   string
	}{
		{args: []string{"apply", "-f", provider}, method: "PUT", path: "/v1/providers/openai", body: providerYAML, ctype: "application/yaml"},
		{args: []string{"apply", "-f", key, "--if-match", "7"}, method: "PUT", path: "/v1/keys/run-42", body: keyYAML, ctype: "application/yaml", ifMatch: `"7"`},
		{args: []string{"get", "key", "run-42"}, method: "GET", path: "/v1/keys/run-42"},
		{args: []string{"get", "keys", "run-42"}, method: "GET", path: "/v1/keys/run-42"},
		{args: []string{"get", "model", "openai/gpt-5"}, method: "GET", path: "/v1/models/openai/gpt-5"},
		{args: []string{"list", "keys", "-l", "team=a", "-l", "env=prod", "--owner", "alice", "--limit", "3"}, method: "GET", path: "/v1/keys", query: url.Values{"label": {"team=a", "env=prod"}, "owner": {"alice"}}},
		{args: []string{"list", "models", "--source", "discovered", "--provider", "openai"}, method: "GET", path: "/v1/models", query: url.Values{"source": {"discovered"}, "provider": {"openai"}}},
		{args: []string{"delete", "budget", "team", "--if-match", "7"}, method: "DELETE", path: "/v1/budgets/team", ifMatch: `"7"`},
		{args: []string{"delete", "budgets", "team", "--if-match", "*"}, method: "DELETE", path: "/v1/budgets/team", ifMatch: `*`},
		{args: []string{"keys", "rotate", "run-42"}, method: "POST", path: "/v1/keys/run-42/rotate"},
		{args: []string{"usage", "--key", "k1", "--key", "key_01J", "--model", "gpt-5", "--provider", "openai", "--owner", "alice", "--from", "2026-09-01T00:00:00Z", "--to", "2026-09-14T00:00:00Z", "--by", "model", "--by", "key", "--interval", "day", "--label", "team=a"},
			method: "GET", path: "/v1/usage", query: url.Values{"key": {"k1", "key_01J"}, "model": {"gpt-5"}, "provider": {"openai"}, "owner": {"alice"}, "from": {"2026-09-01T00:00:00Z"}, "to": {"2026-09-14T00:00:00Z"}, "by": {"model", "key"}, "interval": {"day"}, "label": {"team=a"}}},
		{args: []string{"usage", "--since", "24h"}, method: "GET", path: "/v1/usage", query: url.Values{"from": {"2026-09-13T12:00:00Z"}}},
		{args: []string{"requests", "--status", "refused", "--error", "rate_limited", "--limit", "2", "--key", "k1"}, method: "GET", path: "/v1/requests", query: url.Values{"status": {"refused"}, "error": {"rate_limited"}, "key": {"k1"}}},
		{args: []string{"models"}, env: map[string]string{"LUX_TOKEN": "", "LUX_KEY": "lux_abc"}, method: "GET", path: "/lux/v1/models", auth: "Bearer lux_abc"},
		{args: []string{"whoami"}, method: "GET", path: "/v1/self"},
		{args: []string{"keys", "create", "agent", "--models", "gpt-5,anthropic/*", "--budget", "team"}, method: "PUT", path: "/v1/keys/agent", ctype: "application/json", body: `"kind":"Key"`},
		{args: []string{"providers", "create", "anthropic", "--dialect", "anthropic", "--base-url", "https://api.example.com"}, method: "PUT", path: "/v1/providers/anthropic", ctype: "application/json", body: `"kind":"Provider"`},
		{args: []string{"models", "create", "sonnet", "--target", "anthropic/claude-sonnet"}, method: "PUT", path: "/v1/models/sonnet", ctype: "application/json", body: `"kind":"Model"`},
		{args: []string{"budgets", "create", "q4", "--amount", "500"}, method: "PUT", path: "/v1/budgets/q4", ctype: "application/json", body: `"kind":"Budget"`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			f.reset()
			r := run(t, f.env(tc.env), tc.args...)
			if r.code != 0 {
				t.Fatalf("exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
			}
			reqs := f.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests: %+v", len(reqs), reqs)
			}
			got := reqs[0]
			auth := tc.auth
			if auth == "" {
				auth = "Bearer " + canaryToken
			}
			if got.Method != tc.method || got.Path != tc.path || got.Auth != auth || got.IfMatch != tc.ifMatch || got.ContentType != tc.ctype {
				t.Fatalf("request %+v", got)
			}
			q, _ := url.ParseQuery(got.Query)
			if tc.query == nil {
				tc.query = url.Values{}
			}
			if q.Encode() != tc.query.Encode() {
				t.Fatalf("query %q, want %q", q.Encode(), tc.query.Encode())
			}
			if !strings.Contains(got.Body, tc.body) {
				t.Fatalf("body %q lacks %q", got.Body, tc.body)
			}
			if tc.method == "DELETE" && r.stdout != "" {
				t.Fatalf("delete printed %q", r.stdout)
			}
		})
	}
}

// TestListFollowsEveryPage is the list half of the paging rule at the
// command: two pages are one envelope, and --limit counts items.
func TestListFollowsEveryPage(t *testing.T) {
	f := newFake(t)
	f.answers["GET /v1/keys"] = pagedKeys()
	r := run(t, f.env(nil), "list", "keys")
	if r.code != 0 {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	var list struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list.Items) != 3 || list.NextCursor != nil {
		t.Fatalf("stdout %q: %v", r.stdout, err)
	}
	if len(f.requests()) != 2 {
		t.Fatalf("%d requests", len(f.requests()))
	}
	f.reset()
	r = run(t, f.env(nil), "list", "keys", "--limit", "1")
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list.Items) != 1 || len(f.requests()) != 1 {
		t.Fatalf("limited: stdout %q after %d requests", r.stdout, len(f.requests()))
	}
}
