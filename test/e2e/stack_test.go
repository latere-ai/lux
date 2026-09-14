// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration || postgres

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/test/stubs/issuer"
	"latere.ai/x/lux/test/stubs/provider"
	"latere.ai/x/lux/test/stubs/sink"
)

// The binaries TestMain builds, and the module root. lux is built
// beside the two servers because spec 020's sandbox composition runs it
// as the workload inside the sandbox, holding a placeholder alone, and
// the example authorizer because that spec runs the endpoint its
// document prints beside luxd.
var (
	luxdBin, stubsBin, luxBin, authorizerBin string
	root                                     string
)

// kek is one 32-byte key encryption key in LUX_SECRETS_KEK's syntax.
const kek = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

// The shape of a Provider's base URL per dialect: the version prefix the
// dialect's API carries.
var versionPrefix = map[string]string{"openai": "/v1", "anthropic": "/v1", "gemini": "/v1beta", "lux": "/v1"}

// build compiles luxd and lux-stubs into a fresh directory and returns
// it; the caller removes it.
func build() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("no caller information")
	}
	root = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("module root %s: %w", root, err)
	}
	dir, err := os.MkdirTemp("", "lux-e2e")
	if err != nil {
		return "", err
	}
	for _, pkg := range []string{"./cmd/luxd", "./cmd/lux-stubs", "./cmd/lux", "./examples/authorizer"} {
		name := filepath.Base(pkg)
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), pkg)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("go build %s: %w\n%s", pkg, err, out)
		}
	}
	luxdBin, stubsBin, luxBin = filepath.Join(dir, "luxd"), filepath.Join(dir, "lux-stubs"), filepath.Join(dir, "lux")
	authorizerBin = filepath.Join(dir, "authorizer")
	return dir, nil
}

// syncBuffer lets a test read a process's output while it still writes.
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

// waitFor polls until read carries want, or fails after the deadline.
func waitFor(t *testing.T, what string, timeout time.Duration, read func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(read(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never carried %q:\n%s", what, want, read())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// eventually polls cond until it holds, or fails after the deadline.
func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// process is one child process with its output captured.
type process struct {
	cmd    *exec.Cmd
	out    *syncBuffer
	errOut *syncBuffer
	done   chan error
	once   sync.Once
	exit   error
}

func startProcess(t *testing.T, bin string, env map[string]string, args ...string) *process {
	t.Helper()
	p := &process{cmd: exec.Command(bin, args...), out: &syncBuffer{}, errOut: &syncBuffer{}, done: make(chan error, 1)}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.errOut
	p.cmd.Env = os.Environ()
	for k, v := range env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", bin, err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(p.kill)
	return p
}

// exited reports whether the process has ended.
func (p *process) exited() bool {
	select {
	case err := <-p.done:
		p.done <- err
		return true
	default:
		return false
	}
}

// kill ends the process at once; a process already gone is left alone.
func (p *process) kill() {
	p.once.Do(func() {
		if !p.exited() {
			_ = p.cmd.Process.Kill()
		}
		p.exit = <-p.done
	})
}

// stop asks the process to stop with SIGTERM and returns its exit code,
// or fails when it has not stopped within timeout.
func (p *process) stop(t *testing.T, timeout time.Duration) int {
	t.Helper()
	code := -1
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case p.exit = <-p.done:
		case <-time.After(timeout):
			_ = p.cmd.Process.Kill()
			p.exit = <-p.done
			t.Fatalf("the process did not stop within %s", timeout)
		}
	})
	var ee *exec.ExitError
	switch {
	case p.exit == nil:
		code = 0
	case errors.As(p.exit, &ee):
		code = ee.ExitCode()
	}
	return code
}

// stubs is one lux-stubs process: the URL of each stub, the issuer URL
// luxd is told, and the process.
type stubs struct {
	*process
	urls map[string]string
}

var stubLine = regexp.MustCompile(`(?m)^lux-stubs: ([a-z]+) (http://\S+)$`)

// startStubs starts lux-stubs with args and returns once it is ready.
func startStubs(t *testing.T, args ...string) *stubs {
	t.Helper()
	p := startProcess(t, stubsBin, nil, args...)
	waitFor(t, "lux-stubs stdout", 10*time.Second, p.out.String, "lux-stubs: ready\n")
	s := &stubs{process: p, urls: map[string]string{}}
	for _, m := range stubLine.FindAllStringSubmatch(p.out.String(), -1) {
		s.urls[m[1]] = m[2]
	}
	return s
}

// base is a Provider's baseURL toward the dialect's stub.
func (s *stubs) base(dialect string) string { return s.urls[dialect] + versionPrefix[dialect] }

