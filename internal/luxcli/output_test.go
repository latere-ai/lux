// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

// The fixtures a server would answer, one per kind, with every member a
// column reads.
const (
	providerJSON = `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider","metadata":{"name":"openai"},"spec":{"dialect":"openai","tunnel":false,"baseURL":"https://api.example.com/v1","credential":{"header":"Authorization","scheme":"bearer"},"concurrency":0},"status":{"id":"prv_01","version":2,"owner":"https://login.example.com|alice","credential":{"set":true,"version":3},"health":{"state":"Healthy"},"discovered":{"count":12},"createdAt":"2026-09-13T12:00:00Z","warnings":null}}` + "\n"
	tunnelJSON   = `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider","metadata":{"name":"my-laptop"},"spec":{"dialect":"openai","tunnel":true,"concurrency":0},"status":{"id":"prv_02","owner":"https://login.example.com|alice","credential":{"set":false,"version":0},"tunnel":{"state":"Connected","session":"tun_01"},"createdAt":"2026-09-14T11:59:30Z","warnings":null}}` + "\n"
	modelJSON    = `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Model","metadata":{"name":"gpt-5"},"spec":{"targets":[{"provider":"openai","model":"gpt-5","weight":100,"priority":0}],"fallback":"onError","pricing":{"currency":"USD","per":1000000,"input":"1","output":"2"},"modalities":{"input":["text","image"],"output":["text"]},"contextWindow":200000},"status":{"id":"mdl_01","owner":"https://login.example.com|alice","source":"declared","available":true,"createdAt":"2026-09-14T10:00:00Z","warnings":null}}` + "\n"
	keyJSON      = `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Key","metadata":{"name":"run-42"},"spec":{"models":["gpt-5","anthropic/*"],"limits":{"requestsPerMinute":120,"tokensPerMinute":200000,"spend":{"amount":"10","currency":"USD","window":"24h"}},"budget":"team","allowUnpriced":false,"passthrough":false,"disabled":false},"status":{"id":"key_01","owner":"https://login.example.com|alice","prefix":"lux_9d4e","state":"Active","budget":{"name":"team","id":"bud_01"},"expiresAt":"2026-10-14T12:00:00Z","createdAt":"2026-09-14T11:55:00Z","warnings":null}}` + "\n"
	budgetJSON   = `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Budget","metadata":{"name":"team"},"spec":{"amount":"50","currency":"USD","window":"month","hard":true},"status":{"id":"bud_01","owner":"https://login.example.com|alice","state":"Open","spent":"12.5","remaining":"37.5","resetsAt":"2026-10-01T00:00:00Z","keys":3,"createdAt":"2026-09-01T00:00:00Z","warnings":null}}` + "\n"
)

