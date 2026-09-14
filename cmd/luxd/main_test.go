// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
)

// env reads m, and answers publicURL for LUX_PUBLIC_URL when m does not
// mention it, since every mode requires the variable and few tests are
// about it.
func env(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		if k == "LUX_PUBLIC_URL" {
			return publicURL
		}
		return ""
	}
}

// kek is one 32-byte key in the variable's syntax; publicURL is the
// address every response's URLs are built from.
const (
	kek       = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	publicURL = "https://lux.example.com"
)

// serveEnv is the smallest environment serve starts under: loopback
// listeners and one issuer, a stub on loopback so the start-up fetch of
// spec 006 has something to reach. The extra entries override.
func serveEnv(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	m := map[string]string{
		"LUX_PUBLIC_ADDR":   "127.0.0.1:0",
		"LUX_INTERNAL_ADDR": "127.0.0.1:0",
		"LUX_OIDC_ISSUERS":  issuertest.New(t).URL(),
		"LUX_SECRETS_KEK":   kek,
	}
	maps.Copy(m, extra)
	return m
}

// TestServeRefusesToStartWithoutAnIssuer is spec 006's first start-up
// rule at the process: no issuer and no manifest directory is exit 1
// with the one configuration line naming the variable.
func TestServeRefusesToStartWithoutAnIssuer(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{
		"LUX_PUBLIC_ADDR":   "127.0.0.1:0",
		"LUX_INTERNAL_ADDR": "127.0.0.1:0",
	}), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "luxd: configuration: ") || !strings.Contains(got, "LUX_OIDC_ISSUERS is unset") {
		t.Fatalf("stderr = %q", got)
	}
}

func TestVersionFlagPrintsTheIdentityAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"-version"}, env(nil), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "luxd dev (") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"frobnicate"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), `unknown subcommand "frobnicate"`) {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// TestStartupRequiresAWorkingKEK is spec 005's start-up rule at the
// process: serve refuses to start with LUX_SECRETS_KEK absent and with a
// key that is not 32 bytes, naming the key by position and never by
// value; with the memory store the variable is still required; in file
// mode it is not. The key that opens no stored credential is the
// secrets package's half of the row.
func TestStartupRequiresAWorkingKEK(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{
		"LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0", "LUX_OIDC_ISSUERS": "https://login.example.com",
	}), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "LUX_SECRETS_KEK is unset") {
		t.Fatalf("absent: exit %d, stderr %q", code, errOut.String())
	}
	const short = "CQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQ=="
	errOut.Reset()
	code = run(t.Context(), nil, env(map[string]string{
		"LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0", "LUX_OIDC_ISSUERS": "https://login.example.com",
		"LUX_SECRETS_KEK": kek + "," + short,
	}), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "LUX_SECRETS_KEK key 2 of 2 decodes to 31 bytes, not 32") || strings.Contains(errOut.String(), short) {
		t.Fatalf("a short key: exit %d, stderr %q", code, errOut.String())
	}

	dir := t.TempDir()
	writeFile(t, dir, "provider.yaml", providerYAML)
	srv := startServe(t, map[string]string{"LUX_MANIFEST_DIR": dir, "OPENAI_KEY": "sk-live", "LUX_OIDC_ISSUERS": "", "LUX_SECRETS_KEK": ""})
	if !strings.Contains(srv.out.String(), "luxd: credentials: read from the environment by the file mode") {
		t.Fatalf("no file-mode credentials line in:\n%s", srv.out.String())
	}
	if code := srv.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestServeStartsTheJobs: in server mode the start-up log names the keys
// and the rows they open, and the two jobs hold their leases while the
// process serves and give them back at stop.
func TestServeStartsTheJobs(t *testing.T) {
	srv := startServe(t, nil)
	if !strings.Contains(srv.out.String(), "luxd: credentials: 1 key(s) in LUX_SECRETS_KEK, 0 stored row(s) open under them") {
		t.Fatalf("no credentials line in:\n%s", srv.out.String())
	}
	if code := srv.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(srv.errOut.String(), `"level":"ERROR"`) {
		t.Fatalf("the jobs logged an error:\n%s", srv.errOut.String())
	}
}

// TestServeStartsTheMeteringFlush is spec 009's wiring at the process:
// the Limiter's and the Recorder's flush loops start with the jobs on
// LUX_METERING_FLUSH, the start-up log names the interval, and a stop
// flushes once more without an error.
func TestServeStartsTheMeteringFlush(t *testing.T) {
	srv := startServe(t, map[string]string{"LUX_METERING_FLUSH": "250ms"})
	if !strings.Contains(srv.out.String(), "luxd: metering: spend counters and usage aggregates flush every 250ms") {
		t.Fatalf("no metering line in:\n%s", srv.out.String())
	}
	time.Sleep(600 * time.Millisecond) // two intervals with nothing to flush
	if code := srv.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(srv.errOut.String(), `"level":"ERROR"`) {
		t.Fatalf("the flush logged an error:\n%s", srv.errOut.String())
	}
}