// gateway is one luxd process: its two base URLs and the process.
type gateway struct {
	*process
	public, internal string
}

var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// freePort finds a port nothing listens at right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// serverEnv is the environment of a luxd in server mode over the stubs:
// a public listener on a port chosen up front so LUX_PUBLIC_URL can name
// it, localhost in that URL because the loop check compares hostnames
// alone, the memory store, the stub issuer, the stub authorizer, the stub
// sink, and the admission of loopback upstreams. extra overrides.
func serverEnv(t *testing.T, s *stubs, extra map[string]string) map[string]string {
	t.Helper()
	port := freePort(t)
	env := map[string]string{
		"LUX_PUBLIC_ADDR":            "127.0.0.1:" + strconv.Itoa(port),
		"LUX_INTERNAL_ADDR":          "127.0.0.1:0",
		"LUX_PUBLIC_URL":             "http://localhost:" + strconv.Itoa(port),
		"LUX_SECRETS_KEK":            kek,
		"LUX_OIDC_ISSUERS":           s.urls["issuer"],
		"LUX_OIDC_INSECURE_ISSUERS":  s.urls["issuer"],
		"LUX_AUTHORIZER_URL":         s.urls["authorizer"],
		"LUX_AUTHORIZER_TOKEN":       stub.DefaultToken,
		"LUX_EVENTS_URL":             s.urls["sink"],
		"LUX_EVENTS_SECRET":          sink.DefaultSecret,
		"LUX_UPSTREAM_ALLOW_PRIVATE": "1",
	}
	maps.Copy(env, extra)
	return env
}

// startLuxd starts luxd serve with env and returns once it listens.
func startLuxd(t *testing.T, env map[string]string) *gateway {
	t.Helper()
	p := startProcess(t, luxdBin, env, "serve")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m := listening.FindStringSubmatch(p.out.String()); m != nil {
			return &gateway{process: p, public: "http://" + m[1], internal: "http://" + m[2]}
		}
		if p.exited() {
			t.Fatalf("luxd exited before listening:\n%s%s", p.out.String(), p.errOut.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("luxd never reported its listeners:\n%s%s", p.out.String(), p.errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// response is one answer read whole.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) code() string { return r.header.Get("Lux-Error") }

// json reads the body as a generic document.
func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, r.body)
	}
	return doc
}

// client follows no redirect, so a 3xx is read as it is.
var client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends one request and reads the answer whole.
func do(t *testing.T, method, url string, header http.Header, body string) response {
	t.Helper()
	return doCtx(t, t.Context(), method, url, header, body)
}

func doCtx(t *testing.T, ctx context.Context, method, url string, header http.Header, body string) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, url, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func jsonBearer(token string) http.Header {
	h := bearer(token)
	h.Set("Content-Type", "application/json")
	return h
}

// mint asks the stub issuer for a token for sub.
func mint(t *testing.T, s *stubs, sub string) string {
	t.Helper()
	token, err := issuer.Mint(t.Context(), client, s.urls["issuer"], issuertest.Claims{Sub: sub})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// stack is a whole loopback installation: the stubs, a luxd in server
// mode, and a token for subject dev.
type stack struct {
	stubs *stubs
	gw    *gateway
	token string
}

// newStack starts the stubs with stubArgs and a luxd over them with extra
// in its environment.
func newStack(t *testing.T, extra map[string]string, stubArgs ...string) *stack {
	t.Helper()
	s := startStubs(t, stubArgs...)
	gw := startLuxd(t, serverEnv(t, s, extra))
	return &stack{stubs: s, gw: gw, token: mint(t, s, "dev")}
}

// apply PUTs one manifest by kind and name with the stack's token.
func (s *stack) apply(t *testing.T, kind, name, manifest string) response {
	t.Helper()
	h := bearer(s.token)
	h.Set("Content-Type", "application/yaml")
	resp := do(t, http.MethodPut, s.gw.public+"/v1/"+kind+"s/"+name, h, manifest)
	if resp.status != http.StatusCreated && resp.status != http.StatusOK {
		t.Fatalf("PUT /v1/%ss/%s = %d %s", kind, name, resp.status, resp.body)
	}
	return resp
}

// key applies a Key and returns its minted value.
func (s *stack) key(t *testing.T, name string, selectors ...string) string {
	t.Helper()
	resp := s.apply(t, "key", name, keyYAML(name, selectors...))
	value, _ := resp.json(t)["status"].(map[string]any)["value"].(string)
	if !strings.HasPrefix(value, "lux_") {
		t.Fatalf("the Key's value was not returned: %s", resp.body)
	}
	return value
}

// provider applies a Provider of the dialect toward its stub, the two
// jobs off unless jobs is true, with the stub's credential unless
// credential names another.
func (s *stack) provider(t *testing.T, name, dialect string, jobs bool, credential string) {
	t.Helper()
	if credential == "" {
		credential = provider.DefaultCredential
	}
	s.apply(t, "provider", name, providerYAML(name, dialect, s.stubs.base(dialect), credential, jobs))
}

// model applies a Model with one target and, when priced, the pricing
// TestE2ECostIsExact computes against: 2.50 in and 10 out per million.
func (s *stack) model(t *testing.T, name, providerName, upstream string, priced bool) {
	t.Helper()
	s.apply(t, "model", name, modelYAML(name, providerName, upstream, priced))
}

// fixtures is the tier's usual catalogue: the four Providers with the
// jobs off, one Model each named after its dialect, and a Key for all.
func (s *stack) fixtures(t *testing.T) (keyValue string) {
	t.Helper()
	for _, d := range []string{"openai", "anthropic", "gemini", "lux"} {
		s.provider(t, d, d, false, "")
		s.model(t, d+"-model", d, "stub-"+d, true)
	}
	return s.key(t, "dev", "*")
}

// chat sends one chat completion through the /openai door.
func (s *stack) chat(t *testing.T, keyValue, model, text string, stream bool) response {
	t.Helper()
	body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": text}}}
	if stream {
		body["stream"] = true
	}
	return do(t, http.MethodPost, s.gw.public+"/openai/v1/chat/completions", jsonBearer(keyValue), marshal(body))
}

// records reads GET /v1/requests for one Key.
func (s *stack) records(t *testing.T, keyName string) []map[string]any {
	t.Helper()
	resp := do(t, http.MethodGet, s.gw.public+"/v1/requests?key="+keyName, bearer(s.token), "")
	if resp.status != http.StatusOK {
		t.Fatalf("GET /v1/requests = %d %s", resp.status, resp.body)
	}
	items, _ := resp.json(t)["items"].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any))
	}
	return out
}

