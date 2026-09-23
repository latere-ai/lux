// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/httpjson"
)

// binary is the built lux, shared by the tests that run it as a process
// and read its bytes; TestMain builds it once and removes it after.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lux-binary")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "lux")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		panic("building lux: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestMainWiresTheProcess runs main with the process's own arguments and
// streams swapped: the exit code is the command's and stdout carries
// the identity.
func TestMainWiresTheProcess(t *testing.T) {
	var got int
	exit = func(code int) { got = code }
	t.Cleanup(func() { exit = os.Exit })
	oldArgs, oldStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = oldArgs, oldStdout })
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = out
	os.Args = []string{"lux", "-version"}
	main()
	_ = out.Close()
	data, _ := os.ReadFile(out.Name())
	if got != 0 || string(data) != "lux dev (none, unknown)\n" {
		t.Fatalf("exit %d, stdout %q", got, data)
	}
	os.Args = []string{"lux", "frobnicate"}
	main()
	if got != 2 {
		t.Fatalf("an unknown command exits %d", got)
	}
}

// lux runs the built binary with exactly the environment given and
// returns the exit code and the two streams.
func lux(t *testing.T, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// TestBinaryExitCodes is spec 014's row: the built binary carries the
// three exit codes through the process, one case each.
func TestBinaryExitCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: "not_found", Message: "There is no such object.", Details: map[string]any{"request_id": "req_1"}})
	}))
	defer srv.Close()
	if code, out, errOut := lux(t, nil, "-version"); code != 0 || out != "lux dev (none, unknown)\n" || errOut != "" {
		t.Fatalf("-version: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if code, out, errOut := lux(t, []string{"LUX_URL=" + srv.URL, "LUX_TOKEN=t"}, "get", "key", "x"); code != 1 || out != "" || errOut != "There is no such object.\n" {
		t.Fatalf("a refusal: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if code, out, errOut := lux(t, nil, "frobnicate"); code != 2 || out != "" || !strings.HasPrefix(errOut, `There is no command "frobnicate".`) {
		t.Fatalf("a usage error: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if code, _, errOut := lux(t, []string{"LUX_URL=" + srv.URL, "LUX_TOKEN=t"}, "serve", "--dialect", "openai"); code != 2 || !strings.Contains(errOut, "lux serve needs -dialect, -upstream, and -as.") {
		t.Fatalf("lux serve without its flags: exit %d stderr %q", code, errOut)
	}
}

// TestClientEmbedsNoIssuer is spec 014's row: the binary contains no
// issuer URL, no OAuth client id, and no audience string, and its build
// list holds no identity library, so no command can perform a token
// exchange of any kind.
func TestClientEmbedsNoIssuer(t *testing.T) {
	list := exec.Command("go", "list", "-deps", ".")
	out, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		for _, forbidden := range []string{"authkit", "oauth2", "oidc", "/authz", "luxsdk", "jwt", "jose"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("the build list reaches %s", line)
			}
		}
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	// The claim and parameter names are quoted as a token exchange writes
	// them, since a bare word is also a substring of the linker's own
	// symbol names.
	for _, s := range []string{`"client_id"`, `"client_secret"`, `"device_code"`, `"grant_type"`, `"id_token"`, `"refresh_token"`, `"access_token"`, `"audience"`, `"aud"`, "openid-configuration", "/oauth2/", "/oauth/", "grant_type=", "client_id="} {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("the binary's string table holds %q", s)
		}
	}
}

// TestClientHonorsProxyVariables is spec 014's row: with HTTP_PROXY set
// to a refusing address the command fails to reach the server and exits
// 1, and with it set to a proxy that answers it reaches it, so the
// transport reads the process's proxy variables. The target is a name
// off loopback, since the standard transport never proxies loopback.
func TestClientHonorsProxyVariables(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "lux.example.com" && r.URL.Path == "/v1/self" {
			proxied.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sub":"alice"}` + "\n"))
			return
		}
		http.Error(w, "not the request expected", http.StatusBadGateway)
	}))
	defer proxy.Close()
	base := []string{"LUX_URL=http://lux.example.com", "LUX_TOKEN=t", "NO_PROXY=", "no_proxy="}
	code, out, errOut := lux(t, append(base, "HTTP_PROXY="+proxy.URL), "whoami")
	if code != 0 || out != `{"sub":"alice"}`+"\n" || proxied.Load() != 1 {
		t.Fatalf("through the proxy: exit %d stdout %q stderr %q, proxied %d", code, out, errOut, proxied.Load())
	}
	code, out, errOut = lux(t, append(base, "HTTP_PROXY=http://127.0.0.1:1"), "whoami", "-v")
	if code != 1 || out != "" || !strings.HasPrefix(errOut, "The server could not be reached") || !strings.Contains(errOut, "127.0.0.1:1") {
		t.Fatalf("a refusing proxy: exit %d stdout %q stderr %q", code, out, errOut)
	}
}