// TestRewrapNeedsTheStore is spec 005's row at the process: without
// LUX_DB_URL the role exits 1 naming the variable; with one, the
// Postgres store is a later phase and the role says so with exit 1.
func TestRewrapNeedsTheStore(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"rewrap"}, env(map[string]string{"LUX_SECRETS_KEK": kek}), io.Discard, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "luxd: configuration: LUX_DB_URL is unset") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q", got)
	}
	errOut.Reset()
	if code := run(t.Context(), []string{"rewrap"}, env(map[string]string{"LUX_SECRETS_KEK": kek, "LUX_DB_URL": "postgres://lux:secret@db.example.com/lux"}), io.Discard, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "luxd: LUX_DB_URL: the Postgres store is not in this build") || strings.Contains(got, "secret") {
		t.Fatalf("stderr = %q", got)
	}
	if code := run(t.Context(), []string{"rewrap", "-no-such-flag"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("a bad flag: exit %d", code)
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"serve", "-no-such-flag"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestSubcommandSplitsAroundTheFirstBareWord(t *testing.T) {
	name, rest := subcommand([]string{"-a", "serve", "-b"})
	if name != "serve" || strings.Join(rest, " ") != "-a -b" {
		t.Fatalf("subcommand() = %q, %q", name, rest)
	}
	if name, rest := subcommand([]string{"-version"}); name != "" || len(rest) != 1 {
		t.Fatalf("subcommand() = %q, %q", name, rest)
	}
}

func TestBadConfigurationExitsOneWithOneLine(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{
		"LUX_PUBLIC_ADDR":   "nope",
		"LUX_INTERNAL_ADDR": "nope",
	}), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got := errOut.String(); !strings.HasPrefix(got, "luxd: configuration: ") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q", got)
	}
}

func TestOccupiedAddressExitsOne(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	for _, tc := range []struct{ name, public, internal string }{
		{"public", ln.Addr().String(), "127.0.0.1:0"},
		{"internal", "127.0.0.1:0", ln.Addr().String()},
	} {
		var errOut bytes.Buffer
		code := run(t.Context(), nil, env(serveEnv(t, map[string]string{
			"LUX_PUBLIC_ADDR":   tc.public,
			"LUX_INTERNAL_ADDR": tc.internal,
		})), io.Discard, &errOut)
		if code != 1 || !strings.Contains(errOut.String(), "address already in use") {
			t.Fatalf("%s: exit %d, stderr %q", tc.name, code, errOut.String())
		}
	}
}

// syncBuffer lets the test read stdout while serve still writes to it.
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

var listening = regexp.MustCompile(`listening public=(\S+) internal=(\S+)`)

// server is one serve run on loopback ports: its two base URLs, what it
// wrote so far, and stop, which cancels the context and returns the exit
// code.
type server struct {
	publicURL, internalURL string
	out, errOut            *syncBuffer
	stop                   func() int
}

