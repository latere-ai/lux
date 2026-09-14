// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/test/stubs/provider"
	"latere.ai/x/lux/test/stubs/sink"
)

// syncBuffer lets the test read stdout while run still writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var line = regexp.MustCompile(`(?m)^lux-stubs: ([a-z]+) (http://\S+)$`)

// stubs is one run of the binary in this process: the URL of each stub
// and stop, which cancels the context and returns the exit code.
type stubs struct {
	urls   map[string]string
	out    *syncBuffer
	errOut *syncBuffer
	stop   func() int
}

// start runs the binary with args and returns once every line is
// printed.
func start(t *testing.T, args ...string) *stubs {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out, errOut := &syncBuffer{}, &syncBuffer{}
	codec := make(chan int, 1)
	go func() { codec <- run(ctx, args, out, errOut) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(out.String(), "lux-stubs: ready\n") {
			urls := map[string]string{}
			for _, m := range line.FindAllStringSubmatch(out.String(), -1) {
				urls[m[1]] = m[2]
			}
			var once sync.Once
			var exit int
			s := &stubs{urls: urls, out: out, errOut: errOut, stop: func() int {
				once.Do(func() {
					cancel()
					select {
					case exit = <-codec:
					case <-time.After(shutdownGrace + 5*time.Second):
						t.Fatal("lux-stubs did not stop")
					}
				})
				return exit
			}}
			t.Cleanup(func() { s.stop() })
			return s
		}
		select {
		case code := <-codec:
			t.Fatalf("lux-stubs exited %d before it was ready; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("lux-stubs never reported ready; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// do sends one request and reads the answer whole.
func do(t *testing.T, method, url, body string, header http.Header) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

func bearer(value string) http.Header { return http.Header{"Authorization": {"Bearer " + value}} }

// TestLuxStubsServesEveryStub: the binary prints one line per stub with
// its URL and a ready line, each URL answers as its stub does, and a
// cancelled context is exit 0.
func TestLuxStubsServesEveryStub(t *testing.T) {
	s := start(t)
	for _, name := range names {
		if s.urls[name] == "" {
			t.Fatalf("no line for %s in:\n%s", name, s.out.String())
		}
	}
	if code, body := do(t, http.MethodGet, s.urls["openai"]+"/_received", "", nil); code != 200 || body != "[]" {
		t.Fatalf("GET openai/_received = %d %s", code, body)
	}
	if code, body := do(t, http.MethodPost, s.urls["anthropic"]+"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, http.Header{"x-api-key": {provider.DefaultCredential}}); code != 200 || !strings.Contains(body, provider.Content("anthropic", "m", "hi")) {
		t.Fatalf("POST anthropic/v1/messages = %d %s", code, body)
	}
	if code, body := do(t, http.MethodGet, s.urls["issuer"]+"/.well-known/openid-configuration", "", nil); code != 200 || !strings.Contains(body, `"issuer":"`+s.urls["issuer"]+`"`) {
		t.Fatalf("discovery = %d %s", code, body)
	}
	if !strings.Contains(s.out.String(), "lux-stubs: issuer url "+s.urls["issuer"]+"\n") {
		t.Fatalf("no issuer url line in:\n%s", s.out.String())
	}
	// The shared stub answers null for an empty record where the issuer
	// answers []; a reader across the process boundary takes both.
	if code, body := do(t, http.MethodGet, s.urls["authorizer"]+"/requests", "", nil); code != 200 || (strings.TrimSpace(body) != "[]" && strings.TrimSpace(body) != "null") {
		t.Fatalf("GET authorizer/requests = %d %s", code, body)
	}
	if code, body := do(t, http.MethodPost, s.urls["authorizer"], `{"subject":"s","action":"key.create","resource":{"kind":"Key"}}`, bearer(stub.DefaultToken)); code != 200 || !strings.Contains(body, `"allow":true`) {
		t.Fatalf("a decision = %d %s", code, body)
	}
	if code, body := do(t, http.MethodGet, s.urls["sink"]+"/_events", "", nil); code != 200 || body != "[]" {
		t.Fatalf("GET sink/_events = %d %s", code, body)
	}
	client := events.NewSink(events.SinkOptions{URL: s.urls["sink"], Secret: []byte(sink.DefaultSecret), Client: &http.Client{}})
	if err := client.Deliver(t.Context(), []byte(`{"id":"evt_1"}`)); err != nil {
		t.Fatalf("a delivery with the default secret: %v", err)
	}
	if code := s.stop(); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, s.errOut.String())
	}
}

// TestLuxStubsFlags: every flag reaches its stub.
func TestLuxStubsFlags(t *testing.T) {
	s := start(t,
		"-credential", "sk-flag",
		"-issuer-url", "http://issuer.example.com:9000",
		"-es256",
		"-authorizer-token", "flag-token",
		"-authorizer-deny", "key.create",
		"-sink-secret", "flag-secret",
		"-fail-first", "1",
	)
	if code, _ := do(t, http.MethodGet, s.urls["openai"]+"/v1/models", "", bearer(provider.DefaultCredential)); code != 401 {
		t.Fatalf("the default credential = %d, want 401 under -credential", code)
	}
	if code, _ := do(t, http.MethodGet, s.urls["openai"]+"/v1/models", "", bearer("sk-flag")); code != 200 {
		t.Fatalf("the flag's credential = %d", code)
	}
	if _, body := do(t, http.MethodGet, s.urls["issuer"]+"/.well-known/openid-configuration", "", nil); !strings.Contains(body, `"issuer":"http://issuer.example.com:9000"`) {
		t.Fatalf("discovery under -issuer-url = %s", body)
	}
	if _, body := do(t, http.MethodGet, s.urls["issuer"]+"/jwks", "", nil); !strings.Contains(body, `"alg":"ES256"`) {
		t.Fatalf("the key set under -es256 = %s", body)
	}
	decision := `{"subject":"s","action":"key.create","resource":{"kind":"Key"}}`
	if code, _ := do(t, http.MethodPost, s.urls["authorizer"], decision, bearer(stub.DefaultToken)); code != 401 {
		t.Fatalf("the default token = %d, want 401 under -authorizer-token", code)
	}
	if code, body := do(t, http.MethodPost, s.urls["authorizer"], decision, bearer("flag-token")); code != 200 || !strings.Contains(body, `"allow":false`) || !strings.Contains(body, "-authorizer-deny key.create") {
		t.Fatalf("the denied action = %d %s", code, body)
	}
	if code, body := do(t, http.MethodPost, s.urls["authorizer"], `{"subject":"s","action":"key.read","resource":{"kind":"Key"}}`, bearer("flag-token")); code != 200 || !strings.Contains(body, `"allow":true`) {
		t.Fatalf("another action = %d %s", code, body)
	}
	client := events.NewSink(events.SinkOptions{URL: s.urls["sink"], Secret: []byte("flag-secret"), Client: &http.Client{}})
	if err := client.Deliver(t.Context(), []byte(`{"id":"evt_1"}`)); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the first delivery under -fail-first 1: %v", err)
	}
	if err := client.Deliver(t.Context(), []byte(`{"id":"evt_1"}`)); err != nil {
		t.Fatalf("the second delivery: %v", err)
	}
	wrong := events.NewSink(events.SinkOptions{URL: s.urls["sink"], Secret: []byte(sink.DefaultSecret), Client: &http.Client{}})
	if err := wrong.Deliver(t.Context(), []byte(`{"id":"evt_2"}`)); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("the default secret under -sink-secret: %v", err)
	}

	failing := start(t, "-authorizer-fail", "503")
	if code, _ := do(t, http.MethodPost, failing.urls["authorizer"], decision, bearer(stub.DefaultToken)); code != 503 {
		t.Fatalf("under -authorizer-fail 503 = %d", code)
	}
}

