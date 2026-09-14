// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// specsDir is the deck, found from this file.
func specsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "specs")
}

// exempt are the three groups spec 011 names as codes no response
// carries: the record's client_closed, the tunnel's close reasons, and
// the lux command's exit codes, which its spec spells in words.
var exempt = map[string]bool{"client_closed": true, "superseded": true, "token_expired": true, "provider_deleted": true, "draining": true}

var (
	codeRow    = regexp.MustCompile("^\\| `([a-z_]+)` \\|")
	refusedAs  = regexp.MustCompile("(?:refused with|answers|answered) `([a-z_]+)`")
	tableRow   = regexp.MustCompile("^\\| `([a-z_]+)` \\| ([0-9]{3}) \\| ([^|]+) \\|")
	identifier = regexp.MustCompile(`[A-Za-z]+\.[A-Za-z]+\(|internal/|\.go\b|Err[A-Z]|LUX_[A-Z]`)
)

// TestErrorTable reads every spec in the deck, collects every code named
// in an error table, the Code column of 003's and 004's tables, and
// every backticked snake_case word a spec says a request is refused
// with or answers, subtracts the three exempt groups, and asserts each
// has exactly one row here with a status and a sentence; that a code in
// 004's table has the same status there; that spec 011's own table is
// this table row for row; that no two rows share a sentence and every
// sentence is one sentence in the user register; and that no handler on
// either plane, the doors', the manifest's, or the identity's, writes a
// message outside the table.
func TestErrorTable(t *testing.T) {
	dir := specsDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no specs at %s: %v", dir, err)
	}
	named := map[string][]string{} // code to the specs naming it
	statusIn004 := map[string]int{}
	var own []struct {
		code    string
		status  int
		message string
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(file)
		inTable := false
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "| Code |") {
				inTable = true
				continue
			}
			if inTable && !strings.HasPrefix(line, "|") {
				inTable = false
			}
			if inTable && (strings.HasPrefix(base, "003-") || strings.HasPrefix(base, "004-")) {
				if m := codeRow.FindStringSubmatch(line); m != nil {
					named[m[1]] = append(named[m[1]], base)
					if strings.HasPrefix(base, "004-") {
						if r := tableRow.FindStringSubmatch(line); r != nil {
							statusIn004[m[1]], _ = strconv.Atoi(r[2])
						}
					}
				}
			}
			if inTable && strings.HasPrefix(base, "011-") {
				if r := tableRow.FindStringSubmatch(line); r != nil {
					status, _ := strconv.Atoi(r[2])
					own = append(own, struct {
						code    string
						status  int
						message string
					}{r[1], status, strings.TrimSpace(r[3])})
				}
			}
			if strings.HasPrefix(base, "011-") {
				continue
			}
			for _, m := range refusedAs.FindAllStringSubmatch(line, -1) {
				named[m[1]] = append(named[m[1]], base)
			}
		}
	}
	if len(named) < 20 {
		t.Fatalf("the deck names %d codes, which is too few to be the whole deck", len(named))
	}
	for code, specs := range named {
		if exempt[code] {
			continue
		}
		if _, ok := table[Code(code)]; !ok {
			t.Errorf("%s, named by %v, has no row here", code, slices.Compact(specs))
		}
	}
	for code, status := range statusIn004 {
		if Code(code).Status() != status {
			t.Errorf("%s is %d in spec 004 and %d here", code, status, Code(code).Status())
		}
	}
	if len(own) != len(table) {
		t.Fatalf("spec 011's table has %d rows, the code %d", len(own), len(table))
	}
	for i, row := range own {
		got, ok := table[Code(row.code)]
		if !ok || got.Status != row.status || got.Message != row.message || codes[i] != Code(row.code) {
			t.Errorf("spec 011 row %s %d %q; the code has %v at position %d", row.code, row.status, row.message, got, i)
		}
	}
	seen := map[string]Code{}
	for _, c := range Codes() {
		row := table[c]
		if row.Message == "" || !strings.HasSuffix(row.Message, ".") || strings.Contains(row.Message, ". ") || strings.Contains(row.Message, "`") {
			t.Errorf("%s: %q is not one sentence", c, row.Message)
		}
		if identifier.MatchString(row.Message) {
			t.Errorf("%s: %q names an identifier on a user surface", c, row.Message)
		}
		if other, dup := seen[row.Message]; dup {
			t.Errorf("%s and %s share the sentence %q", c, other, row.Message)
		}
		seen[row.Message] = c
	}
	if Code("bogus").Status() != http.StatusInternalServerError || Code("bogus").Message() != "" {
		t.Error("a string that is not a code has a status or a sentence")
	}
	// The other planes' and packages' tables are this one.
	for _, c := range gateway.Codes() {
		if Code(c).Status() != c.Status() || Code(c).Message() != c.Message() {
			t.Errorf("the doors write %s as %d %q, this table %d %q", c, c.Status(), c.Message(), Code(c).Status(), Code(c).Message())
		}
	}
	for _, c := range manifest.Codes() {
		if Code(c).Message() != c.Message() {
			t.Errorf("the manifest writes %s as %q, this table %q", c, c.Message(), Code(c).Message())
		}
	}
	for _, c := range auth.Codes() {
		if Code(c).Message() != c.Message() {
			t.Errorf("the identity writes %s as %q, this table %q", c, c.Message(), Code(c).Message())
		}
	}
}

