// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The api group is spec 011's grammar over the four kinds: the four
// routes per kind, apply as create then update, the preconditions,
// addressing by id or name, a Model name with slashes, rotate,
// budget_in_use, pagination and the list filters, the envelope, the
// request id, the rate limit headers, the well-known document, the
// file mode's read_only, and every answer held to the OpenAPI document.
var apiCases = []testCase{
	{group: "api", name: "case011WellKnown", spec: 11, fn: case011WellKnown},
	{group: "api", name: "case011NoCORS", spec: 11, fn: case011NoCORS},
	{group: "api", name: "case011ListReads", spec: 11, bearer: true, fn: case011ListReads},
	{group: "api", name: "case011Self", spec: 11, bearer: true, fn: case011Self},
	{group: "api", name: "case011Envelope", spec: 11, bearer: true, fn: case011Envelope},
	{group: "api", name: "case011RequestIdOnEveryResponse", spec: 11, bearer: true, fn: case011RequestIdOnEveryResponse},
	{group: "api", name: "case011ReadOnlyInFileMode", spec: 11, mode: fileMode, fn: case011ReadOnlyInFileMode},
	{group: "api", name: "case011GrammarPerKind", spec: 11, bearer: true, mode: serverMode, fn: case011GrammarPerKind},
	{group: "api", name: "case011ApplyWithoutTheEnvelope", spec: 11, bearer: true, mode: serverMode, fn: case011ApplyWithoutTheEnvelope},
	{group: "api", name: "case011BodiesAndTypes", spec: 11, bearer: true, mode: serverMode, fn: case011BodiesAndTypes},
	{group: "api", name: "case011ApplyIsCreateThenUpdate", spec: 11, bearer: true, mode: serverMode, fn: case011ApplyIsCreateThenUpdate},
	{group: "api", name: "case011Preconditions", spec: 11, bearer: true, mode: serverMode, fn: case011Preconditions},
	{group: "api", name: "case011AddressByIdOrName", spec: 11, bearer: true, mode: serverMode, fn: case011AddressByIdOrName},
	{group: "api", name: "case011ModelNamesWithSlashes", spec: 11, bearer: true, mode: serverMode, fn: case011ModelNamesWithSlashes},
	{group: "api", name: "case011Rotate", spec: 11, bearer: true, mode: serverMode, fn: case011Rotate},
	{group: "api", name: "case011BudgetInUse", spec: 11, bearer: true, mode: serverMode, fn: case011BudgetInUse},
	{group: "api", name: "case011Pagination", spec: 11, bearer: true, mode: serverMode, fn: case011Pagination},
	{group: "api", name: "case011RateLimitHeaders", spec: 11, bearer: true, mode: serverMode, fn: case011RateLimitHeaders},
	{group: "api", name: "case011SecretsNeverInResponses", spec: 11, bearer: true, mode: serverMode, fn: case011SecretsNeverInResponses},
	{group: "api", name: "case011OpenAPIValidatesEveryResponse", spec: 11, fn: case011OpenAPIValidatesEveryResponse},
}

// phantomProvider is a Provider spec the server is never asked to dial:
// a host under example.com with discovery and health off.
func phantomProvider(host string) map[string]any {
	return map[string]any{
		"dialect": "openai", "baseURL": "https://" + host + ".example.com/v1",
		"discovery": map[string]any{"mode": "none"}, "health": map[string]any{"mode": "none"},
	}
}

// budgetSpec is a small Budget.
func budgetSpec(amount string) map[string]any {
	return map[string]any{"amount": amount, "currency": "USD", "window": "month"}
}

// etagOf asserts the ETag is the quoted status.version and returns the
// version.
func etagOf(t testing.TB, resp *response) int64 {
	t.Helper()
	body := resp.json(t)
	version := int64(num(body, "status.version"))
	if want := `"` + strconv.FormatInt(version, 10) + `"`; resp.Header.Get("ETag") != want || version <= 0 {
		t.Errorf("ETag %q, status.version %d", resp.Header.Get("ETag"), version)
	}
	return version
}