// TestLuxStubsRefusesBadStarts: a bad flag and a stray argument are usage
// errors, an outage mode outside the list and a taken address are
// start-up failures, each one line on stderr.
func TestLuxStubsRefusesBadStarts(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"-no-such-flag"}, io.Discard, &errOut); code != 2 {
		t.Fatalf("a bad flag: exit %d", code)
	}
	errOut.Reset()
	if code := run(t.Context(), []string{"serve"}, io.Discard, &errOut); code != 2 || !strings.Contains(errOut.String(), `unexpected argument "serve"`) {
		t.Fatalf("a stray argument: exit %d, stderr %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run(t.Context(), []string{"-authorizer-fail", "sometimes"}, io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "-authorizer-fail") || strings.Count(errOut.String(), "\n") != 1 {
		t.Fatalf("a bad outage mode: exit %d, stderr %q", code, errOut.String())
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	errOut.Reset()
	if code := run(t.Context(), []string{"-sink-addr", ln.Addr().String()}, io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "-sink-addr") || !strings.Contains(errOut.String(), "address already in use") {
		t.Fatalf("a taken address: exit %d, stderr %q", code, errOut.String())
	}
}

// TestStubsMountTheSharedPackages: the binary's build list reaches
// latere.ai/x/pkg/authkit/issuertest and latere.ai/x/pkg/authz/stub, and
// no package under test/stubs serves a discovery document, a key set, or
// an authorization decision of its own.
func TestStubsMountTheSharedPackages(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.Command("go", "list", "-deps", "./cmd/lux-stubs")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := string(out)
	for _, want := range []string{"latere.ai/x/pkg/authkit/issuertest\n", "latere.ai/x/pkg/authz/stub\n"} {
		if !strings.Contains(deps, want) {
			t.Errorf("the build list lacks %s", strings.TrimSpace(want))
		}
	}
	forbidden := []string{"openid-configuration", `"/jwks"`, `"allow":`, "authz.Decision{"}
	err = filepath.WalkDir(filepath.Join(root, "test", "stubs"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if bytes.Contains(data, []byte(f)) {
				t.Errorf("%s serves a route of a shared package: it contains %q", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// moduleRoot is the module root, found from this file.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", dir, err)
	}
	return dir
}
