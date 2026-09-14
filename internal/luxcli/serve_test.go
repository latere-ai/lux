// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lux/internal/api"
)

// serveFake is a gateway held to the bytes: the PUT of the Provider and
// the session route, over unencrypted HTTP/2, which is what the agent's
// plaintext client speaks. The session route refuses by default, so a
// run ends without a tunnel.
type serveFake struct {
	srv     *httptest.Server
	mu      sync.Mutex
	got     []seen
	session http.HandlerFunc
}

func newServeFake(t *testing.T) *serveFake {
	t.Helper()
	f := &serveFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/providers/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"kind":"Provider","metadata":{"name":"`+r.PathValue("name")+`"},"spec":{"tunnel":true}}`+"\n")
	})
	mux.HandleFunc("POST /v1/providers/{name}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		// The session's request body is the agent's frame stream and never
		// ends, so the connect is recorded from its headers alone.
		f.add(seen{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), UA: r.Header.Get("User-Agent")})
		f.mu.Lock()
		h := f.session
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		writeCode(w, api.CodeForbidden, "the subject may not attach this provider")
	})
	f.srv = httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	f.srv.Config.Protocols = protocols
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *serveFake) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.add(seen{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
		UA: r.Header.Get("User-Agent"), ContentType: r.Header.Get("Content-Type"), Body: string(body)})
}

func (f *serveFake) add(s seen) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, s)
}

func (f *serveFake) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.got...)
}

func (f *serveFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = nil
}

// TestServeFlags is spec 014's row for the flags: each missing or
// wrong one is exit 2 with nothing sent, the Provider the flags
// describe is applied through the same PUT lux apply uses with
// spec.tunnel, the dialect, the discovery globs and the labels,
// -no-apply sends no PUT at all, and a refused connect is rendered as
// the refusal it is rather than as a close reason.
func TestServeFlags(t *testing.T) {
	f := newServeFake(t)
	env := map[string]string{"LUX_URL": f.srv.URL, "LUX_TOKEN": canaryToken}
	full := []string{"serve", "--dialect", "openai", "--upstream", "http://127.0.0.1:11434/v1", "--as", "laptop"}

	usage := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no flag at all", []string{"serve"}, env, "lux serve needs -dialect, -upstream, and -as."},
		{"no upstream", []string{"serve", "--dialect", "openai", "--as", "laptop"}, env, "lux serve needs -dialect, -upstream, and -as."},
		{"no as", []string{"serve", "--dialect", "openai", "--upstream", "http://127.0.0.1:1/v1"}, env, "lux serve needs -dialect, -upstream, and -as."},
		{"an unknown dialect", slicesOf(full, "--dialect", "ollama"), env, `-dialect takes openai, anthropic, gemini, or lux, not "ollama".`},
		{"an upstream with no scheme", slicesOf(full, "--upstream", "127.0.0.1:11434"), env, `-upstream takes the runtime's own URL, such as http://127.0.0.1:11434/v1, not "127.0.0.1:11434".`},
		{"an upstream that is not HTTP", slicesOf(full, "--upstream", "unix:///run/llama.sock"), env, `-upstream takes the runtime's own URL, such as http://127.0.0.1:11434/v1, not "unix:///run/llama.sock".`},
		{"no carrier", slicesOf(full, "--carriers", "0"), env, "-carriers is a count of parked streams and is at least 1."},
		{"a label that is not a pair", append(full, "--label", "team"), env, `-label takes k=v, not "team".`},
		{"an argument", append(full, "extra"), env, "lux serve takes no argument."},
		{"no gateway", full, map[string]string{"LUX_TOKEN": canaryToken}, "Set " + EnvURL + " to the gateway's URL."},
		{"no token", full, map[string]string{"LUX_URL": f.srv.URL}, "Set " + EnvToken + " or " + EnvTokenFile + " to a token from your issuer."},
		{"a Key instead of a token", full, map[string]string{"LUX_URL": f.srv.URL, "LUX_KEY": "lux_x"}, EnvKey + " opens a door, not /v1."},
	}
	for _, tc := range usage {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			r := run(t, tc.env, tc.args...)
			if r.code != ExitUsage || !strings.HasPrefix(r.stderr, tc.want) || r.stdout != "" {
				t.Fatalf("exit %d\nstdout %q\nstderr %q\nwant %q", r.code, r.stdout, r.stderr, tc.want)
			}
			if got := f.requests(); len(got) != 0 {
				t.Fatalf("%d request(s) were sent: %+v", len(got), got)
			}
		})
	}

	// The Provider the flags describe, then the connect the fake refuses.
	f.reset()
	r := run(t, env, "serve", "--dialect", "openai", "--upstream", "http://127.0.0.1:11434/v1", "--as", "laptop",
		"--include", "llama*", "--include", "qwen*", "--exclude", "*-embed", "--label", "host=desk", "--carriers", "2")
	if r.code != ExitRefused || !strings.HasPrefix(r.stderr, api.CodeForbidden.Message()+"\n") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	got := f.requests()
	if len(got) != 2 {
		t.Fatalf("%d request(s): %+v", len(got), got)
	}
	put := got[0]
	want := `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider","metadata":{"name":"laptop","labels":{"host":"desk"}},` +
		`"spec":{"dialect":"openai","tunnel":true,"discovery":{"include":["llama*","qwen*"],"exclude":["*-embed"]},"concurrency":0}}`
	if put.Method != http.MethodPut || put.Path != "/v1/providers/laptop" || put.Body != want {
		t.Errorf("the PUT is %s %s\n got %s\nwant %s", put.Method, put.Path, put.Body, want)
	}
	if put.Auth != "Bearer "+canaryToken || put.UA != "lux/1.2.3" {
		t.Errorf("the PUT carried %+v", put)
	}
	if connect := got[1]; connect.Method != http.MethodPost || connect.Path != "/v1/providers/laptop/tunnel" || connect.UA != "lux/1.2.3" {
		t.Errorf("the connect is %+v", connect)
	}
	if !strings.Contains(r.stdout, `"kind":"Provider"`) {
		t.Errorf("the applied Provider was not printed: %q", r.stdout)
	}

	// -v adds the code and the developer's detail, and never the token.
	f.reset()
	r = run(t, env, "serve", "--dialect", "lux", "--upstream", "http://127.0.0.1:11434", "--as", "laptop", "--no-apply", "-v")
	if r.code != ExitRefused || !strings.Contains(r.stderr, "code: forbidden\n") || !strings.Contains(r.stderr, "detail: the subject may not attach this provider\n") {
		t.Fatalf("exit %d\nstderr %q", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, canaryToken) || strings.Contains(r.stdout, canaryToken) {
		t.Fatal("the token was printed")
	}
	got = f.requests()
	if len(got) != 1 || got[0].Method != http.MethodPost {
		t.Fatalf("-no-apply sent %+v", got)
	}
}

// slicesOf is args with one flag's value replaced, so a table row names
// the one flag it is about.
func slicesOf(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for i, a := range out {
		if a == flag {
			out[i+1] = value
			return out
		}
	}
	return append(out, flag, value)
}
