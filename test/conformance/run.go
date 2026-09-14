// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// mode names which servers a case runs against: every server, a server
// mode one alone, or a file mode one alone.
type mode int

const (
	anyMode mode = iota
	serverMode
	fileMode
)

// testCase is one row of the case table: the group it runs in, its
// name, case<NNN><Name> after the spec whose criterion it proves, and
// what it needs before it can run.
type testCase struct {
	group  string
	name   string
	spec   int
	bearer bool // needs the default token
	stubs  bool // in the stub table: needs Config.StubsURL
	key    bool // needs the suite's Key, and with it the server mode
	mode   mode
	fn     func(t testing.TB, c *client)
}

// groups are the seven of spec 018, in the order they run: the manifest
// group first, because the api group's objects assume the contract, and
// the fixture group last.
var groups = []string{"manifest", "api", "doors", "keys", "identity", "usage", "fixture"}

// cases is the table, assembled from each group's list in group order.
var cases = slices.Concat(manifestCases, apiCases, doorsCases, keysCases, identityCases, usageCases, fixtureCases)

// stubCases lists the cases in the stub table by name, sorted.
func stubCases() []string {
	var out []string
	for _, tc := range cases {
		if tc.stubs {
			out = append(out, tc.name)
		}
	}
	slices.Sort(out)
	return out
}

// Run executes every case the configuration admits against one server,
// one subtest per group and one per case. It fetches GET
// /.well-known/lux once before the first case and configures itself
// from it, mints its Key before the doors group, and deletes everything
// it created, by the run's label, when the test ends.
func Run(t *testing.T, cfg Config) {
	run(t, cfg)
}

// runOption edits the client before a run; only the package's own tests
// pass one.
type runOption func(*client)

// tolerate accepts the OpenAPI drifts of the reference server as this
// tree wires it, by finding, each naming the spec that owes the fix.
func tolerate(drifts map[string]string) runOption {
	return func(c *client) { c.tolerated = drifts }
}

// run is Run with the report returned, for the package's own tests.
func run(t *testing.T, cfg Config, opts ...runOption) *report {
	t.Helper()
	c := newClient(t, cfg)
	for _, o := range opts {
		o(c)
	}
	t.Cleanup(func() { c.teardown(t) })
	for _, g := range groups {
		list := casesOf(g)
		if reason := c.groupSkip(g, list); reason != "" {
			c.rep.drop(g, reason)
			continue
		}
		t.Run(g, func(t *testing.T) {
			if g == "doors" || g == "keys" {
				c.prepareDoors(t)
			}
			for _, tc := range list {
				c.runCase(t, tc)
			}
		})
	}
	c.rep.print(t, stubCases())
	return c.rep
}

// casesOf is the group's rows, in table order.
func casesOf(group string) []testCase {
	var out []testCase
	for _, tc := range cases {
		if tc.group == group {
			out = append(out, tc)
		}
	}
	return out
}

// needsToken reports a case that cannot run without the default bearer:
// one that asks for it on a server mode server, since the file mode
// asks no bearer of anyone.
func (c *client) needsToken(tc testCase) bool { return tc.bearer && !c.fileMode() && c.token == "" }

// serverOnly reports a case a file mode server cannot run: one that
// writes desired state or drives a door with a minted Key.
func (c *client) serverOnly(tc testCase) bool {
	return c.fileMode() && (tc.mode == serverMode || tc.key)
}

// groupSkip is the reason a whole group is dropped: every case in it
// needs a bearer and there is none, every case needs the server mode
// and the server reads a directory, or the file mode's /v1 is out of
// reach.
func (c *client) groupSkip(group string, list []testCase) string {
	allToken, allServer := true, true
	for _, tc := range list {
		if !c.needsToken(tc) {
			allToken = false
		}
		if !c.serverOnly(tc) {
			allServer = false
		}
	}
	switch {
	case allToken:
		return EnvToken + " is unset and every case in the " + group + " group needs a bearer"
	case allServer:
		return "the server is in file mode and answers read_only, so nothing in the " + group + " group can be created"
	case c.fileMode() && c.api == "" && group != "fixture":
		return "the server is in file mode, where /v1 is on the internal listener, and " + EnvInternalURL + " is unset"
	}
	return ""
}