// pagedKeys answers GET /v1/keys in two pages, each item with odd
// spacing so a re-encoding would show.
func pagedKeys() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[{"kind":"Key","metadata":{"name":"a"}, "spec": {"models":["x"]}},{"kind":"Key","metadata":{"name":"b"}}],"next_cursor":"c2"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"kind":"Key",  "metadata":{"name":"c"}}]}` + "\n"))
	}
}

// TestOutputFidelity is spec 014's row: -o json for one object is
// byte-identical to the response; a two-page list is one envelope with
// every item's bytes unchanged and next_cursor empty; -o yaml
// round-trips to the same value.
func TestOutputFidelity(t *testing.T) {
	f := newFake(t)
	odd := `{ "kind" : "Key",   "z": 1, "a": [1,  2], "b": {"y": null, "x": "true"} }` // no newline, odd spacing
	f.on("GET", "/v1/keys/x", 200, odd)
	r := run(t, f.env(nil), "get", "key", "x")
	if r.code != 0 || r.stdout != odd {
		t.Fatalf("json: exit %d stdout %q", r.code, r.stdout)
	}
	f.answers["GET /v1/keys"] = pagedKeys()
	r = run(t, f.env(nil), "list", "keys")
	want := `{"items":[{"kind":"Key","metadata":{"name":"a"}, "spec": {"models":["x"]}},{"kind":"Key","metadata":{"name":"b"}},{"kind":"Key",  "metadata":{"name":"c"}}]}` + "\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("two pages: exit %d\nstdout %q\nwant   %q", r.code, r.stdout, want)
	}
	for _, body := range []string{odd, providerJSON, modelJSON, keyJSON, budgetJSON, want} {
		f.on("GET", "/v1/keys/x", 200, body)
		r = run(t, f.env(nil), "get", "key", "x", "-o", "yaml")
		if r.code != 0 {
			t.Fatalf("yaml: exit %d stderr %q", r.code, r.stderr)
		}
		var back any
		if err := yaml.Unmarshal([]byte(r.stdout), &back); err != nil {
			t.Fatalf("yaml does not parse: %v\n%s", err, r.stdout)
		}
		if !reflect.DeepEqual(normalize(t, back), normalizeJSON(t, body)) {
			t.Fatalf("yaml round trip differs:\n%s\nfrom %s", r.stdout, body)
		}
		// Field order is the server's.
		if strings.HasPrefix(body, `{"apiVersion"`) && !strings.HasPrefix(r.stdout, "apiVersion: lux.latere.ai/v1beta1\nkind: ") {
			t.Fatalf("yaml lost the order:\n%s", r.stdout)
		}
	}
}

// normalize renders any value through JSON and back, so a YAML integer
// and a JSON number compare equal.
func normalize(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return normalizeJSON(t, string(data))
}

func normalizeJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestColumns is spec 014's row: every column in the table renders for
// each kind, a tunneled Provider shows an empty BASEURL and its tunnel
// state.
func TestColumns(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		body         string
		heads, wide  string
		row, wideRow []string
	}{
		{providerJSON, "NAME DIALECT BASEURL HEALTH MODELS AGE OWNER", "ID CREDENTIAL TUNNEL",
			[]string{"openai", "openai", "https://api.example.com/v1", "Healthy", "12", "1d", "https://login.example.com|alice"}, []string{"prv_01", "set v3", ""}},
		{tunnelJSON, "NAME DIALECT BASEURL HEALTH MODELS AGE OWNER", "ID CREDENTIAL TUNNEL",
			[]string{"my-laptop", "openai", "", "Unknown", "-", "30s", "https://login.example.com|alice"}, []string{"prv_02", "unset", "Connected"}},
		{modelJSON, "NAME SOURCE TARGETS AVAILABLE PRICED AGE OWNER", "ID CONTEXT MODALITIES",
			[]string{"gpt-5", "declared", "openai/gpt-5", "true", "yes", "2h", "https://login.example.com|alice"}, []string{"mdl_01", "200000", "text,image/text"}},
		{keyJSON, "NAME PREFIX STATE MODELS BUDGET EXPIRES OWNER", "ID RPM TPM SPEND",
			[]string{"run-42", "lux_9d4e", "Active", "gpt-5,anthropic/*", "team", "30d", "https://login.example.com|alice"}, []string{"key_01", "120", "200000", "10/24h"}},
		{budgetJSON, "NAME STATE AMOUNT SPENT REMAINING RESETS OWNER", "ID WINDOW HARD KEYS",
			[]string{"team", "Open", "50 USD", "12.5", "37.5", "16d", "https://login.example.com|alice"}, []string{"bud_01", "month", "true", "3"}},
	}
	for _, tc := range cases {
		f.on("GET", "/v1/keys/x", 200, tc.body)
		for _, mode := range []string{"table", "wide"} {
			r := run(t, f.env(nil), "get", "key", "x", "-o", mode)
			if r.code != 0 {
				t.Fatalf("%s: exit %d stderr %q", mode, r.code, r.stderr)
			}
			lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("%s: %d lines:\n%s", mode, len(lines), r.stdout)
			}
			heads, row := tc.heads, tc.row
			if mode == "wide" {
				heads += " " + tc.wide
				row = append(append([]string{}, tc.row...), tc.wideRow...)
			}
			if got := strings.Join(strings.Fields(lines[0]), " "); got != heads {
				t.Fatalf("%s heads %q, want %q", mode, got, heads)
			}
			cells := cellsUnder(lines[0], lines[1])
			if !reflect.DeepEqual(cells, row) {
				t.Fatalf("%s row %q, want %q\n%s", mode, cells, row, r.stdout)
			}
		}
	}
	// A list of one kind renders one row per item, and an empty list one
	// header.
	f.on("GET", "/v1/keys", 200, `{"items":[`+strings.TrimSpace(keyJSON)+`,`+strings.TrimSpace(keyJSON)+`]}`)
	r := run(t, f.env(nil), "list", "keys", "-o", "table")
	if lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n"); r.code != 0 || len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("list table: exit %d\n%s", r.code, r.stdout)
	}
	f.on("GET", "/v1/keys", 200, `{"items":[]}`)
	if r := run(t, f.env(nil), "list", "keys", "-o", "table"); r.code != 0 || r.stdout != "\n" {
		t.Fatalf("empty list: exit %d stdout %q", r.code, r.stdout)
	}
}

// cellsUnder reads a row's cells by the header's column starts.
func cellsUnder(header, row string) []string {
	var starts []int
	inWord := false
	for i, c := range header {
		if c != ' ' && !inWord {
			starts = append(starts, i)
		}
		inWord = c != ' '
	}
	cells := make([]string, len(starts))
	for i, s := range starts {
		end := len(row)
		if i+1 < len(starts) && starts[i+1] < len(row) {
			end = starts[i+1]
		}
		if s >= len(row) {
			continue
		}
		cells[i] = strings.TrimSpace(row[s:end])
	}
	return cells
}

// TestNoColumnCarriesASecret is spec 014's row: no column in any mode
// carries a credential value, a Key's value included, which appears
// below the table alone.
func TestNoColumnCarriesASecret(t *testing.T) {
	f := newFake(t)
	poisoned := strings.Replace(strings.Replace(keyJSON, `"budget":"team",`, `"budget":"team","value":"`+canaryCred+`",`, 1), `"prefix":"lux_9d4e",`, `"prefix":"lux_9d4e","value":"`+canaryKey+`",`, 1)
	provider := strings.Replace(providerJSON, `"credential":{"header":"Authorization","scheme":"bearer"}`, `"credential":{"value":"`+canaryCred+`","header":"Authorization","scheme":"bearer"}`, 1)
	for _, body := range []string{poisoned, provider, `{"items":[` + strings.TrimSpace(poisoned) + `]}`, `{"items":[` + strings.TrimSpace(provider) + `]}`} {
		f.on("GET", "/v1/keys/x", 200, body)
		for _, mode := range []string{"table", "wide"} {
			r := run(t, f.env(nil), "get", "key", "x", "-o", mode)
			if r.code != 0 {
				t.Fatalf("exit %d stderr %q", r.code, r.stderr)
			}
			table, _, _ := strings.Cut(r.stdout, "key: ")
			if strings.Contains(table, canaryCred) || strings.Contains(table, canaryKey) {
				t.Fatalf("%s carries a secret:\n%s", mode, r.stdout)
			}
		}
	}
	// The Key's value is the one line after the table, once, and only for
	// one object.
	f.on("PUT", "/v1/keys/run-42", 201, poisoned)
	r := run(t, f.env(nil), "apply", "-f", write(t, "k.yaml", keyYAML), "-o", "table")
	if r.code != 0 || strings.Count(r.stdout, canaryKey) != 1 || !strings.Contains(r.stdout, "\nkey: "+canaryKey+"\nThe value is shown once; store it now.\n") {
		t.Fatalf("create under table:\n%s", r.stdout)
	}
}

func TestGenericTables(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/v1/self", 200, `{"subject":"https://login.example.com|alice","claims":{"email":"alice@example.com"},"policy":"owner","limits":{"requests_per_minute":1200}}`)
	r := run(t, f.env(nil), "whoami", "-o", "table")
	want := "subject: https://login.example.com|alice\nclaims: {\"email\":\"alice@example.com\"}\npolicy: owner\nlimits: {\"requests_per_minute\":1200}\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("whoami table:\n%s\nwant\n%s", r.stdout, want)
	}
	f.on("GET", "/v1/usage", 200, `{"items":[{"model":"gpt-5","requests":3,"cost":1250000,"priced":true,"labels":{"team":"a"}},{"model":"sonnet","requests":1,"cost":0,"priced":false,"labels":{}}]}`)
	r = run(t, f.env(nil), "usage", "-o", "table")
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 3 || strings.Join(strings.Fields(lines[0]), " ") != "MODEL REQUESTS COST PRICED" || strings.Join(strings.Fields(lines[1]), " ") != "gpt-5 3 1250000 true" {
		t.Fatalf("usage table:\n%s", r.stdout)
	}
	f.on("GET", "/lux/v1/models", 200, `{"object":"list","data":[{"id":"gpt-5","object":"model","created":0,"owned_by":"lux"}]}`)
	r = run(t, f.env(map[string]string{"LUX_TOKEN": "", "LUX_KEY": "lux_x"}), "models", "-o", "wide")
	lines = strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "ID OBJECT CREATED OWNED_BY" || strings.Join(strings.Fields(lines[1]), " ") != "gpt-5 model 0 lux" {
		t.Fatalf("door models table:\n%s", r.stdout)
	}
	// A body that is a bare scalar or a list of scalars still renders.
	f.on("GET", "/v1/self", 200, `"just a string"`)
	if r := run(t, f.env(nil), "whoami", "-o", "table"); r.stdout != "just a string\n" {
		t.Fatalf("scalar: %q", r.stdout)
	}
	f.on("GET", "/v1/self", 200, `{"items":[1,2]}`)
	if r := run(t, f.env(nil), "whoami", "-o", "table"); r.stdout != "\n" {
		t.Fatalf("scalars as items: %q", r.stdout)
	}
}

func TestHumanizeAndWhen(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: "5s", 90 * time.Second: "1m", 3 * time.Hour: "3h", 49 * time.Hour: "2d"} {
		if got := humanize(d); got != want {
			t.Errorf("humanize(%s) = %q, want %q", d, got, want)
		}
	}
	if _, ok := when("not a time"); ok {
		t.Error("a non-time parsed")
	}
	if _, ok := when(json.Number("1")); ok {
		t.Error("a number parsed as a time")
	}
	a := &app{o: Options{Now: func() time.Time { return now }}}
	o := newObject()
	status := newObject()
	status.set("expiresAt", "2026-09-14T11:00:00Z")
	status.set("resetsAt", "2026-09-14T11:00:00Z")
	o.set("status", status)
	if expires(a, o) != "expired" || resets(a, o) != "now" {
		t.Errorf("past instants: %q %q", expires(a, o), resets(a, o))
	}
	if age(a, o) != "-" || resets(a, newObject()) != "-" {
		t.Error("an absent instant is a dash")
	}
	if got := yamlValue(json.Number("18446744073709551615")); got != uint64(18446744073709551615) {
		t.Errorf("a large number became %v", got)
	}
	if got := yamlValue(json.Number("1.5")); got != 1.5 {
		t.Errorf("a fraction became %v", got)
	}
	if _, err := decodeJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Error("two values decoded")
	}
	if _, err := decodeJSON([]byte(`{"a":`)); err == nil {
		t.Error("a truncated object decoded")
	}
	if _, err := decodeJSON([]byte(`[1,`)); err == nil {
		t.Error("a truncated array decoded")
	}
	o.del("nothing")
	o.del("status")
	if len(o.keys) != 0 {
		t.Error("del left a key")
	}
}
