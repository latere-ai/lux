// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lux/internal/check"
	stubsink "latere.ai/x/lux/test/stubs/sink"
)

// TestCheckCommand is spec 017's check role at the process: luxd check
// prints one line per requirement on stdout in the fixed order and exits
// 0 when every line is ok or warn, exits 1 with the failing line named
// when one is not, and a bad flag is a usage error. The local issuer row
// of spec 035 is absent with LUX_LOCAL_ISSUER_KEY unset, ok with the
// key's algorithm and key id when it is set, fail when it does not parse,
// and never warn.
func TestCheckCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"check", "-no-such-flag"}, env(nil), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "-no-such-flag") {
		t.Fatalf("a bad flag: exit %d, stderr %q", code, errOut.String())
	}
	errOut.Reset()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	vars := serveEnv(t, map[string]string{"LUX_OIDC_ISSUERS": iss.URL(), "LUX_OIDC_AUDIENCE": "lux,api.example.com"})
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(vars), &out, &errOut); code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "; audience lux, api.example.com\n") {
		t.Errorf("the issuers line does not report the whole audience list:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "the public listener answers under the root") {
		t.Errorf("the public url line does not say where the listener is mounted:\n%s", out.String())
	}
	based := maps.Clone(vars)
	based["LUX_BASE_PATH"], based["LUX_PUBLIC_URL"] = "/v1/models", "https://api.example.com/v1/models"
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(based), &out, &errOut); code != 0 {
		t.Fatalf("exit %d under a base path:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "https://api.example.com/v1/models is absolute, the public listener answers under base path /v1/models") {
		t.Errorf("the public url line does not report the base path:\n%s", out.String())
	}
	inOrder := func(out string, names []string) {
		t.Helper()
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != len(names) {
			t.Fatalf("%d lines, want %d:\n%s", len(lines), len(names), out)
		}
		for i, l := range lines {
			if !strings.Contains(l, " "+names[i]+": ") {
				t.Errorf("line %d is %q, want the %s requirement", i+1, l, names[i])
			}
		}
	}
	unset := slices.DeleteFunc(slices.Clone(check.Names), func(n string) bool { return n == "local issuer" || n == "bootstrap" })
	inOrder(out.String(), unset)

	withIssuerKey := slices.DeleteFunc(slices.Clone(check.Names), func(n string) bool { return n == "bootstrap" })
	key, kid := issuerKey(t)
	local := maps.Clone(vars)
	local["LUX_LOCAL_ISSUER_KEY"] = key
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(local), &out, &errOut); code != 0 {
		t.Fatalf("exit %d with a local issuer key:\n%s", code, out.String())
	}
	inOrder(out.String(), withIssuerKey)
	if want := "\nok   local issuer: ES256 key " + kid + " signs tokens issued as " + publicURL + ", and 0 further key(s) of LUX_LOCAL_ISSUER_KEYS verify\n"; !strings.Contains(out.String(), want) {
		t.Errorf("no ok local issuer line naming the algorithm and key id:\n%s", out.String())
	}
	local["LUX_LOCAL_ISSUER_KEY"] = "sk-not-a-key"
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(local), &out, &errOut); code != 1 {
		t.Fatalf("exit %d with an unusable key:\n%s", code, out.String())
	}
	inOrder(out.String(), withIssuerKey)
	if !strings.Contains(out.String(), "\nfail local issuer: LUX_LOCAL_ISSUER_KEY is not a PEM encoded private key") || strings.Contains(out.String(), "warn local issuer") {
		t.Errorf("no failing local issuer line:\n%s", out.String())
	}

	// The bootstrap row (spec 035): the dry run of the apply, with the
	// counts when the directory resolves and the first file when it does
	// not; nothing is written either way.
	booted := maps.Clone(vars)
	booted["LUX_ADMIN_SUBJECTS"] = iss.URL() + "|alice"
	booted["LUX_TEST_PROVIDER_KEY"] = "sk-bootstrap"
	booted["LUX_BOOTSTRAP_DIR"] = writeDir(t, map[string]string{
		"openai.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: openai\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n  credential:\n    valueFrom:\n      env: LUX_TEST_PROVIDER_KEY\n",
	})
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(booted), &out, &errOut); code != 0 {
		t.Fatalf("exit %d with a bootstrap directory:\n%s", code, out.String())
	}
	inOrder(out.String(), slices.DeleteFunc(slices.Clone(check.Names), func(n string) bool { return n == "local issuer" }))
	if !strings.Contains(out.String(), "\nok   bootstrap: bootstrap dir "+booted["LUX_BOOTSTRAP_DIR"]+": 1 files read, 1 to create, 0 to update, 0 unchanged; nothing written\n") {
		t.Errorf("no ok bootstrap line with the counts:\n%s", out.String())
	}
	booted["LUX_BOOTSTRAP_DIR"] = writeDir(t, map[string]string{
		"orphan.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: orphan\nspec:\n  targets:\n    - provider: nobody\n      model: x\n",
	})
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(booted), &out, &errOut); code != 1 {
		t.Fatalf("exit %d with a bootstrap directory that does not resolve:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "\nfail bootstrap: ") || !strings.Contains(out.String(), "orphan.yaml: Model orphan: not_found") {
		t.Errorf("no failing bootstrap line naming the file:\n%s", out.String())
	}

	vars["LUX_OIDC_ISSUERS"] = "http://127.0.0.1:1"
	out.Reset()
	if code := run(t.Context(), []string{"check"}, env(vars), &out, &errOut); code != 1 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "\nfail issuers: ") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr %q; the lines are stdout's", errOut.String())
	}
}