// TestMapError: every error a handler meets maps to one code, the store
// errors by name, ErrHashTaken naming no Key, an unknown error and a
// Lookup's pass-through as store_unavailable, and an Error as itself.
func TestMapError(t *testing.T) {
	for _, tc := range []struct {
		err   error
		code  Code
		paths []string
	}{
		{&Error{Code: CodeConflict, Detail: "x"}, CodeConflict, nil},
		{&manifest.Error{Code: manifest.CodeInvalidField, Paths: []string{"spec.x"}, Detail: "d"}, CodeInvalidField, []string{"spec.x"}},
		{&auth.Error{Code: auth.CodeForbidden, Detail: "no"}, CodeForbidden, nil},
		{store.ErrNotFound, CodeNotFound, nil},
		{store.ErrVersionConflict, CodeConflict, nil},
		{store.ErrNameTaken, CodeAlreadyExists, nil},
		{store.ErrHashTaken, CodeInvalidField, []string{"spec.value"}},
		{store.ErrInvalidCursor, CodeInvalidField, []string{"cursor"}},
		{store.ErrReadOnly, CodeReadOnly, nil},
		{errors.New("dial tcp: connection refused"), CodeStoreUnavailable, nil},
		{errors.Join(errors.New("manifest: spec.budget: the Lookup failed"), errors.New("timeout")), CodeStoreUnavailable, nil},
	} {
		got := mapError(tc.err)
		if got.Code != tc.code || !reflect.DeepEqual(got.Paths, tc.paths) {
			t.Errorf("mapError(%v) = %v, want %s at %v", tc.err, got, tc.code, tc.paths)
		}
	}
	if e := mapError(store.ErrHashTaken); e.Detail != "a Key with this value already exists" {
		t.Errorf("ErrHashTaken detail %q", e.Detail)
	}
	if (&Error{Code: CodeInvalidField, Paths: []string{"a", "b"}, Detail: "d"}).Error() != "invalid_field at a, b: d" || (&Error{Code: CodeInternal}).Error() != "internal" {
		t.Error("Error.Error renders wrongly")
	}
}

// TestEnvelope: the envelope carries request_id always, paths for every
// code that names a field, detail where there is one and not otherwise,
// Lux-Error beside it, and the fixed sentence; an internal carries an
// empty detail.
func TestEnvelope(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodPut, "/v1/budgets/team", `{"spec": {"amount": "-1"}}`)
	d := wantCode(t, rec, CodeInvalidField)
	if !reflect.DeepEqual(paths(d), []string{"spec.amount"}) || d["detail"] == nil || rec.Header().Get("Lux-Error") != "invalid_field" {
		t.Errorf("details %v, headers %v", d, rec.Header())
	}
	rec = h.request(http.MethodGet, "/v1/budgets/nothing", "")
	d = wantCode(t, rec, CodeNotFound)
	if _, has := d["paths"]; has {
		t.Errorf("not_found carries paths: %v", d)
	}
	if len(d) != 2 {
		t.Errorf("details carry more than request_id and detail: %v", d)
	}
	e := body(t, rec)["error"].(map[string]any)
	if len(e) != 3 || e["message"] != CodeNotFound.Message() {
		t.Errorf("error %v", e)
	}
}