// case011WellKnown holds GET /.well-known/lux to spec 011: the name, the
// build, the apiVersion the manifests carry, the API and OpenAPI URLs,
// one door per dialect listed, and the mode.
func case011WellKnown(t testing.TB, c *client) {
	w := c.well
	if w.Name != "lux" || w.Version == "" {
		t.Errorf("name %q version %q", w.Name, w.Version)
	}
	if w.APIVersion != v1.APIVersion {
		t.Errorf("apiVersion %q, want %s", w.APIVersion, v1.APIVersion)
	}
	if !strings.HasSuffix(w.API, "/v1") || w.OpenAPI != w.API+"/openapi.json" {
		t.Errorf("api %q openapi %q", w.API, w.OpenAPI)
	}
	if len(w.Dialects) == 0 || len(w.Dialects) != len(w.Doors) {
		t.Errorf("dialects %v doors %v", w.Dialects, w.Doors)
	}
	base := strings.TrimSuffix(w.API, "/v1")
	for _, d := range w.Dialects {
		if w.Doors[d] != base+"/"+d {
			t.Errorf("the %s door is %q, want %s", d, w.Doors[d], base+"/"+d)
		}
	}
	if w.Mode == "server" && (len(w.Issuers) == 0 || w.Audience == "") {
		t.Errorf("server mode names issuers %v and audience %q", w.Issuers, w.Audience)
	}
	if w.Mode == "file" && (len(w.Issuers) != 0 || w.Audience != "") {
		t.Errorf("file mode names issuers %v and audience %q", w.Issuers, w.Audience)
	}
}

// case011NoCORS: OPTIONS is not_found and no answer carries an
// Access-Control header.
func case011NoCORS(t testing.TB, c *client) {
	if c.api == "" {
		c.skip(t, "the server is in file mode, where /v1 is on the internal listener, and "+EnvInternalURL+" is unset")
	}
	resp := c.v1(t, http.MethodOptions, "/keys", nil, bearer(""))
	c.expect(t, resp, "not_found")
	for name := range resp.Header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("OPTIONS answered with %s", name)
		}
	}
	known := c.request(t, http.MethodGet, c.cfg.URL+"/.well-known/lux", nil)
	if known.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("/.well-known/lux answers with Access-Control-Allow-Origin")
	}
}

// case011ListReads: every kind lists as {"items": [...]} with the
// items an array, the read half a file mode server serves too.
func case011ListReads(t testing.TB, c *client) {
	for _, kind := range kindOrder {
		resp := c.list(t, kind, "")
		if resp.Status != http.StatusOK {
			t.Errorf("GET /%s: %d %s", plurals[kind], resp.Status, excerpt(resp.Body))
			continue
		}
		if _, ok := resp.json(t)["items"].([]any); !ok {
			t.Errorf("GET /%s: items is not an array: %s", plurals[kind], excerpt(resp.Body))
		}
	}
}

// case011Self: GET /v1/self reports the caller's subject and the
// policy, and file mode reports the policy alone.
func case011Self(t testing.TB, c *client) {
	resp := c.v1(t, http.MethodGet, "/self", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /self: %d %s", resp.Status, excerpt(resp.Body))
	}
	self := resp.json(t)
	policy := str(self, "policy")
	if !slices.Contains([]string{"authorizer", "owner", "file"}, policy) {
		t.Errorf("policy %q", policy)
	}
	if c.fileMode() {
		if policy != "file" || str(self, "subject") != "" {
			t.Errorf("file mode self %s", excerpt(resp.Body))
		}
		return
	}
	if c.cfg.Subject != "" && str(self, "subject") != c.cfg.Subject {
		t.Errorf("subject %q, want %q", str(self, "subject"), c.cfg.Subject)
	}
	if str(self, "issuer") == "" || str(self, "sub") == "" || str(self, "subject") != str(self, "issuer")+"|"+str(self, "sub") {
		t.Errorf("issuer %q sub %q subject %q", str(self, "issuer"), str(self, "sub"), str(self, "subject"))
	}
}

