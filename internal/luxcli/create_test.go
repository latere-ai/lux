// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestFlagFormsBuildTheManifest is spec 014's row: each create flag form
// builds the manifest the table says and applies it through PUT, and the
// manifest lux keys create builds decodes to the object the equivalent
// file decodes to, so Resolve, a deterministic function of that object,
// resolves them identically.
func TestFlagFormsBuildTheManifest(t *testing.T) {
	f := newFake(t)
	for _, p := range []string{"/v1/keys/agent", "/v1/providers/anthropic", "/v1/models/sonnet", "/v1/budgets/q4"} {
		f.on("PUT", p, 201, `{"kind":"x"}`+"\n")
	}
	cases := []struct {
		args []string
		path string
		file string
	}{
		{
			[]string{"keys", "create", "agent", "--models", "sonnet, anthropic/*", "--rpm", "120", "--tpm", "0", "--spend", "10/24h", "--budget", "q4", "--ttl", "720h", "--label", "team=research", "--label", "env=prod", "--passthrough"},
			"/v1/keys/agent",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: agent\n  labels: {team: research, env: prod}\nspec:\n  models: [sonnet, \"anthropic/*\"]\n  limits:\n    requestsPerMinute: 120\n    tokensPerMinute: 0\n    spend: {amount: \"10\", window: 24h}\n  budget: q4\n  ttl: 720h\n  passthrough: true\n",
		},
		{
			[]string{"keys", "create", "agent", "--models", "sonnet"},
			"/v1/keys/agent",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: agent\nspec:\n  models: [sonnet]\n",
		},
		{
			[]string{"providers", "create", "anthropic", "--dialect", "anthropic", "--base-url", "https://api.example.com", "--header", "X-Team=research"},
			"/v1/providers/anthropic",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: anthropic\nspec:\n  dialect: anthropic\n  baseURL: https://api.example.com\n  headers: {X-Team: research}\n",
		},
		{
			[]string{"models", "create", "sonnet", "--target", "anthropic/claude-sonnet@80:1", "--target", "bedrock/anthropic.claude@20", "--target", "fallback/sonnet", "--price-input", "3", "--price-output", "15", "--fallback", "never"},
			"/v1/models/sonnet",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: sonnet\nspec:\n  targets:\n    - {provider: anthropic, model: claude-sonnet, weight: 80, priority: 1}\n    - {provider: bedrock, model: anthropic.claude, weight: 20}\n    - {provider: fallback, model: sonnet}\n  fallback: never\n  pricing: {input: \"3\", output: \"15\"}\n",
		},
		{
			[]string{"budgets", "create", "q4", "--amount", "500", "--currency", "EUR", "--window", "month", "--soft"},
			"/v1/budgets/q4",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: q4\nspec:\n  amount: \"500\"\n  currency: EUR\n  window: month\n  hard: false\n",
		},
		{
			[]string{"keys", "create", "agent", "--models", "sonnet", "--budget", "q4, member-weekly"},
			"/v1/keys/agent",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: agent\nspec:\n  models: [sonnet]\n  budgets: [q4, member-weekly]\n",
		},
		{
			[]string{"budgets", "create", "q4", "--amount", "10", "--window", "168h", "--anchor", "2026-09-14T00:00:00Z"},
			"/v1/budgets/q4",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: q4\nspec:\n  amount: \"10\"\n  window: 168h\n  anchor: 2026-09-14T00:00:00Z\n",
		},
		{
			[]string{"budgets", "create", "q4", "--amount", "12.50"},
			"/v1/budgets/q4",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: q4\nspec:\n  amount: \"12.5\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[:2], " ")+" "+tc.path, func(t *testing.T) {
			f.reset()
			r := run(t, f.env(nil), tc.args...)
			if r.code != 0 {
				t.Fatalf("exit %d stderr %q", r.code, r.stderr)
			}
			got := f.last(t)
			if got.Method != "PUT" || got.Path != tc.path || got.ContentType != "application/json" {
				t.Fatalf("request %+v", got)
			}
			if strings.Contains(got.Body, `"status"`) {
				t.Fatalf("the body carries a status: %s", got.Body)
			}
			built, err := manifest.Decode([]byte(got.Body), manifest.MediaJSON, manifest.Hint{})
			if err != nil {
				t.Fatalf("the body does not decode: %v\n%s", err, got.Body)
			}
			written, err := manifest.Decode([]byte(tc.file), manifest.MediaYAML, manifest.Hint{})
			if err != nil {
				t.Fatalf("the file does not decode: %v", err)
			}
			if !reflect.DeepEqual(built, written) {
				b, _ := json.Marshal(built)
				w, _ := json.Marshal(written)
				t.Fatalf("the flags built\n%s\nthe file decodes to\n%s", b, w)
			}
			// Resolve is deterministic in the object, so equal objects
			// resolve equally; the Budget needs no Lookup to show it.
			if b, ok := built.(*v1.Budget); ok {
				r1, err1 := manifest.Resolve(context.Background(), b, manifest.Options{Now: func() time.Time { return now }})
				r2, err2 := manifest.Resolve(context.Background(), written, manifest.Options{Now: func() time.Time { return now }})
				if err1 != nil || err2 != nil || !reflect.DeepEqual(r1, r2) {
					t.Fatalf("resolve differs: %v %v", err1, err2)
				}
			}
		})
	}
	// A Provider's credential goes into the body from the environment and
	// nowhere else.
	f.reset()
	r := run(t, f.env(map[string]string{"ANTHROPIC_API_KEY": canaryCred}), "providers", "create", "anthropic", "--dialect", "anthropic", "--base-url", "https://api.example.com", "--credential-from-env", "ANTHROPIC_API_KEY")
	got := f.last(t)
	if r.code != 0 || !strings.Contains(got.Body, `"credential":{"value":"`+canaryCred+`"}`) || strings.Contains(r.stdout+r.stderr, canaryCred) {
		t.Fatalf("exit %d body %s stdout %q stderr %q", r.code, got.Body, r.stdout, r.stderr)
	}
}

// TestDryRunAppliesNothing is spec 014's row: --dry-run prints the
// manifest and sends nothing, in JSON by default and in YAML under -o
// yaml, and never the credential.
func TestDryRunAppliesNothing(t *testing.T) {
	f := newFake(t)
	env := f.env(map[string]string{"ANTHROPIC_API_KEY": canaryCred})
	r := run(t, env, "keys", "create", "agent", "--models", "sonnet", "--budget", "q4", "--dry-run")
	if r.code != 0 || len(f.requests()) != 0 {
		t.Fatalf("exit %d, %d requests, stderr %q", r.code, len(f.requests()), r.stderr)
	}
	obj, err := manifest.Decode([]byte(r.stdout), manifest.MediaJSON, manifest.Hint{})
	if err != nil || obj.Kind() != "Key" || obj.Name() != "agent" || strings.Contains(r.stdout, `"status"`) {
		t.Fatalf("dry-run json %q: %v", r.stdout, err)
	}
	r = run(t, env, "providers", "create", "anthropic", "--dialect", "anthropic", "--base-url", "https://api.example.com", "--credential-from-env", "ANTHROPIC_API_KEY", "--dry-run", "-o", "yaml")
	if r.code != 0 || len(f.requests()) != 0 || !strings.HasPrefix(r.stdout, "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\n") || strings.Contains(r.stdout, canaryCred) || strings.Contains(r.stdout, "status") {
		t.Fatalf("dry-run yaml:\n%s\nstderr %q", r.stdout, r.stderr)
	}
	if _, err := manifest.Decode([]byte(r.stdout), manifest.MediaYAML, manifest.Hint{}); err != nil {
		t.Fatalf("the printed manifest does not decode: %v", err)
	}
	for _, mode := range []string{"table", "wide"} {
		r = run(t, env, "budgets", "create", "q4", "--amount", "5", "--dry-run", "-o", mode)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "apiVersion: ") {
			t.Fatalf("dry-run under %s:\n%s", mode, r.stdout)
		}
	}
	// No credential is needed for a dry run: the manifest is local.
	r = run(t, map[string]string{}, "models", "create", "sonnet", "--target", "anthropic/claude-sonnet", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, `"kind":"Model"`) {
		t.Fatalf("dry-run without credentials: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

func TestFlagFormUsageErrors(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"keys", "create"}, "lux keys create takes a name."},
		{[]string{"keys", "create", "agent"}, "-models is required."},
		{[]string{"keys", "create", "agent", "--models", "x", "--spend", "ten"}, "-spend takes an amount"},
		{[]string{"keys", "create", "agent", "--models", "x", "--ttl", "soon"}, "-ttl takes a duration"},
		{[]string{"keys", "create", "agent", "--models", "x", "--label", "team"}, `-label takes k=v, not "team".`},
		{[]string{"providers", "create", "p"}, "-dialect and -base-url are required."},
		{[]string{"providers", "create", "p", "--dialect", "openai", "--base-url", "https://x", "--header", "bad"}, `-header takes k=v, not "bad".`},
		{[]string{"providers", "create", "p", "--dialect", "openai", "--base-url", "https://x", "--credential-from-env", "NOPE"}, "The variable NOPE is unset or empty."},
		{[]string{"models", "create", "m"}, "-target is required at least once."},
		{[]string{"models", "create", "m", "--target", "noslash"}, "-target takes <provider>/<model>"},
		{[]string{"models", "create", "m", "--target", "/model"}, "-target takes <provider>/<model>"},
		{[]string{"models", "create", "m", "--target", "p/m@heavy"}, "-target takes <provider>/<model>"},
		{[]string{"models", "create", "m", "--target", "p/m@1:high"}, "-target takes <provider>/<model>"},
		{[]string{"models", "create", "m", "--target", "p/m", "--price-input", "3"}, "-price-input and -price-output go together."},
		{[]string{"models", "create", "m", "--target", "p/m", "--price-input", "3", "--price-output", "lots"}, "A price is a decimal amount"},
		{[]string{"budgets", "create", "b"}, "-amount is required."},
		{[]string{"budgets", "create", "b", "--amount", "-5"}, "-amount takes a decimal amount"},
		{[]string{"budgets", "create", "b", "--amount", "5", "--anchor", "monday"}, "-anchor takes an RFC 3339 instant"},
	}
	for _, tc := range cases {
		f.reset()
		r := run(t, f.env(nil), tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) || len(f.requests()) != 0 || r.stdout != "" {
			t.Errorf("%v: exit %d, %d requests\nstderr %q\nwant %q", tc.args, r.code, len(f.requests()), r.stderr, tc.want)
		}
	}
}