// TestMalformedBody: a body that is not JSON, and one that is not YAML
// under a YAML type, is malformed_body with the line and column in
// details.detail; a body of two documents is multi_document.
func TestMalformedBody(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodPut, "/v1/budgets/team", "{\n  \"spec\": {\"amount\": }\n}")
	d := wantCode(t, rec, CodeMalformedBody)
	if detail, _ := d["detail"].(string); !regexp.MustCompile(`line 2|2:`).MatchString(detail) {
		t.Errorf("JSON detail %q names no line", detail)
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", "spec:\n  amount: [oops\n", "Content-Type", "application/yaml")
	d = wantCode(t, rec, CodeMalformedBody)
	if detail, _ := d["detail"].(string); !regexp.MustCompile(`\[2:|line 2`).MatchString(detail) {
		t.Errorf("YAML detail %q names no line", detail)
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", "spec:\n  amount: \"1\"\n---\nspec: {}\n", "Content-Type", "application/yaml")
	wantCode(t, rec, CodeMultiDocument)
	rec = h.request(http.MethodPut, "/v1/budgets/team", "")
	wantCode(t, rec, CodeUnsupportedMediaType)
}

// TestBodiesAndTypes: a body one byte over LUX_MAX_MANIFEST_BYTES is
// body_too_large, before a byte is read when Content-Length says so; an
// unsupported content type is unsupported_media_type, before the body is
// read; a YAML and a JSON body of one manifest produce equal responses;
// the four YAML and JSON types are accepted.
func TestBodiesAndTypes(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxManifestBytes = 256 })
	pad := strings.Repeat(" ", 256-len(budgetJSON)+1)
	rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON+pad)
	if d := wantCode(t, rec, CodeBodyTooLarge); !strings.Contains(d["detail"].(string), "257") {
		t.Errorf("detail %v", d["detail"])
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON+pad[1:])
	if rec.Code != http.StatusCreated {
		t.Fatalf("exactly the limit: %d %s", rec.Code, rec.Body.String())
	}
	// A chunked body with no Content-Length crosses the limit while it
	// streams.
	r := httptest.NewRequest(http.MethodPut, "/v1/budgets/team", strings.NewReader(budgetJSON+pad))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+h.alice)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if d := wantCode(t, w, CodeBodyTooLarge); !strings.Contains(d["detail"].(string), "crossed") {
		t.Errorf("streamed detail %v", d["detail"])
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "Content-Type", "text/plain")
	wantCode(t, rec, CodeUnsupportedMediaType)
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "Content-Type", "application/json; charset=utf-8")
	if rec.Code != http.StatusOK {
		t.Errorf("a JSON type with a parameter: %d %s", rec.Code, rec.Body.String())
	}
	yamlBody := "spec:\n  amount: \"50\"\n  currency: USD\n  window: month\n"
	var responses []string
	for _, ct := range []string{"application/yaml", "application/x-yaml", "text/yaml"} {
		rec := h.request(http.MethodPut, "/v1/budgets/team", yamlBody, "Content-Type", ct)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", ct, rec.Code, rec.Body.String())
		}
		responses = append(responses, rec.Body.String())
	}
	rec = h.request(http.MethodPut, "/v1/budgets/team", budgetJSON)
	responses = append(responses, rec.Body.String())
	strip := regexp.MustCompile(`"version":[0-9]+,|"updatedAt":"[^"]+",`)
	for i := 1; i < len(responses); i++ {
		if strip.ReplaceAllString(responses[i], "") != strip.ReplaceAllString(responses[0], "") {
			t.Errorf("response %d differs from the YAML one:\n%s\n%s", i, responses[i], responses[0])
		}
	}
	if rec := h.request(http.MethodGet, "/v1/budgets/team", ""); rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("response Content-Type %q", rec.Header().Get("Content-Type"))
	}
}

// TestRequestIdOnEveryResponse: every response, served or refused,
// carries Lux-Request-Id, req_ and a ULID; a caller's own is replaced;
// the id in the envelope is the header's; X-Content-Type-Options is set.
func TestRequestIdOnEveryResponse(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.NewID = nil })
	ulid := regexp.MustCompile(`^req_[0-9A-HJKMNP-TV-Z]{26}$`)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/.well-known/lux", ""}, {"GET", "/v1/openapi.json", ""}, {"GET", "/v1/self", ""},
		{"PUT", "/v1/budgets/team", budgetJSON}, {"GET", "/v1/budgets/team", ""}, {"GET", "/v1/budgets", ""},
		{"GET", "/v1/budgets/none", ""}, {"PATCH", "/v1/budgets/team", ""}, {"GET", "/v1/usage", ""},
	} {
		rec := h.request(tc.method, tc.path, tc.body, "Lux-Request-Id", "req_CALLERCHOSEN00000000000000")
		id := rec.Header().Get("Lux-Request-Id")
		if !ulid.MatchString(id) || id == "req_CALLERCHOSEN00000000000000" {
			t.Errorf("%s %s: Lux-Request-Id %q", tc.method, tc.path, id)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: no nosniff", tc.method, tc.path)
		}
	}
	rec := h.request(http.MethodGet, "/v1/budgets/none", "", "Authorization", "")
	if d := wantCode(t, rec, CodeUnauthenticated); d["request_id"] != rec.Header().Get("Lux-Request-Id") {
		t.Error("the envelope's request_id is not the header's")
	}
}

