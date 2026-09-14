// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lux/internal/api"
)

// now is the clock every run reads, so AGE and --since are exact.
var now = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

// canaries are the values that must never reach stderr, and reach stdout
// only where spec 014 says.
const (
	canaryToken = "tok-canary-7f3c9a-never-on-stderr"
	canaryFile  = "file-canary-2b8e11-never-on-stderr"
	canaryCred  = "sk-canary-c0ffee-never-printed"
	canaryKey   = "lux_canary_9d4e1f_shown_once"
)

// seen is one request the fake server recorded.
type seen struct {
	Method, Path, Query, Auth, UA, ContentType, IfMatch string
	Body                                                string
}

// fake is a scripted server: answers by "METHOD path", every request
// recorded, and the API's own not_found envelope for a path with no
// script, so a command that reaches a route the test did not expect is
// visible as a refusal.
type fake struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []seen
	answers map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{answers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, seen{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			UA: r.Header.Get("User-Agent"), ContentType: r.Header.Get("Content-Type"), IfMatch: r.Header.Get("If-Match"), Body: string(body)})
		h := f.answers[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Lux-Request-Id", "req_FAKE")
		if h != nil {
			h(w, r)
			return
		}
		writeCode(w, api.CodeNotFound, r.Method+" "+r.URL.Path+" is not in the route table")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// writeCode answers one code as the API does: its status, its sentence,
// and the details with the request id.
func writeCode(w http.ResponseWriter, code api.Code, detail string, paths ...string) {
	details := map[string]any{"request_id": "req_FAKE"}
	if detail != "" {
		details["detail"] = detail
	}
	if len(paths) > 0 {
		details["paths"] = paths
	}
	httpjson.WriteError(w, code.Status(), httpjson.Error{Code: string(code), Message: code.Message(), Details: details})
}

// on scripts one answer: a status and a body.
func (f *fake) on(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// refuse scripts one refusal by code.
func (f *fake) refuse(method, path string, code api.Code, detail string, paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) { writeCode(w, code, detail, paths...) }
}

func (f *fake) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.got...)
}

func (f *fake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = nil
}

func (f *fake) last(t *testing.T) seen {
	t.Helper()
	got := f.requests()
	if len(got) == 0 {
		t.Fatal("no request reached the server")
	}
	return got[len(got)-1]
}

// env is the environment of one run: LUX_URL at the fake and LUX_TOKEN
// set, unless the extra entries say otherwise; an empty value unsets.
func (f *fake) env(extra map[string]string) map[string]string {
	m := map[string]string{"LUX_URL": f.srv.URL, "LUX_TOKEN": canaryToken}
	for k, v := range extra {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return m
}

// result is one run's exit code and streams.
type result struct {
	code   int
	stdout string
	stderr string
}

// run drives the command in process.
func run(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), Options{
		Args:   args,
		Getenv: func(k string) string { return env[k] },
		Stdout: &out, Stderr: &errOut,
		Version: "1.2.3", Commit: "abc1234", Date: "2026-09-14",
		Now: func() time.Time { return now },
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// write puts a fixture file in the test's directory.
func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The fixtures: one manifest per kind, as a person writes them.
const (
	providerYAML = "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: openai\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n"
	modelYAML    = "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: gpt-5\nspec:\n  targets:\n    - provider: openai\n      model: gpt-5\n"
	budgetYAML   = "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: team\nspec:\n  amount: \"50\"\n  window: month\n"
	keyYAML      = "apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: run-42\nspec:\n  models: [gpt-5]\n  budget: team\n"
)