// case011Envelope: a refusal is one envelope with the code, one
// sentence, request_id equal to Lux-Request-Id, paths for a code that
// names fields and none otherwise, and the developer detail apart.
func case011Envelope(t testing.TB, c *client) {
	e := c.expect(t, c.read(t, v1.KindBudget, c.name("nobody")), "not_found")
	if len(e.paths) != 0 {
		t.Errorf("not_found carries paths %v", e.paths)
	}
	if c.fileMode() {
		return
	}
	resp := c.list(t, v1.KindBudget, "limit=0")
	e = c.expect(t, resp, "invalid_field")
	if !slices.Equal(e.paths, []string{"limit"}) || e.detail == "" {
		t.Errorf("invalid_field paths %v detail %q", e.paths, e.detail)
	}
	if strings.Contains(e.message, e.detail) || strings.Contains(e.message, "limit") {
		t.Errorf("the user sentence %q carries the developer detail", e.message)
	}
	// A method the table does not list is not_found in the envelope, PATCH
	// on every kind's item route among them.
	for _, kind := range kindOrder {
		c.expect(t, c.v1(t, http.MethodPatch, "/"+plurals[kind]+"/"+c.name("nobody"), map[string]any{"spec": map[string]any{}}), "not_found")
	}
}

// case011RequestIdOnEveryResponse: Lux-Request-Id is on every answer,
// which the client asserts on every request, a caller's own is replaced,
// and X-Request-Id of printable ASCII is echoed while one over 128
// bytes is not.
func case011RequestIdOnEveryResponse(t testing.TB, c *client) {
	resp := c.v1(t, http.MethodGet, "/self", nil, header("Lux-Request-Id", "conf-mine"), header("X-Request-Id", "conf-"+c.run))
	if resp.ID == "conf-mine" || !strings.HasPrefix(resp.ID, v1.PrefixRequest) {
		t.Errorf("Lux-Request-Id %q after a caller sent its own", resp.ID)
	}
	if resp.Header.Get("X-Request-Id") != "conf-"+c.run {
		t.Errorf("X-Request-Id %q was not echoed", resp.Header.Get("X-Request-Id"))
	}
	long := strings.Repeat("x", 129)
	if resp := c.v1(t, http.MethodGet, "/self", nil, header("X-Request-Id", long)); resp.Header.Get("X-Request-Id") != "" {
		t.Error("a 129-byte X-Request-Id was echoed")
	}
	for _, d := range c.well.Dialects {
		resp := c.door(t, d, http.MethodGet, "/v1/models", nil, "", header("Lux-Request-Id", "conf-mine"))
		if resp.ID == "conf-mine" || !strings.HasPrefix(resp.ID, v1.PrefixRequest) {
			t.Errorf("the %s door answered Lux-Request-Id %q", d, resp.ID)
		}
	}
}

// case011ReadOnlyInFileMode: a write under /v1/{kind}s on a file mode
// server is read_only with Allow: GET, before the body is read.
func case011ReadOnlyInFileMode(t testing.TB, c *client) {
	resp := c.apply(t, v1.KindBudget, c.name("ro"), map[string]any{"spec": budgetSpec("1")})
	c.expect(t, resp, "read_only")
	if resp.Header.Get("Allow") != http.MethodGet {
		t.Errorf("Allow %q", resp.Header.Get("Allow"))
	}
	c.expect(t, c.del(t, v1.KindBudget, c.name("ro")), "read_only")
}

// case011GrammarPerKind: the four routes of each kind, one object per
// kind, applied, read, listed, and deleted, with the ETag the version
// and the owner the caller.
func case011GrammarPerKind(t testing.TB, c *client) {
	specs := map[string]map[string]any{
		v1.KindProvider: phantomProvider("grammar." + c.run),
		v1.KindBudget:   budgetSpec("1"),
		v1.KindModel:    {"targets": []map[string]any{{"provider": c.name("grammar")}}},
		v1.KindKey:      {"models": []string{c.name("grammar")}},
	}
	ids := map[string]string{}
	for _, kind := range kindOrder {
		name := c.name("grammar")
		resp := c.apply(t, kind, name, map[string]any{"metadata": c.meta(name), "spec": specs[kind]})
		if resp.Status != http.StatusCreated {
			t.Fatalf("PUT /%s/%s: %d %s", plurals[kind], name, resp.Status, excerpt(resp.Body))
		}
		etagOf(t, resp)
		body := resp.json(t)
		ids[kind] = str(body, "status.id")
		if c.cfg.Subject != "" && str(body, "status.owner") != c.cfg.Subject {
			t.Errorf("%s owner %q, want %q", kind, str(body, "status.owner"), c.cfg.Subject)
		}
		read := c.read(t, kind, name)
		if read.Status != http.StatusOK {
			t.Fatalf("GET /%s/%s: %d %s", plurals[kind], name, read.Status, excerpt(read.Body))
		}
		etagOf(t, read)
		if kind == v1.KindKey {
			// The Key's selector is the Model's exact name, and status
			// records what it matched at resolve.
			selectors := arr(read.json(t), "status.selectors")
			if len(selectors) != 1 || str(selectors[0], "selector") != c.name("grammar") || canonical(t, field(selectors[0], "matched")) != `["`+c.name("grammar")+`"]` {
				t.Errorf("status.selectors %s", canonical(t, selectors))
			}
		}
		found := false
		for _, it := range c.items(t, c.list(t, kind, "label="+c.label)) {
			if str(it, "status.id") == ids[kind] {
				found = true
			}
		}
		if !found {
			t.Errorf("GET /%s?label=%s does not list %s", plurals[kind], c.label, ids[kind])
		}
	}
	for _, kind := range slices.Backward(kindOrder) {
		if resp := c.del(t, kind, ids[kind]); resp.Status != http.StatusNoContent || len(resp.Body) != 0 {
			t.Errorf("DELETE /%s/%s: %d %s", plurals[kind], ids[kind], resp.Status, excerpt(resp.Body))
		}
		c.expect(t, c.read(t, kind, ids[kind]), "not_found")
	}
}