// TestCheckIsReadOnly is spec 017's row: luxd check against a serving
// installation changes no object, leaves no archive object behind, sends
// the sink one check.ping and nothing else, and asks the authorizer the
// one probe. The check runs as its own process over the same endpoints
// the serving process dials, which is how an operator runs it.
func TestCheckIsReadOnly(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	authorizer := stub.New(t)
	events := stubsink.New(stubsink.Options{Secret: []byte(stubsink.DefaultSecret)})
	sinkSrv := httptest.NewServer(events)
	defer sinkSrv.Close()
	archive := s3test.New(t, "lux")
	vars := serveEnv(t, map[string]string{
		"LUX_OIDC_ISSUERS":        iss.URL(),
		"LUX_AUTHORIZER_URL":      authorizer.URL(),
		"LUX_AUTHORIZER_TOKEN":    authorizer.Token(),
		"LUX_EVENTS_URL":          sinkSrv.URL,
		"LUX_EVENTS_SECRET":       stubsink.DefaultSecret,
		"LUX_REQUESTLOG_EXPORTER": "s3",
		"LUX_S3_ENDPOINT":         archive.URL(),
		"LUX_S3_BUCKET":           "lux",
		"LUX_S3_ACCESS_KEY":       s3test.Key,
		"LUX_S3_SECRET_KEY":       s3test.Secret,
	})
	srv := startServe(t, vars)
	defer srv.stop()
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}
	if resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/providers/openai",
		`{"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider","metadata":{"name":"openai"},"spec":{"dialect":"openai","baseURL":"https://api.example.com/v1","credential":{"value":"sk-canary"}}}`, bearer...); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	// The snapshot is every object as the API lists it, less status.health,
	// which the serve's own health job writes on its schedule whether or
	// not a check runs; the members a write would move, version and
	// updatedAt among them, stay in the comparison.
	snapshot := func() string {
		var parts []string
		for _, kind := range []string{"providers", "models", "keys", "budgets"} {
			resp, body := do(t, http.MethodGet, srv.publicURL+"/v1/"+kind, "", bearer...)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /v1/%s: %d %s", kind, resp.StatusCode, body)
			}
			var list struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal([]byte(body), &list); err != nil {
				t.Fatalf("GET /v1/%s: %v in %s", kind, err, body)
			}
			for _, item := range list.Items {
				if status, ok := item["status"].(map[string]any); ok {
					delete(status, "health")
				}
			}
			normalised, err := json.Marshal(list.Items)
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, string(normalised))
		}
		return strings.Join(parts, "\n")
	}
	// The serve's own provider.created reaches the sink first, so the
	// count below starts after it and the check's delivery is the one
	// that follows.
	deadline := time.Now().Add(5 * time.Second)
	for len(events.Events()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the serve delivered no event for the Provider it created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := snapshot()
	eventsBefore, requestsBefore, keysBefore := len(events.Events()), len(authorizer.Requests()), archive.Keys()

	var out bytes.Buffer
	if code := run(t.Context(), []string{"check"}, env(vars), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}

	// The authorizer's record is read before the second snapshot, whose
	// list requests are decisions of their own.
	probes := authorizer.Requests()[requestsBefore:]
	if len(probes) != 1 || probes[0].Resource.ID != authz.ProbeID {
		t.Errorf("the authorizer received %+v, want the one probe", probes)
	}
	if after := snapshot(); after != before {
		t.Errorf("the objects changed:\n%s\n---\n%s", before, after)
	}
	if got := archive.Keys(); strings.Join(got, ",") != strings.Join(keysBefore, ",") {
		t.Errorf("the archive holds %v, held %v: the check object was not deleted", got, keysBefore)
	}
	delivered := events.Events()[eventsBefore:]
	if len(delivered) != 1 {
		t.Fatalf("%d events reached the sink, want one check.ping", len(delivered))
	}
	var ping struct {
		Type   string         `json:"type"`
		Reason string         `json:"reason"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(delivered[0].Body, &ping); err != nil {
		t.Fatal(err)
	}
	if ping.Type != "check.ping" || ping.Reason != "check" || len(ping.Data) != 0 {
		t.Errorf("the sink received %s, want a check.ping with reason check and empty data", delivered[0].Body)
	}
	if strings.Contains(out.String(), "sk-canary") {
		t.Errorf("a credential is in the lines:\n%s", out.String())
	}
}