// caseSkip is the reason one case skips, or empty.
func (c *client) caseSkip(tc testCase) string {
	switch {
	case c.needsToken(tc):
		return EnvToken + " is unset and this case needs a bearer"
	case tc.key && c.fileMode():
		return "the server is in file mode and answers read_only, so no Key can be minted"
	case tc.mode == serverMode && c.fileMode():
		return "the server is in file mode and answers read_only"
	case tc.mode == fileMode && !c.fileMode():
		return "the server is in server mode"
	case tc.key && c.key == nil:
		return "the suite could not mint its Key, and this case drives a door with it"
	case tc.stubs && c.stubs == nil:
		return EnvStubsURL + " is unset, and this case reads a stub"
	}
	return ""
}

// runCase runs one case as a subtest and records its outcome.
func (c *client) runCase(t *testing.T, tc testCase) {
	full := tc.group + "/" + tc.name
	ok := t.Run(tc.name, func(t *testing.T) {
		if reason := c.caseSkip(tc); reason != "" {
			c.rep.skip(full, reason)
			t.Skip(reason)
		}
		defer func() {
			if t.Skipped() {
				c.rep.mark(full, skipped)
			}
		}()
		tc.fn(t, c)
	})
	c.rep.mu.Lock()
	o, seen := c.rep.outcomes[full]
	c.rep.mu.Unlock()
	if seen && o == skipped {
		return
	}
	if ok {
		c.rep.mark(full, passed)
	} else {
		c.rep.mark(full, failed)
	}
}

// skip records why a case skips and skips it. The case is named by the
// last two segments of the subtest's name, the group and the case,
// whatever test called Run.
func (c *client) skip(t testing.TB, reason string) {
	t.Helper()
	parts := strings.Split(t.Name(), "/")
	name := parts[len(parts)-1]
	if len(parts) >= 2 {
		name = parts[len(parts)-2] + "/" + name
	}
	c.rep.skip(name, reason)
	t.Skip(reason)
}

// newClient fetches the well-known document, the OpenAPI document, and
// the stubs document, and settles the default token.
func newClient(t testing.TB, cfg Config) *client {
	t.Helper()
	if cfg.URL == "" {
		t.Fatal("Config.URL is empty")
	}
	ctx, cancel := context.WithTimeout(t.Context(), runTimeout)
	t.Cleanup(cancel)
	run := strings.ToLower(v1.NewID("", time.Now(), nil))
	c := &client{cfg: cfg, http: newHTTPClient(), ctx: ctx, run: run, label: "conformance=" + run, rep: newReport(run), sentences: map[string]string{}, violations: map[string]bool{}, fixtures: previous}
	if cfg.Token != nil {
		if tok, ok := cfg.Token(cfg.Subject); ok {
			c.token = tok
		}
	}
	resp := c.request(t, http.MethodGet, cfg.URL+"/.well-known/lux", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET %s/.well-known/lux: %d %s", cfg.URL, resp.Status, excerpt(resp.Body))
	}
	if err := json.Unmarshal(resp.Body, &c.well); err != nil {
		t.Fatalf("GET %s/.well-known/lux: %v\n%s", cfg.URL, err, excerpt(resp.Body))
	}
	if c.well.Name != "lux" || c.well.API == "" || len(c.well.Doors) == 0 {
		t.Fatalf("GET %s/.well-known/lux does not describe a Lux server: %s", cfg.URL, excerpt(resp.Body))
	}
	switch c.well.Mode {
	case "server":
		c.api = c.well.API
	case "file":
		if cfg.InternalURL != "" {
			c.api = cfg.InternalURL + "/v1"
		}
	default:
		t.Fatalf("GET %s/.well-known/lux: mode %q is not server or file", cfg.URL, c.well.Mode)
	}
	if c.api != "" {
		doc := c.request(t, http.MethodGet, c.api+"/openapi.json", nil)
		if doc.Status != http.StatusOK {
			t.Fatalf("GET %s/openapi.json: %d %s", c.api, doc.Status, excerpt(doc.Body))
		}
		parsed, err := parseDocument(doc.Body)
		if err != nil {
			t.Fatalf("GET %s/openapi.json: %v", c.api, err)
		}
		c.doc = parsed
	}
	c.loadStubs(t)
	return c
}