// case011ApplyIsCreateThenUpdate: PUT twice with one manifest is 201
// then 200, the second answer the first but for the version and the
// update time.
func case011ApplyIsCreateThenUpdate(t testing.TB, c *client) {
	name := c.name("twice")
	body := map[string]any{"metadata": c.meta(name), "spec": budgetSpec("2")}
	first := c.apply(t, v1.KindBudget, name, body)
	if first.Status != http.StatusCreated {
		t.Fatalf("first PUT: %d %s", first.Status, excerpt(first.Body))
	}
	defer c.mustDelete(t, v1.KindBudget, name)
	second := c.apply(t, v1.KindBudget, name, body)
	if second.Status != http.StatusOK {
		t.Fatalf("second PUT: %d %s", second.Status, excerpt(second.Body))
	}
	if etagOf(t, first) != 1 || etagOf(t, second) != 2 {
		t.Errorf("versions %s then %s", first.Header.Get("ETag"), second.Header.Get("ETag"))
	}
	a, b := first.json(t), second.json(t)
	for _, m := range []map[string]any{obj(a, "status"), obj(b, "status")} {
		delete(m, "version")
		delete(m, "updatedAt")
	}
	if canonical(t, a) != canonical(t, b) {
		t.Errorf("the second answer differs beyond the version and updatedAt:\n%s\n%s", canonical(t, a), canonical(t, b))
	}
	// An update that changes a field the contract marks immutable is
	// immutable_field naming the field.
	changed := map[string]any{"metadata": c.meta(name), "spec": map[string]any{"amount": "2", "currency": "EUR", "window": "month"}}
	e := c.expect(t, c.apply(t, v1.KindBudget, name, changed), "immutable_field")
	if !slices.Equal(e.paths, []string{"spec.currency"}) {
		t.Errorf("immutable_field names %v", e.paths)
	}
}

// case011ApplyWithoutTheEnvelope: a body of spec alone on a kind's route
// is a whole manifest, a body whose kind disagrees with the route is
// unsupported_kind, and a body whose name disagrees with the path is
// invalid_field at metadata.name while one without a name takes the
// path's.
func case011ApplyWithoutTheEnvelope(t testing.TB, c *client) {
	name := c.name("bare")
	resp := c.apply(t, v1.KindKey, name, map[string]any{"spec": map[string]any{"models": []string{c.name("*")}}})
	if resp.Status != http.StatusCreated {
		t.Fatalf("a body of spec alone: %d %s", resp.Status, excerpt(resp.Body))
	}
	defer c.mustDelete(t, v1.KindKey, name)
	created := resp.json(t)
	if str(created, "apiVersion") != v1.APIVersion || str(created, "kind") != v1.KindKey || str(created, "metadata.name") != name {
		t.Errorf("the answer's envelope: %s", excerpt(resp.Body))
	}
	c.expect(t, c.apply(t, v1.KindKey, name, map[string]any{"kind": "Model", "spec": map[string]any{"models": []string{"x"}}}), "unsupported_kind")
	c.expect(t, c.apply(t, v1.KindKey, name, map[string]any{"apiVersion": "lux.latere.ai/v2", "spec": map[string]any{"models": []string{"x"}}}), "unsupported_version")
	e := c.expect(t, c.apply(t, v1.KindKey, name, map[string]any{"metadata": map[string]any{"name": c.name("other")}, "spec": map[string]any{"models": []string{"x"}}}), "invalid_field")
	if !slices.Equal(e.paths, []string{"metadata.name"}) {
		t.Errorf("a name that disagrees with the path names %v", e.paths)
	}
}