// startServe runs serve on loopback ports with extra in the environment
// and returns once the listeners are reported.
func startServe(t *testing.T, extra map[string]string) server {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	vars := serveEnv(t, extra)
	out, errOut := &syncBuffer{}, &syncBuffer{}
	codec := make(chan int, 1)
	go func() { codec <- run(ctx, nil, env(vars), out, errOut) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			return server{"http://" + m[1], "http://" + m[2], out, errOut, func() int {
				cancel()
				select {
				case code := <-codec:
					return code
				case <-time.After(gracePeriod + 10*time.Second):
					t.Fatal("serve did not stop")
					return -1
				}
			}}
		}
		select {
		case code := <-codec:
			t.Fatalf("serve exited %d before listening; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never reported its listeners; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFor polls buf until it carries want, or fails after five seconds.
func waitFor(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q in:\n%s", want, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServeAnswersTheProbesOnBothListenersAndStopsCleanly(t *testing.T) {
	srv := startServe(t, nil)
	publicURL, internalURL, stop := srv.publicURL, srv.internalURL, srv.stop

	for _, base := range []string{publicURL, internalURL} {
		for _, p := range []string{"/livez", "/readyz"} {
			if code, body := get(t, base+p); code != 200 || body != "ok\n" {
				t.Errorf("GET %s%s = %d %q", base, p, code, body)
			}
		}
		if code, body := get(t, base+"/version"); code != 200 || !strings.Contains(body, `"version":"dev"`) {
			t.Errorf("GET %s/version = %d %q", base, code, body)
		}
	}
	if code, body := get(t, publicURL+"/"); code != 200 || !strings.HasPrefix(body, "luxd dev (") {
		t.Errorf("GET / = %d %q", code, body)
	}
	if code, _ := get(t, publicURL+"/metrics"); code != 404 {
		t.Errorf("GET /metrics on the public listener = %d, want 404: the registry is the internal listener's alone", code)
	}

	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestReadinessFailsOnceDrainingBegins(t *testing.T) {
	draining := make(chan struct{})
	check := notDraining(draining)
	if err := check(t.Context()); err != nil {
		t.Fatalf("before draining: %v", err)
	}
	close(draining)
	if err := check(t.Context()); err == nil || err.Error() != "shutting down" {
		t.Fatalf("after draining: %v", err)
	}
}

func TestSleepCtxReturnsEarlyWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	sleepCtx(ctx, time.Minute)
	if time.Since(start) > time.Second {
		t.Fatal("sleepCtx waited for the timer despite a cancelled context")
	}
}

// TestMemoryStoreLogsItsAssumptions is spec 010's row: without a store
// the start-up log names the three consequences in one line.
func TestMemoryStoreLogsItsAssumptions(t *testing.T) {
	srv := startServe(t, nil)
	defer srv.stop()
	var line string
	for l := range strings.SplitSeq(srv.out.String(), "\n") {
		if strings.Contains(l, "state in memory") {
			line = l
		}
	}
	if !strings.HasPrefix(line, "luxd: ") {
		t.Fatalf("no memory line in:\n%s", srv.out.String())
	}
	for _, want := range []string{"nothing is recovered after a restart", "every window starts empty", "only replica"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lacks %q: %s", want, line)
		}
	}
}

// TestDatabaseIsNotSelectableYet: the Postgres store is a later phase, so
// a configured LUX_DB_URL is refused with one line rather than answered
// with state in memory.
func TestDatabaseIsNotSelectableYet(t *testing.T) {
	var errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"LUX_OIDC_ISSUERS": "https://login.example.com", "LUX_SECRETS_KEK": kek, "LUX_DB_URL": "postgres://lux:secret@db.example.com/lux"}), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	got := errOut.String()
	if !strings.HasPrefix(got, "luxd: LUX_DB_URL: ") || strings.Count(got, "\n") != 1 || strings.Contains(got, "secret") {
		t.Fatalf("stderr = %q", got)
	}
}

const manifestHead = "apiVersion: lux.latere.ai/v1beta1\n"

// providerYAML turns both jobs off, so a test binary dials no host.
const providerYAML = manifestHead + `kind: Provider
metadata:
  name: openai
spec:
  dialect: openai
  baseURL: https://api.example.com/v1
  credential:
    valueFrom:
      env: OPENAI_KEY
  discovery:
    mode: none
  health:
    mode: none
`

const modelYAML = manifestHead + `kind: Model
metadata:
  name: gpt-5
spec:
  targets:
    - provider: openai
`

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFileModeStartupNamesTheFailingFile is spec 010's row at the
// binary: a file that fails to resolve is one luxd: line naming the file
// and the path, exit 1, and nothing is served.
func TestFileModeStartupNamesTheFailingFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "model.yaml", modelYAML)
	var out, errOut bytes.Buffer
	code := run(t.Context(), nil, env(map[string]string{"LUX_MANIFEST_DIR": dir, "LUX_PUBLIC_ADDR": "127.0.0.1:0", "LUX_INTERNAL_ADDR": "127.0.0.1:0"}), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	got := errOut.String()
	if !strings.HasPrefix(got, "luxd: manifest dir "+dir) || !strings.Contains(got, "model.yaml: not_found at spec.targets[0].provider") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q", got)
	}
	if strings.Contains(out.String(), "listening") {
		t.Fatal("a failed start served")
	}
}

// TestFileModeServesAndReloadsOnSIGHUP: the start-up line names the
// directory, SIGHUP picks up an added file, and a directory that stops
// resolving is reported while the previous snapshot keeps serving.
func TestFileModeServesAndReloadsOnSIGHUP(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "provider.yaml", providerYAML)
	srv := startServe(t, map[string]string{"LUX_MANIFEST_DIR": dir, "OPENAI_KEY": "sk-live"})
	if !strings.Contains(srv.out.String(), "luxd: manifest dir "+dir+": 1 files read, 1 providers") {
		t.Fatalf("no file mode line in:\n%s", srv.out.String())
	}
	if code, body := get(t, srv.internalURL+"/readyz"); code != 200 || body != "ok\n" {
		t.Fatalf("GET /readyz = %d %q", code, body)
	}

	writeFile(t, dir, "model.yaml", modelYAML)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, srv.out, "luxd: reloaded manifest dir "+dir+": 2 files read, 1 providers, 0 budgets, 1 models")

	writeFile(t, dir, "model.yaml", strings.ReplaceAll(modelYAML, "provider: openai", "provider: nowhere"))
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, srv.errOut, "luxd: reload failed, the previous snapshot keeps serving: manifest dir "+dir+": model.yaml: not_found at spec.targets[0].provider")

	if code := srv.stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