// prepareDoors mints the suite's Key and applies the Providers and
// Models the doors and keys groups drive, once. A failure leaves the Key
// nil and the cases that need it skip naming it.
func (c *client) prepareDoors(t *testing.T) {
	t.Helper()
	if c.prepared || c.fileMode() || c.token == "" {
		return
	}
	c.prepared = true
	if !t.Run("case007SuiteMintsItsKey", func(t *testing.T) { c.mintKey(t) }) {
		c.rep.mark("doors/case007SuiteMintsItsKey", failed)
		return
	}
	c.rep.mark("doors/case007SuiteMintsItsKey", passed)
	if !t.Run("fixtures", func(t *testing.T) { c.applyFixtures(t) }) {
		c.key = nil
	}
}

// mintKey is case007SuiteMintsItsKey: the suite's Key, applied through
// PUT /v1/keys/{name} with the selector every suite Model matches, its
// value read once from the create response and never asked for again.
func (c *client) mintKey(t testing.TB) {
	t.Helper()
	name := c.name("key")
	resp := c.apply(t, v1.KindKey, name, map[string]any{"metadata": c.meta(name), "spec": map[string]any{"models": []string{c.name("*")}}})
	if resp.Status != http.StatusCreated {
		t.Fatalf("PUT /keys/%s: %d %s", name, resp.Status, excerpt(resp.Body))
	}
	obj := resp.json(t)
	k := &suiteKey{id: str(obj, "status.id"), name: name, value: str(obj, "status.value"), prefix: str(obj, "status.prefix")}
	if k.value == "" {
		t.Fatalf("the create response carries no status.value: %s", excerpt(resp.Body))
	}
	if !strings.HasPrefix(k.value, "lux_") || len(k.value) != 44 {
		t.Errorf("status.value %q is not lux_ and forty characters", k.value)
	}
	if k.prefix != k.value[:12] {
		t.Errorf("status.prefix %q is not the value's first twelve characters", k.prefix)
	}
	if !strings.HasPrefix(k.id, v1.PrefixKey) {
		t.Errorf("status.id %q does not begin with %s", k.id, v1.PrefixKey)
	}
	c.key = k
}

// providerSpec is one suite Provider: pointed at the stub of its dialect
// when there are stubs, and at a host under example.com the server is
// never asked to dial otherwise, because every case without stubs is
// refused before the forward. Discovery and health are off, so the
// server dials nothing on its own.
func (c *client) providerSpec(dialect, timeout string) map[string]any {
	base := "https://" + dialect + ".provider.example.com/v1"
	if c.stubs != nil {
		base = c.stubs.Providers[dialect] + "/v1"
		if dialect == "gemini" {
			base = c.stubs.Providers[dialect] + "/v1beta"
		}
	}
	return map[string]any{
		"dialect": dialect, "baseURL": base, "timeout": timeout,
		"credential": map[string]any{"value": c.credential(dialect)},
		"discovery":  map[string]any{"mode": "none"},
		"health":     map[string]any{"mode": "none"},
	}
}

// credential is the value the suite's Providers carry: the stubs
// document's, which every stub provider checks, or a placeholder for a
// Provider no request ever reaches.
func (c *client) credential(dialect string) string {
	if c.stubs != nil && c.stubs.Credential != "" {
		return c.stubs.Credential
	}
	return "sk-conf-" + dialect + "-example"
}