// case011BodiesAndTypes: a YAML body and a JSON body of one manifest
// produce equal objects, a content type outside the two is
// unsupported_media_type, and a body above every LUX_MAX_MANIFEST_BYTES
// a server may set is body_too_large.
func case011BodiesAndTypes(t testing.TB, c *client) {
	yamlName, jsonName := c.name("as-yaml"), c.name("as-json")
	yamlBody := "metadata:\n  name: " + yamlName + "\n  labels:\n    conformance: " + c.run + "\nspec:\n  amount: \"7\"\n  currency: USD\n  window: month\n"
	fromYAML := c.apply(t, v1.KindBudget, yamlName, yamlBody, header("Content-Type", "application/yaml"))
	if fromYAML.Status != http.StatusCreated {
		t.Fatalf("the YAML body: %d %s", fromYAML.Status, excerpt(fromYAML.Body))
	}
	defer c.mustDelete(t, v1.KindBudget, yamlName)
	fromJSON := c.mustApply(t, v1.KindBudget, jsonName, map[string]any{"metadata": c.meta(jsonName), "spec": budgetSpec("7")})
	defer c.mustDelete(t, v1.KindBudget, jsonName)
	if canonical(t, fromYAML.json(t)["spec"]) != canonical(t, fromJSON["spec"]) {
		t.Errorf("YAML resolved to %s and JSON to %s", canonical(t, fromYAML.json(t)["spec"]), canonical(t, fromJSON["spec"]))
	}
	c.expect(t, c.apply(t, v1.KindBudget, c.name("as-text"), "amount: 1", header("Content-Type", "text/plain")), "unsupported_media_type")
	// A valid manifest padded past the largest limit a server may set,
	// refused on its Content-Length before a byte is read.
	huge := `{"spec": {"amount": "1"}` + strings.Repeat(" ", 16<<20) + `}`
	c.expect(t, c.apply(t, v1.KindBudget, c.name("huge"), huge), "body_too_large")
}

// case011Preconditions: If-Match at the version updates, stale is
// conflict, If-None-Match: * on a taken name is already_exists, If-Match:
// * on a free name is not_found, and a weak validator is invalid_field
// at the header.
func case011Preconditions(t testing.TB, c *client) {
	name := c.name("pre")
	body := map[string]any{"metadata": c.meta(name), "spec": budgetSpec("3")}
	if resp := c.apply(t, v1.KindBudget, name, body); resp.Status != http.StatusCreated {
		t.Fatalf("PUT: %d %s", resp.Status, excerpt(resp.Body))
	}
	defer c.mustDelete(t, v1.KindBudget, name)
	if resp := c.apply(t, v1.KindBudget, name, body, header("If-Match", `"1"`)); resp.Status != http.StatusOK || resp.Header.Get("ETag") != `"2"` {
		t.Errorf("If-Match at the version: %d ETag %q", resp.Status, resp.Header.Get("ETag"))
	}
	c.expect(t, c.apply(t, v1.KindBudget, name, body, header("If-Match", `"1"`)), "conflict")
	c.expect(t, c.apply(t, v1.KindBudget, name, body, header("If-None-Match", "*")), "already_exists")
	free := c.name("pre-free")
	c.expect(t, c.apply(t, v1.KindBudget, free, map[string]any{"metadata": c.meta(free), "spec": budgetSpec("3")}, header("If-Match", "*")), "not_found")
	e := c.expect(t, c.apply(t, v1.KindBudget, name, body, header("If-Match", `W/"2"`)), "invalid_field")
	if !slices.Equal(e.paths, []string{"If-Match"}) {
		t.Errorf("a weak validator names %v", e.paths)
	}
	c.expect(t, c.del(t, v1.KindBudget, name, header("If-Match", `"1"`)), "conflict")
}

