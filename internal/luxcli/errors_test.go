// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lux/internal/api"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestErrorsBecomeExits is spec 014's row over every code of spec 011's
// table: each exits 1 with the table's sentence on stderr and nothing on
// stdout; an unknown code exits 1 with its own sentence; every usage
// error exits 2 with no request made.
func TestErrorsBecomeExits(t *testing.T) {
	f := newFake(t)
	for _, code := range api.Codes() {
		t.Run(string(code), func(t *testing.T) {
			f.reset()
			f.refuse("GET", "/v1/keys/x", code, "the developer's detail", "spec.field")
			r := run(t, f.env(nil), "get", "key", "x")
			if r.code != 1 || r.stdout != "" {
				t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			first, _, _ := strings.Cut(r.stderr, "\n")
			if first != code.Message() {
				t.Fatalf("stderr %q, want the sentence %q first", r.stderr, code.Message())
			}
			if strings.Contains(r.stderr, "the developer's detail") || strings.Contains(r.stderr, "spec.field") || strings.Contains(r.stderr, "req_FAKE") {
				t.Fatalf("the detail leaked without -v: %q", r.stderr)
			}
		})
	}
	t.Run("unknown code", func(t *testing.T) {
		f.reset()
		f.answers["GET /v1/keys/x"] = func(w http.ResponseWriter, _ *http.Request) {
			httpjson.WriteError(w, http.StatusTeapot, httpjson.Error{Code: "frobnicated", Message: "The server is frobnicated."})
		}
		r := run(t, f.env(nil), "get", "key", "x")
		if r.code != 1 || r.stderr != "The server is frobnicated.\n" || r.stdout != "" {
			t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("an envelope without a sentence", func(t *testing.T) {
		f.on("GET", "/v1/keys/x", 500, `{"error":{"code":"internal"}}`)
		r := run(t, f.env(nil), "get", "key", "x")
		if r.code != 1 || r.stderr != messageRefused+"\n" {
			t.Fatalf("exit %d stderr %q", r.code, r.stderr)
		}
	})
	t.Run("usage errors send nothing", func(t *testing.T) {
		for _, args := range [][]string{{"get", "key"}, {"apply"}, {"apply", "-f", "/nonexistent/file.yaml"}, {"frobnicate"}, {"get", "key", "x", "-o", "csv"}} {
			f.reset()
			r := run(t, f.env(nil), args...)
			if r.code != 2 || len(f.requests()) != 0 || r.stdout != "" {
				t.Fatalf("%v: exit %d, %d requests, stdout %q", args, r.code, len(f.requests()), r.stdout)
			}
		}
	})
}

func TestTransportAndUnreadableAnswersExitOne(t *testing.T) {
	r := run(t, map[string]string{"LUX_URL": "http://127.0.0.1:1", "LUX_TOKEN": canaryToken}, "whoami")
	if r.code != 1 || r.stderr != messageUnreachable+"\n" || r.stdout != "" {
		t.Fatalf("unreachable: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	r = run(t, map[string]string{"LUX_URL": "http://127.0.0.1:1", "LUX_TOKEN": canaryToken}, "whoami", "-v")
	if r.code != 1 || !strings.Contains(r.stderr, "code: "+codeUnreachable+"\n") || !strings.Contains(r.stderr, "detail: GET http://127.0.0.1:1/v1/self: ") {
		t.Fatalf("unreachable -v: stderr %q", r.stderr)
	}
	f := newFake(t)
	f.on("GET", "/v1/self", 502, "<html>bad gateway</html>")
	r = run(t, f.env(nil), "whoami", "-v")
	if r.code != 1 || !strings.HasPrefix(r.stderr, messageUnreadable+"\ncode: "+codeUnreadable+"\ndetail: GET ") || !strings.Contains(r.stderr, "status 502") || !strings.Contains(r.stderr, "request: req_FAKE\n") {
		t.Fatalf("unreadable -v: exit %d stderr %q", r.code, r.stderr)
	}
	// A 2xx list that is not a list is unreadable too.
	f.on("GET", "/v1/keys", 200, "[]")
	if r = run(t, f.env(nil), "list", "keys"); r.code != 1 || r.stderr != messageUnreadable+"\n" {
		t.Fatalf("not a list: exit %d stderr %q", r.code, r.stderr)
	}
	// A 2xx object that is not JSON cannot be rendered locally.
	f.on("GET", "/v1/self", 200, "not json")
	if r = run(t, f.env(nil), "whoami", "-o", "yaml"); r.code != 2 || r.stderr != messageUnreadable+"\n" {
		t.Fatalf("not json under yaml: exit %d stderr %q", r.code, r.stderr)
	}
}

// TestRefusalExtraLines is spec 014's row: the four codes in the extra
// line table print their line and no other code does.
func TestRefusalExtraLines(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/.well-known/lux", 200, `{"name":"lux","version":"0.1.0-test","apiVersion":"`+v1.APIVersion+`"}`)
	extra := map[api.Code]string{
		api.CodeRateLimited:        "retry after 30 seconds",
		api.CodeSpendExceeded:      "retry after 30 seconds",
		api.CodeBudgetExhausted:    "retry after 30 seconds",
		api.CodeConflict:           "read it again with lux get and re-apply",
		api.CodeUnsupportedVersion: "server: 0.1.0-test serves " + v1.APIVersion,
	}
	for _, code := range api.Codes() {
		f.answers["GET /v1/keys/x"] = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			writeCode(w, code, "")
		}
		r := run(t, f.env(nil), "get", "key", "x")
		want := code.Message() + "\n"
		if line, ok := extra[code]; ok {
			want += line + "\n"
		}
		if r.stderr != want {
			t.Errorf("%s: stderr %q, want %q", code, r.stderr, want)
		}
	}
	// Without Retry-After the line says only that a retry is later.
	f.refuse("GET", "/v1/keys/x", api.CodeRateLimited, "")
	if r := run(t, f.env(nil), "get", "key", "x"); r.stderr != api.CodeRateLimited.Message()+"\nretry later\n" {
		t.Errorf("no Retry-After: %q", r.stderr)
	}
	// An unreadable well-known document adds nothing.
	f.on("GET", "/.well-known/lux", 200, `{"name":"lux"}`)
	f.refuse("GET", "/v1/keys/x", api.CodeUnsupportedVersion, "")
	if r := run(t, f.env(nil), "get", "key", "x"); r.stderr != api.CodeUnsupportedVersion.Message()+"\n" {
		t.Errorf("well-known without apiVersion: %q", r.stderr)
	}
	f.on("GET", "/.well-known/lux", 503, `down`)
	if r := run(t, f.env(nil), "get", "key", "x"); r.stderr != api.CodeUnsupportedVersion.Message()+"\n" {
		t.Errorf("well-known down: %q", r.stderr)
	}
	// With -v the code, the paths, the detail, and the request id follow.
	f.refuse("PUT", "/v1/providers/openai", api.CodeInvalidField, "a loopback host needs LUX_UPSTREAM_ALLOW_PRIVATE", "spec.baseURL")
	r := run(t, f.env(nil), "apply", "-f", write(t, "p.yaml", providerYAML), "-v")
	want := api.CodeInvalidField.Message() + "\ncode: invalid_field\npaths: spec.baseURL\ndetail: a loopback host needs LUX_UPSTREAM_ALLOW_PRIVATE\nrequest: req_FAKE\n"
	if r.code != 1 || r.stderr != want {
		t.Fatalf("-v: exit %d stderr %q", r.code, r.stderr)
	}
}

// TestUnsupportedVersionLineWithoutAURL covers the extra line when no
// URL is known, which cannot happen through Run but guards serverLine.
func TestUnsupportedVersionLineWithoutAURL(t *testing.T) {
	a := &app{o: Options{Getenv: func(string) string { return "" }}}
	if line := a.serverLine(); line != "" {
		t.Fatalf("line %q", line)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }))
	defer srv.Close()
	a.url = srv.URL
	a.o.HTTP = srv.Client()
	if line := a.serverLine(); line != "" {
		t.Fatalf("line %q", line)
	}
}