// TestXRequestIdIsEchoedOnly: a caller's X-Request-Id of 128 printable
// bytes is echoed unchanged and appears in no event or log line; one of
// 129 bytes or with a control byte is not echoed.
func TestXRequestIdIsEchoedOnly(t *testing.T) {
	h := newHarness(t, nil)
	ok := strings.Repeat("x", 128)
	rec := h.request(http.MethodPut, "/v1/budgets/team", budgetJSON, "X-Request-Id", ok)
	if rec.Header().Get("X-Request-Id") != ok {
		t.Errorf("echo %q", rec.Header().Get("X-Request-Id"))
	}
	rows, _ := h.st.Journal().Since(bg(), 0, 100)
	for _, r := range rows {
		if strings.Contains(string(r.Payload), ok) {
			t.Error("an event carries the caller's X-Request-Id")
		}
	}
	if strings.Contains(h.log.String(), ok) {
		t.Error("a log line carries the caller's X-Request-Id")
	}
	for _, bad := range []string{strings.Repeat("x", 129), "abc\x01def", "abc\x7fdef", "über"} {
		rec := h.request(http.MethodGet, "/v1/budgets/team", "", "X-Request-Id", bad)
		if _, present := rec.Header()["X-Request-Id"]; present {
			t.Errorf("%q was echoed", bad)
		}
	}
	rec = h.request(http.MethodGet, "/v1/budgets/team", "", "X-Request-Id", "")
	if _, present := rec.Header()["X-Request-Id"]; present {
		t.Error("an absent X-Request-Id was echoed")
	}
}

// failing is a store whose every read and write fails, or panics.
type failing struct {
	store.Store
	panics bool
}

var errDown = errors.New("store: dial tcp 10.0.0.12:5432: i/o timeout")

func (f *failing) Objects() store.Objects { return &failingObjects{f.Store.Objects(), f.panics} }
func (f *failing) Transact(context.Context, func(tx store.Store) error) error {
	return errDown
}

type failingObjects struct {
	store.Objects
	panics bool
}

func (o *failingObjects) ByName(context.Context, string, string) (v1.Object, int64, error) {
	if o.panics {
		panic("a bug in the handler")
	}
	return nil, 0, errDown
}

func (o *failingObjects) Get(context.Context, string, string) (v1.Object, int64, error) {
	return nil, 0, errDown
}

func (o *failingObjects) List(context.Context, string, store.Filter, store.Page) ([]v1.Object, string, error) {
	return nil, "", errDown
}

// TestStoreErrorsMap: a store error that is not one of the named errors
// is store_unavailable on a read, a list, an apply, and through the
// Lookup of Resolve, with the store's line in the detail; a handler
// panic is internal with an empty detail and one log line, and the
// refusal counts in lux_refusals_total.
func TestStoreErrorsMap(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	working := h.h.o.Store
	h.h.o.Store = &failing{Store: working}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/budgets/team", ""}, {"GET", "/v1/budgets", ""}, {"PUT", "/v1/budgets/team", budgetJSON}, {"DELETE", "/v1/budgets/team", ""},
	} {
		rec := h.request(tc.method, tc.path, tc.body)
		if d := wantCode(t, rec, CodeStoreUnavailable); !strings.Contains(d["detail"].(string), "i/o timeout") {
			t.Errorf("%s %s: detail %v", tc.method, tc.path, d["detail"])
		}
	}
	// The Lookup's pass-through: the existing read works, the reference
	// fails.
	h.h.o.Store = &lookupFailing{Store: working}
	rec := h.request(http.MethodPut, "/v1/keys/run-42", keyJSON)
	if d := wantCode(t, rec, CodeStoreUnavailable); !strings.Contains(d["detail"].(string), "the Lookup failed") {
		t.Errorf("Lookup pass-through detail %v", d["detail"])
	}
	h.h.o.Store = &failing{Store: working, panics: true}
	rec = h.request(http.MethodGet, "/v1/budgets/team", "")
	d := wantCode(t, rec, CodeInternal)
	if _, has := d["detail"]; has {
		t.Errorf("internal carries a detail: %v", d)
	}
	if !strings.Contains(h.log.String(), "handler panic") || !strings.Contains(h.log.String(), "a bug in the handler") {
		t.Errorf("no log line for the panic:\n%s", h.log.String())
	}
	if n := h.reg.Counter(MetricRefusals, "").Value(map[string]string{"code": "internal"}); n != 1 {
		t.Errorf("lux_refusals_total{code=internal} = %d", n)
	}
	if n := h.reg.Counter(MetricRefusals, "").Value(map[string]string{"code": "store_unavailable"}); n != 5 {
		t.Errorf("lux_refusals_total{code=store_unavailable} = %d", n)
	}
}

// lookupFailing is a store whose Get fails, which is what a Key's
// reference to a Budget by id or a selector's list of Models reads.
type lookupFailing struct{ store.Store }

func (f *lookupFailing) Objects() store.Objects { return &lookupFailingObjects{f.Store.Objects()} }

type lookupFailingObjects struct{ store.Objects }

func (o *lookupFailingObjects) List(context.Context, string, store.Filter, store.Page) ([]v1.Object, string, error) {
	return nil, "", errDown
}