// case011AddressByIdOrName: an object reads the same by id and by name,
// an id of another kind is not_found, and an apply by id is
// invalid_field at metadata.name.
func case011AddressByIdOrName(t testing.TB, c *client) {
	name := c.name("addr")
	created := c.object(t, v1.KindBudget, "addr", budgetSpec("4"))
	id := str(created, "status.id")
	defer c.mustDelete(t, v1.KindBudget, id)
	byID, byName := c.read(t, v1.KindBudget, id), c.read(t, v1.KindBudget, name)
	if byID.Status != http.StatusOK || byName.Status != http.StatusOK {
		t.Fatalf("by id %d, by name %d", byID.Status, byName.Status)
	}
	if canonical(t, byID.json(t)) != canonical(t, byName.json(t)) {
		t.Errorf("the two reads differ:\n%s\n%s", byID.Body, byName.Body)
	}
	c.expect(t, c.read(t, v1.KindKey, id), "not_found")
	e := c.expect(t, c.apply(t, v1.KindBudget, id, map[string]any{"spec": budgetSpec("4")}), "invalid_field")
	if !slices.Equal(e.paths, []string{"metadata.name"}) {
		t.Errorf("an apply by id names %v", e.paths)
	}
}

// case011ModelNamesWithSlashes: a Model named with a slash is one
// object by its percent-encoded name, by its two-segment path, and by
// its id.
func case011ModelNamesWithSlashes(t testing.TB, c *client) {
	c.object(t, v1.KindProvider, "slash", phantomProvider("slash."+c.run))
	defer c.mustDelete(t, v1.KindProvider, c.name("slash"))
	name := c.name("slash") + "/gpt-5"
	resp := c.apply(t, v1.KindModel, name, map[string]any{"metadata": c.meta(name), "spec": map[string]any{"targets": []map[string]any{{"provider": c.name("slash")}}}})
	if resp.Status != http.StatusCreated {
		t.Fatalf("PUT the encoded name: %d %s", resp.Status, excerpt(resp.Body))
	}
	id := str(resp.json(t), "status.id")
	defer c.mustDelete(t, v1.KindModel, id)
	two := c.v1(t, http.MethodGet, "/models/"+name, nil)
	if two.Status != http.StatusOK || str(two.json(t), "status.id") != id {
		t.Errorf("the two-segment path: %d %s", two.Status, excerpt(two.Body))
	}
	if byID := c.read(t, v1.KindModel, id); byID.Status != http.StatusOK || str(byID.json(t), "metadata.name") != name {
		t.Errorf("by id: %d %s", byID.Status, excerpt(byID.Body))
	}
	if list := c.list(t, v1.KindModel, "provider="+c.name("slash")); len(c.items(t, list)) != 1 {
		t.Errorf("?provider= lists %s", excerpt(list.Body))
	}
}

