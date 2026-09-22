// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
)

// runTimeout bounds one Run: spec 018's five minutes, held by the
// context every request carries rather than hoped for.
const runTimeout = 5 * time.Minute

// The kinds' path segments under /v1, spec 011's plurals.
var plurals = map[string]string{
	v1.KindProvider: "providers", v1.KindModel: "models", v1.KindKey: "keys", v1.KindBudget: "budgets",
}

// kindOrder is the order the accepted corpus applies in, spec 003's:
// a Model names a Provider, a Key names Models and a Budget.
var kindOrder = []string{v1.KindProvider, v1.KindBudget, v1.KindModel, v1.KindKey}

// statuses is the HTTP status of every code of spec 011's error table,
// the one table both planes write, so a case asserts the status beside
// the code without asking the server what it means.
var statuses = map[string]int{
	"malformed_body": 400, "multi_document": 400, "unsupported_version": 400, "unsupported_kind": 400,
	"unknown_field": 400, "missing_field": 400, "invalid_field": 400, "reserved_prefix": 400,
	"exclusive_fields": 400, "duplicate_target": 400, "invalid_request": 400, "upstream_rejected": 400,
	"dialect_unsupported": 400, "provider_required": 400, "currency_mismatch": 400,
	"unauthenticated": 401, "forbidden": 403, "key_disabled": 403, "key_expired": 403,
	"route_not_allowed": 403, "model_not_allowed": 403, "model_unpriced": 403,
	"not_found": 404, "model_not_found": 404, "read_only": 405,
	"already_exists": 409, "conflict": 409, "immutable_field": 409, "budget_in_use": 409, "provider_in_use": 409,
	"body_too_large": 413, "unsupported_media_type": 415, "ceiling_exceeded": 422,
	"rate_limited": 429, "spend_exceeded": 429, "budget_exhausted": 429,
	"internal": 500, "upstream_error": 502, "authorizer_unavailable": 503, "store_unavailable": 503,
	"provider_unavailable": 503, "upstream_timeout": 504,
}

// wellKnown is GET /.well-known/lux as spec 011 shapes it, the one
// document Run configures itself from.
type wellKnown struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	APIVersion string            `json:"apiVersion"`
	API        string            `json:"api"`
	OpenAPI    string            `json:"openapi"`
	Doors      map[string]string `json:"doors"`
	Dialects   []string          `json:"dialects"`
	Issuers    []string          `json:"issuers"`
	Audience   string            `json:"audience"`
	Mode       string            `json:"mode"`
}

// suiteKey is the Key the suite mints for the doors and keys groups.
type suiteKey struct {
	id, name, value, prefix string
}

// client is one run against one server: the configuration, the run's
// id and label, what the well-known document said, the OpenAPI document
// every /v1 answer is held to, the stubs when there are any, the Key
// once minted, and what the cases have learnt so far.
type client struct {
	cfg   Config
	http  *http.Client
	ctx   context.Context
	run   string
	label string
	token string // the default bearer, empty without one
	well  wellKnown
	api   string // the base of /v1, the document's api or the internal listener's in the file mode
	stubs *stubs
	key   *suiteKey
	doc   *document
	rep   *report
	// prepared is set once the Key and the fixtures were attempted, so
	// the keys group never mints a second Key.
	prepared bool
	// tolerated are the OpenAPI drifts the package's own tests accept
	// from the reference server, by finding, each with the spec that owes
	// the fix; Run tolerates none.
	tolerated map[string]string
	// fixtures holds the previous releases under testdata/previous/, the
	// embedded tree in every run but the package's own fixture test.
	fixtures fs.FS
	// patience caps every poll's timeout when above zero; the package's
	// own tests set it so a server that never answers right fails fast.
	patience time.Duration

	mu         sync.Mutex
	sentences  map[string]string // code to the one user sentence seen for it
	validated  int               // /v1 answers held to the document
	violations map[string]bool   // the drifts reported, one per case, route, and finding
	corpus     []applied         // the accepted corpus as applied, for the defaults case
	models     []string          // the suite's own Model names, the Key's view
	providers  []string          // the suite's own Provider names
}