// modelSpec is one suite Model: a target per (provider suffix, upstream
// name) pair in priority order, and the pricing when given.
func (c *client) modelSpec(pricing map[string]any, targets ...[2]string) map[string]any {
	ts := make([]map[string]any, 0, len(targets))
	for i, tg := range targets {
		ts = append(ts, map[string]any{"provider": c.name(tg[0]), "model": tg[1], "priority": i})
	}
	spec := map[string]any{"targets": ts}
	if pricing != nil {
		spec["pricing"] = pricing
	}
	return spec
}

// perMillion prices a Model at one unit of USD per million tokens in
// and out, so a cost is the token count in micro-units.
var perMillion = map[string]any{"currency": "USD", "per": 1000000, "input": "1", "output": "1"}

// applyFixtures applies the Providers and Models the doors and keys
// groups drive: one Provider per dialect the server lists, a slow one
// for the timeout case, and the Models the cases name.
func (c *client) applyFixtures(t testing.TB) {
	t.Helper()
	for _, d := range c.well.Dialects {
		c.object(t, v1.KindProvider, d, c.providerSpec(d, "10s"))
		c.providers = append(c.providers, c.name(d))
	}
	c.object(t, v1.KindProvider, "slow", c.providerSpec("openai", "1s"))
	c.providers = append(c.providers, c.name("slow"))
	models := []struct {
		suffix  string
		pricing map[string]any
		targets [][2]string
	}{
		{"gpt", nil, [][2]string{{"openai", "stub-gpt"}}},
		{"same", nil, [][2]string{{"openai", c.name("same")}}},
		{"alias", nil, [][2]string{{"openai", "stub-alias-upstream"}}},
		{"claude", nil, [][2]string{{"anthropic", "stub-claude"}}},
		{"gemini", nil, [][2]string{{"gemini", "stub-gemini"}}},
		{"luxm", nil, [][2]string{{"lux", "stub-lux"}}},
		{"fail500", nil, [][2]string{{"openai", "fail-500"}}},
		{"fail400", nil, [][2]string{{"openai", "fail-400"}}},
		{"hang", nil, [][2]string{{"slow", "hang"}}},
		{"fallback", nil, [][2]string{{"openai", "fail-500"}, {"openai", "stub-gpt"}}},
		{"events", nil, [][2]string{{"openai", "events-3"}}},
		{"tokens", perMillion, [][2]string{{"openai", "tokens-1000-500"}}},
	}
	for _, m := range models {
		if !slices.Contains(c.well.Dialects, m.targets[0][0]) && m.targets[0][0] != "slow" {
			continue
		}
		c.object(t, v1.KindModel, m.suffix, c.modelSpec(m.pricing, m.targets...))
		c.models = append(c.models, c.name(m.suffix))
	}
	slices.Sort(c.models)
}

// teardown deletes every object the run created, by its label and
// nothing else, Keys first and Providers last so no delete is refused
// for a dependant. What it finds is what a case failed to remove.
func (c *client) teardown(t testing.TB) {
	t.Helper()
	if c.api == "" || c.token == "" || c.fileMode() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), time.Minute)
	defer cancel()
	c.ctx = ctx
	for _, kind := range []string{v1.KindKey, v1.KindModel, v1.KindBudget, v1.KindProvider} {
		for range 20 {
			resp := c.list(t, kind, "label="+c.label+"&limit=200")
			if resp.Status != http.StatusOK {
				t.Logf("teardown: listing %ss by label: %d %s", kind, resp.Status, excerpt(resp.Body))
				break
			}
			items := c.items(t, resp)
			if len(items) == 0 {
				break
			}
			for _, it := range items {
				id := str(it, "status.id")
				if resp := c.del(t, kind, id); resp.Status != http.StatusNoContent {
					t.Logf("teardown: DELETE %s %s: %d %s", kind, id, resp.Status, excerpt(resp.Body))
				}
			}
		}
	}
}