// case011Rotate: a rotate answers a new status.value once and keeps the
// id, the name, and the spec.
func case011Rotate(t testing.TB, c *client) {
	created := c.object(t, v1.KindKey, "rot", map[string]any{"models": []string{c.name("*")}})
	id, first := str(created, "status.id"), str(created, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	resp := c.v1(t, http.MethodPost, "/keys/"+id+"/rotate", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("rotate: %d %s", resp.Status, excerpt(resp.Body))
	}
	rotated := resp.json(t)
	second := str(rotated, "status.value")
	if second == "" || second == first || !strings.HasPrefix(second, "lux_") {
		t.Errorf("rotate answered value %q after %q", second, first)
	}
	if str(rotated, "status.id") != id || str(rotated, "metadata.name") != c.name("rot") || canonical(t, rotated["spec"]) != canonical(t, created["spec"]) {
		t.Errorf("rotate changed the id, the name, or the spec: %s", excerpt(resp.Body))
	}
	if str(rotated, "status.prefix") != second[:12] {
		t.Errorf("prefix %q after rotate", str(rotated, "status.prefix"))
	}
	c.expect(t, c.v1(t, http.MethodPost, "/keys/"+id+"/rotate", nil, header("If-None-Match", "*")), "invalid_field")
}

// case011BudgetInUse: a Budget a Key names is budget_in_use until the
// Key is gone.
func case011BudgetInUse(t testing.TB, c *client) {
	b := c.object(t, v1.KindBudget, "inuse", budgetSpec("5"))
	k := c.object(t, v1.KindKey, "inuse", map[string]any{"models": []string{c.name("*")}, "budget": c.name("inuse")})
	if str(k, "status.budget.id") != str(b, "status.id") || str(k, "status.budget.name") != c.name("inuse") {
		t.Errorf("status.budget %v", k["status"])
	}
	c.expect(t, c.del(t, v1.KindBudget, c.name("inuse")), "budget_in_use")
	c.mustDelete(t, v1.KindKey, str(k, "status.id"))
	if resp := c.del(t, v1.KindBudget, c.name("inuse")); resp.Status != http.StatusNoContent {
		t.Errorf("DELETE after the Key: %d %s", resp.Status, excerpt(resp.Body))
	}
}

// case011Pagination: a list pages through items and next_cursor, a label
// filter is a conjunction, a limit above the cap and an unknown parameter
// are invalid_field at their names, and a cursor of another kind is
// invalid_field at cursor.
func case011Pagination(t testing.TB, c *client) {
	page := "page=" + c.run
	for i := range 3 {
		name := c.name("page-" + itoa(i))
		meta := c.meta(name)
		obj(meta, "labels")["page"] = c.run
		c.mustApply(t, v1.KindBudget, name, map[string]any{"metadata": meta, "spec": budgetSpec("6")})
		defer c.mustDelete(t, v1.KindBudget, name)
	}
	first := c.list(t, v1.KindBudget, "label="+c.label+"&label="+page+"&limit=2")
	items := c.items(t, first)
	cursor := str(first.json(t), "next_cursor")
	if len(items) != 2 || cursor == "" {
		t.Fatalf("the first page has %d items and cursor %q: %s", len(items), cursor, excerpt(first.Body))
	}
	second := c.list(t, v1.KindBudget, "label="+c.label+"&label="+page+"&limit=2&cursor="+cursor)
	if rest := c.items(t, second); len(rest) != 1 || str(second.json(t), "next_cursor") != "" {
		t.Errorf("the second page: %s", excerpt(second.Body))
	}
	if none := c.items(t, c.list(t, v1.KindBudget, "label="+c.label+"&label=page=nobody")); len(none) != 0 {
		t.Errorf("a label the objects lack lists %d items", len(none))
	}
	e := c.expect(t, c.list(t, v1.KindBudget, "limit=201"), "invalid_field")
	if !slices.Equal(e.paths, []string{"limit"}) {
		t.Errorf("limit=201 names %v", e.paths)
	}
	e = c.expect(t, c.list(t, v1.KindBudget, "bogus=1"), "invalid_field")
	if !slices.Equal(e.paths, []string{"bogus"}) {
		t.Errorf("an unknown parameter names %v", e.paths)
	}
	e = c.expect(t, c.list(t, v1.KindKey, "cursor="+cursor), "invalid_field")
	if !slices.Equal(e.paths, []string{"cursor"}) {
		t.Errorf("a cursor of another kind names %v", e.paths)
	}
}

// case011RateLimitHeaders: an authenticated /v1 answer carries the three
// RateLimit headers as integers, or none when the server sets no rate.
func case011RateLimitHeaders(t testing.TB, c *client) {
	resp := c.v1(t, http.MethodGet, "/self", nil)
	limit := resp.Header.Get("RateLimit-Limit")
	if limit == "" {
		c.skip(t, "the server sets no control plane rate, so no answer carries the RateLimit headers")
	}
	for _, h := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"} {
		if _, err := strconv.Atoi(resp.Header.Get(h)); err != nil {
			t.Errorf("%s %q is not an integer", h, resp.Header.Get(h))
		}
	}
	if none := c.v1(t, http.MethodGet, "/self", nil, bearer("")); none.Header.Get("RateLimit-Limit") != "" {
		t.Error("an unauthenticated refusal carries RateLimit-Limit")
	}
}

// keyValue matches a minted Key value wherever it appears.
var keyValue = regexp.MustCompile(`lux_[A-Za-z0-9_-]{40}`)