// applied is one accepted corpus case as the server read it back.
type applied struct {
	name   string // the corpus path, accepted/<kind>/<case>.yaml
	kind   string
	golden map[string]any // the golden, renamed under this run
	read   map[string]any // the read-back
}

// name is the run's name for a suite object: conf-<run>-<suffix>.
func (c *client) name(suffix string) string { return "conf-" + c.run + "-" + suffix }

// meta is the metadata block every suite object carries: the name and
// the run label.
func (c *client) meta(name string) map[string]any {
	return map[string]any{"name": name, "labels": map[string]any{"conformance": c.run}}
}

// fileMode reports a server whose desired state is a directory.
func (c *client) fileMode() bool { return c.well.Mode == "file" }

// response is one answer read whole.
type response struct {
	Status int
	Header http.Header
	Body   []byte
	ID     string // Lux-Request-Id
}

// json decodes the body as one object; a body that is not one fails the
// case with the body shown.
func (r *response) json(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("%d: the body is not a JSON object: %v\n%s", r.Status, err, excerpt(r.Body))
	}
	return m
}

// excerpt bounds a body in a failure message.
func excerpt(b []byte) string {
	const limit = 2048
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}

// reqOpt edits one request before it is sent.
type reqOpt func(*http.Request)

// bearer sets the Authorization header; an empty token removes it.
func bearer(token string) reqOpt {
	return func(r *http.Request) {
		if token == "" {
			r.Header.Del("Authorization")
			return
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
}

// header sets one header.
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// encode renders a body: bytes and strings as they are, anything else
// as JSON.
func encode(t testing.TB, body any) []byte {
	t.Helper()
	switch b := body.(type) {
	case nil:
		return nil
	case []byte:
		return b
	case string:
		return []byte(b)
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}
	return data
}

// request sends one request and reads the answer whole. Every answer of
// the server under test carries Lux-Request-Id, which is asserted here
// for both planes, and a JSON answer under /v1 is held to the OpenAPI
// document once Run has fetched it.
func (c *client) request(t testing.TB, method, rawURL string, body any, opts ...reqOpt) *response {
	t.Helper()
	data := encode(t, body)
	var rd io.Reader
	if data != nil {
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, rawURL, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, rawURL, err)
	}
	out := &response{Status: resp.StatusCode, Header: resp.Header, Body: read, ID: resp.Header.Get(gateway.HeaderRequestID)}
	if strings.HasPrefix(rawURL, c.cfg.URL) || (c.api != "" && strings.HasPrefix(rawURL, c.api)) {
		if !strings.HasPrefix(out.ID, v1.PrefixRequest) {
			t.Errorf("%s %s: Lux-Request-Id %q does not begin with %s", method, rawURL, out.ID, v1.PrefixRequest)
		}
	}
	if c.api != "" && strings.HasPrefix(rawURL, c.api) {
		c.hold(t, method, rawURL, out)
	}
	return out
}

// hold validates one /v1 answer against the OpenAPI document and notes
// the one sentence each code carries.
func (c *client) hold(t testing.TB, method, rawURL string, resp *response) {
	t.Helper()
	if resp.Status >= 400 && strings.HasPrefix(strings.TrimSpace(string(resp.Body)), "{") {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(resp.Body, &env) == nil && env.Error.Code != "" {
			c.mu.Lock()
			if prev, ok := c.sentences[env.Error.Code]; ok && prev != env.Error.Message {
				t.Errorf("%s answers %q and answered %q before; one code has one sentence", env.Error.Code, env.Error.Message, prev)
			}
			c.sentences[env.Error.Code] = env.Error.Message
			c.mu.Unlock()
		}
	}
	if c.doc == nil {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	path := strings.TrimPrefix(u.EscapedPath(), strings.TrimSuffix(c.api, "/v1"))
	problems := c.doc.validate(method, path, resp.Status, resp.Header.Get("Content-Type"), resp.Body)
	c.mu.Lock()
	c.validated++
	// One drift is reported once per case, whatever the index of the
	// item it appeared in and however often the case polled. A drift the
	// package's own tests tolerate is logged with its owner and counted.
	var fresh, known []string
	for _, p := range problems {
		finding := indexless(p)
		if owner, ok := c.tolerated[finding]; ok {
			c.rep.drifts[finding] = true
			known = append(known, p+" (a known drift, owed by "+owner+")")
			continue
		}
		key := t.Name() + "|" + method + " " + path + "|" + finding
		if !c.violations[key] {
			c.violations[key] = true
			fresh = append(fresh, p)
		}
	}
	c.mu.Unlock()
	for _, p := range fresh {
		t.Errorf("%s %s answered %d with a body outside GET /v1/openapi.json: %s", method, path, resp.Status, p)
	}
	for _, p := range known {
		t.Logf("%s %s answered %d with a body outside GET /v1/openapi.json: %s", method, path, resp.Status, p)
	}
}

// indexless drops the list indexes from a validation path, so the same
// drift in every item of a list is one finding.
func indexless(p string) string {
	var b strings.Builder
	skip := false
	for _, r := range p {
		switch {
		case r == '[':
			skip = true
			b.WriteString("[]")
		case r == ']':
			skip = false
		case !skip:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// v1 is one control plane request: path is under /v1, the default bearer
// is sent unless an option removes or replaces it.
func (c *client) v1(t testing.TB, method, path string, body any, opts ...reqOpt) *response {
	t.Helper()
	all := make([]reqOpt, 0, len(opts)+1)
	if c.token != "" {
		all = append(all, bearer(c.token))
	}
	all = append(all, opts...)
	return c.request(t, method, c.api+path, body, all...)
}

// door is one data plane request through the door of a dialect, with
// the credential as the bearer when one is given.
func (c *client) door(t testing.TB, dialect, method, path string, body any, credential string, opts ...reqOpt) *response {
	t.Helper()
	base, ok := c.well.Doors[dialect]
	if !ok {
		t.Fatalf("the server serves no %s door", dialect)
	}
	all := make([]reqOpt, 0, len(opts)+1)
	if credential != "" {
		all = append(all, bearer(credential))
	}
	all = append(all, opts...)
	return c.request(t, method, base+path, body, all...)
}

// envelope is one /v1 refusal as spec 011 shapes it.
type envelope struct {
	code, message string
	details       map[string]any
	requestID     string
	paths         []string
	detail        string
}

// envelope reads a refusal: the code and its details, with request_id
// equal to the response's Lux-Request-Id, the code in Lux-Error, and the
// message one sentence.
func (c *client) envelope(t testing.TB, resp *response) envelope {
	t.Helper()
	m := resp.json(t)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("%d: no error envelope in %s", resp.Status, excerpt(resp.Body))
	}
	out := envelope{code: str(e, "code"), message: str(e, "message")}
	out.details, _ = e["details"].(map[string]any)
	out.requestID = str(out.details, "request_id")
	out.detail = str(out.details, "detail")
	for _, p := range arr(out.details, "paths") {
		if s, ok := p.(string); ok {
			out.paths = append(out.paths, s)
		}
	}
	if out.requestID != resp.ID {
		t.Errorf("details.request_id %q is not the response's Lux-Request-Id %q", out.requestID, resp.ID)
	}
	if got := resp.Header.Get(gateway.HeaderError); got != out.code {
		t.Errorf("Lux-Error %q is not the envelope's code %q", got, out.code)
	}
	if !oneSentence(out.message) {
		t.Errorf("%s: message %q is not one sentence", out.code, out.message)
	}
	return out
}

// oneSentence is the shape of a user sentence: ends with a full stop and
// carries no second one.
func oneSentence(s string) bool {
	return s != "" && strings.HasSuffix(s, ".") && !strings.Contains(s, ". ")
}

// expect asserts the answer is the code at the status spec 011's table
// gives it, and returns the envelope.
func (c *client) expect(t testing.TB, resp *response, code string) envelope {
	t.Helper()
	if want, known := statuses[code]; known && resp.Status != want {
		t.Errorf("status %d, want %d for %s\n%s", resp.Status, want, code, excerpt(resp.Body))
	}
	e := c.envelope(t, resp)
	if e.code != code {
		t.Errorf("code %q, want %q\n%s", e.code, code, excerpt(resp.Body))
	}
	return e
}

// doorCode reads the code off a door's refusal in the door's own shape
// and in Lux-Error, and holds the status to the table.
func (c *client) doorCode(t testing.TB, dialect string, resp *response) string {
	t.Helper()
	code := resp.Header.Get(gateway.HeaderError)
	if code == "" {
		t.Fatalf("%s door: no Lux-Error on a %d answer: %s", dialect, resp.Status, excerpt(resp.Body))
	}
	if want, known := statuses[code]; known && resp.Status != want {
		t.Errorf("%s door: status %d, want %d for %s", dialect, resp.Status, want, code)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s door: Content-Type %q on a refusal", dialect, ct)
	}
	m := resp.json(t)
	var inBody, message string
	switch dialect {
	case "openai":
		inBody, message = str(m, "error.code"), str(m, "error.message")
		if str(m, "error.type") != code {
			t.Errorf("openai shape: error.type %q, want %q", str(m, "error.type"), code)
		}
		if _, ok := obj(m, "error")["param"]; !ok {
			t.Error("openai shape: error.param is absent")
		}
	case "anthropic":
		inBody, message = str(m, "error.type"), str(m, "error.message")
		if str(m, "type") != "error" || str(m, "request_id") != resp.ID {
			t.Errorf("anthropic shape: type %q request_id %q, want error and %q", str(m, "type"), str(m, "request_id"), resp.ID)
		}
	case "gemini":
		message = str(m, "error.message")
		details := arr(m, "error.details")
		if len(details) > 0 {
			inBody = str(details[0], "reason")
			if str(details[0], "@type") != "type.googleapis.com/google.rpc.ErrorInfo" || str(details[0], "domain") != "lux" {
				t.Errorf("gemini shape: details[0] %v", details[0])
			}
		}
		if num(m, "error.code") != float64(resp.Status) || str(m, "error.status") == "" {
			t.Errorf("gemini shape: error.code %v error.status %q", m["error"], str(m, "error.status"))
		}
	default:
		inBody, message = str(m, "error.code"), str(m, "error.message")
		if str(m, "error.details.request_id") != resp.ID {
			t.Errorf("lux shape: details.request_id %q, want %q", str(m, "error.details.request_id"), resp.ID)
		}
	}
	if inBody != code {
		t.Errorf("%s door: the body carries %q and Lux-Error %q", dialect, inBody, code)
	}
	if !oneSentence(message) {
		t.Errorf("%s door: message %q is not one sentence", dialect, message)
	}
	return code
}

// expectDoor asserts a door's refusal is the code.
func (c *client) expectDoor(t testing.TB, dialect string, resp *response, code string) {
	t.Helper()
	if got := c.doorCode(t, dialect, resp); got != code {
		t.Errorf("%s door: code %q, want %q\n%s", dialect, got, code, excerpt(resp.Body))
	}
}

// apply is PUT /v1/{kind}s/{name}.
func (c *client) apply(t testing.TB, kind, name string, body any, opts ...reqOpt) *response {
	t.Helper()
	return c.v1(t, http.MethodPut, "/"+plurals[kind]+"/"+escapeName(name), body, opts...)
}

// escapeName percent-encodes a name for the path, a Model's slashes
// kept, since the route matches any number of segments.
func escapeName(name string) string {
	segments := strings.Split(name, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// mustApply applies and requires a create or an update, returning the
// object.
func (c *client) mustApply(t testing.TB, kind, name string, body any, opts ...reqOpt) map[string]any {
	t.Helper()
	resp := c.apply(t, kind, name, body, opts...)
	if resp.Status != http.StatusCreated && resp.Status != http.StatusOK {
		t.Fatalf("PUT /%s/%s: %d %s", plurals[kind], name, resp.Status, excerpt(resp.Body))
	}
	return resp.json(t)
}

// object applies a suite object of the kind with the run's metadata and
// the spec, and requires the create.
func (c *client) object(t testing.TB, kind, suffix string, spec map[string]any) map[string]any {
	t.Helper()
	name := c.name(suffix)
	return c.mustApply(t, kind, name, map[string]any{"metadata": c.meta(name), "spec": spec})
}

// read is GET /v1/{kind}s/{ref}.
func (c *client) read(t testing.TB, kind, ref string, opts ...reqOpt) *response {
	t.Helper()
	return c.v1(t, http.MethodGet, "/"+plurals[kind]+"/"+escapeName(ref), nil, opts...)
}

// del is DELETE /v1/{kind}s/{ref}.
func (c *client) del(t testing.TB, kind, ref string, opts ...reqOpt) *response {
	t.Helper()
	return c.v1(t, http.MethodDelete, "/"+plurals[kind]+"/"+escapeName(ref), nil, opts...)
}

// mustDelete deletes and requires the 204, or a 404 for an object a case
// already removed.
func (c *client) mustDelete(t testing.TB, kind, ref string) {
	t.Helper()
	resp := c.del(t, kind, ref)
	if resp.Status != http.StatusNoContent && resp.Status != http.StatusNotFound {
		t.Errorf("DELETE /%s/%s: %d %s", plurals[kind], ref, resp.Status, excerpt(resp.Body))
	}
}

// list is GET /v1/{kind}s with a query.
func (c *client) list(t testing.TB, kind, query string, opts ...reqOpt) *response {
	t.Helper()
	path := "/" + plurals[kind]
	if query != "" {
		path += "?" + query
	}
	return c.v1(t, http.MethodGet, path, nil, opts...)
}

// items reads a list answer's items.
func (c *client) items(t testing.TB, resp *response) []map[string]any {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("list: %d %s", resp.Status, excerpt(resp.Body))
	}
	var out []map[string]any
	for _, it := range arr(resp.json(t), "items") {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// eventually polls until check reports done or the deadline passes,
// failing with the last reason.
func (c *client) eventually(t testing.TB, timeout time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	if c.patience > 0 && c.patience < timeout {
		timeout = c.patience
	}
	deadline := time.Now().Add(timeout)
	wait := 50 * time.Millisecond
	var why string
	for {
		var done bool
		done, why = check()
		if done {
			return
		}
		if time.Now().After(deadline) || c.ctx.Err() != nil {
			break
		}
		time.Sleep(wait)
		if wait < time.Second {
			wait *= 2
		}
	}
	t.Fatalf("%s did not hold within %s: %s", what, timeout, why)
}

// field walks a dotted path through decoded JSON.
func field(m any, path string) any {
	cur := m
	if path == "" {
		return cur
	}
	for part := range strings.SplitSeq(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = obj[part]
		if !ok {
			return nil
		}
	}
	return cur
}

// str is the string at a path, "" when there is none.
func str(m any, path string) string {
	s, _ := field(m, path).(string)
	return s
}

// num is the number at a path, 0 when there is none.
func num(m any, path string) float64 {
	n, _ := field(m, path).(float64)
	return n
}

// arr is the list at a path, nil when there is none.
func arr(m any, path string) []any {
	a, _ := field(m, path).([]any)
	return a
}

// obj is the object at a path, nil when there is none.
func obj(m any, path string) map[string]any {
	o, _ := field(m, path).(map[string]any)
	return o
}

// canonical renders decoded JSON with sorted keys, so two trees compare
// as bytes.
func canonical(t testing.TB, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return string(data)
}

// decodeJSON decodes one JSON text into v.
func decodeJSON(text string, v any) error { return json.Unmarshal([]byte(text), v) }

// itoa is strconv.Itoa under a shorter name.
func itoa(n int) string { return strconv.Itoa(n) }

// sprintf is fmt.Sprintf under a shorter name.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// basePath is the prefix the server answers under, read from the API
// base the discovery document gave: its path less the trailing /v1, ""
// for a server at the root.
func (c *client) basePath() string {
	u, err := url.Parse(c.api)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(u.Path, "/v1")
}