// received reads a stub provider's record.
func (s *stack) received(t *testing.T, dialect string) []provider.Received {
	t.Helper()
	resp := do(t, http.MethodGet, s.stubs.urls[dialect]+"/_received", nil, "")
	var out []provider.Received
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("GET /_received: %v: %s", err, resp.body)
	}
	return out
}

// events reads the stub sink's verified deliveries.
func (s *stack) events(t *testing.T) []sink.Event {
	t.Helper()
	resp := do(t, http.MethodGet, s.stubs.urls["sink"]+"/_events", nil, "")
	var out []sink.Event
	if err := json.Unmarshal(resp.body, &out); err != nil {
		t.Fatalf("GET /_events: %v: %s", err, resp.body)
	}
	return out
}

func marshal(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// number reads a number at a dotted path of a document.
func number(t *testing.T, doc map[string]any, path string) int64 {
	t.Helper()
	var cur any = doc
	for part := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s: %q is not an object", path, part)
		}
		if cur, ok = m[part]; !ok {
			t.Fatalf("no %s in %v", path, doc)
		}
	}
	f, ok := cur.(float64)
	if !ok {
		t.Fatalf("%s is %T, not a number", path, cur)
	}
	return int64(f)
}

const manifestHead = "apiVersion: lux.latere.ai/v1beta1\n"

func providerYAML(name, dialect, baseURL, credential string, jobs bool) string {
	y := manifestHead + "kind: Provider\nmetadata:\n  name: " + name + "\nspec:\n  dialect: " + dialect + "\n  baseURL: " + baseURL + "\n  credential:\n    value: " + credential + "\n"
	if !jobs {
		y += "  discovery:\n    mode: none\n  health:\n    mode: none\n"
	}
	return y
}

func modelYAML(name, providerName, upstream string, priced bool) string {
	y := manifestHead + "kind: Model\nmetadata:\n  name: " + name + "\nspec:\n  targets:\n    - provider: " + providerName + "\n      model: " + upstream + "\n"
	if priced {
		y += "  pricing:\n    input: \"2.50\"\n    output: \"10\"\n"
	}
	return y
}

func keyYAML(name string, selectors ...string) string {
	quoted := make([]string, 0, len(selectors))
	for _, s := range selectors {
		quoted = append(quoted, strconv.Quote(s))
	}
	return manifestHead + "kind: Key\nmetadata:\n  name: " + name + "\nspec:\n  models: [" + strings.Join(quoted, ", ") + "]\n"
}

func budgetYAML(name string) string {
	return manifestHead + "kind: Budget\nmetadata:\n  name: " + name + "\nspec:\n  amount: \"10\"\n  currency: USD\n  window: month\n"
}