// case011SecretsNeverInResponses: a Key's value is in the create answer
// and in no read or list, a Provider's credential is in no answer, and
// the OpenAPI document carries no example of a value.
func case011SecretsNeverInResponses(t testing.TB, c *client) {
	canary := "sk-canary-" + c.run + "-never-in-any-response"
	spec := phantomProvider("secret." + c.run)
	spec["credential"] = map[string]any{"value": canary}
	created := c.object(t, v1.KindProvider, "secret", spec)
	defer c.mustDelete(t, v1.KindProvider, str(created, "status.id"))
	if canonical(t, created) != strings.ReplaceAll(canonical(t, created), canary, "") {
		t.Error("the create answer carries the Provider's credential")
	}
	if num(created, "status.credential.version") != 1 || field(created, "status.credential.set") != true {
		t.Errorf("status.credential %v", field(created, "status.credential"))
	}
	if read := c.read(t, v1.KindProvider, str(created, "status.id")); strings.Contains(string(read.Body), canary) {
		t.Error("a read carries the Provider's credential")
	}
	// An update without a value keeps the stored credential; one with a
	// value replaces it and bumps the version.
	kept := c.mustApply(t, v1.KindProvider, c.name("secret"), map[string]any{"metadata": c.meta(c.name("secret")), "spec": phantomProvider("secret." + c.run)})
	if num(kept, "status.credential.version") != 1 || field(kept, "status.credential.set") != true {
		t.Errorf("after an update without a value, status.credential %v", field(kept, "status.credential"))
	}
	spec["credential"] = map[string]any{"value": canary + "-2"}
	replaced := c.mustApply(t, v1.KindProvider, c.name("secret"), map[string]any{"metadata": c.meta(c.name("secret")), "spec": spec})
	if num(replaced, "status.credential.version") != 2 || strings.Contains(canonical(t, replaced), canary) {
		t.Errorf("after an update with a value, status.credential %v", field(replaced, "status.credential"))
	}
	key := c.object(t, v1.KindKey, "secret", map[string]any{"models": []string{c.name("*")}})
	id, value := str(key, "status.id"), str(key, "status.value")
	defer c.mustDelete(t, v1.KindKey, id)
	if value == "" {
		t.Fatal("the create answer carries no status.value")
	}
	read := c.read(t, v1.KindKey, id)
	if strings.Contains(string(read.Body), value) || field(read.json(t), "status.value") != nil {
		t.Errorf("a read carries the Key's value: %s", excerpt(read.Body))
	}
	if list := c.list(t, v1.KindKey, "label="+c.label); strings.Contains(string(list.Body), value) || keyValue.Match(list.Body) {
		t.Error("a list carries a Key value")
	}
	doc := c.v1(t, http.MethodGet, "/openapi.json", nil, bearer(""))
	if keyValue.Match(doc.Body) || strings.Contains(string(doc.Body), canary) {
		t.Error("the OpenAPI document carries a Key value or the credential")
	}
}

// case011OpenAPIValidatesEveryResponse: GET /v1/openapi.json is the
// document every /v1 answer of the run is held to, which the client does
// on every request; the case asserts the document names every route of
// spec 011's table and that the holding ran.
func case011OpenAPIValidatesEveryResponse(t testing.TB, c *client) {
	if c.doc == nil {
		c.skip(t, "the server is in file mode, where /v1 is on the internal listener, and "+EnvInternalURL+" is unset")
	}
	for _, p := range []string{"/v1/providers", "/v1/providers/{name}", "/v1/models", "/v1/models/{name}", "/v1/keys", "/v1/keys/{name}", "/v1/keys/{name}/rotate", "/v1/budgets", "/v1/budgets/{name}", "/v1/usage", "/v1/requests", "/v1/self", "/v1/openapi.json", "/.well-known/lux"} {
		if c.doc.paths[p] == nil {
			t.Errorf("the document names no %s", p)
		}
	}
	for _, s := range []string{"Provider", "Model", "Key", "Budget", "Error"} {
		if c.doc.schemas[s] == nil {
			t.Errorf("the document has no %s schema", s)
		}
	}
	c.mu.Lock()
	n := c.validated
	c.mu.Unlock()
	if n == 0 {
		t.Error("no /v1 answer was held to the document")
	}
	known := c.request(t, http.MethodGet, c.cfg.URL+"/.well-known/lux", nil)
	for _, p := range c.doc.validate(http.MethodGet, "/.well-known/lux", known.Status, known.Header.Get("Content-Type"), known.Body) {
		t.Errorf("/.well-known/lux: %s", p)
	}
}
