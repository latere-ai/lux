// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/api"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/manifest"
)

const kek = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

// stubProvider is a provider on loopback speaking the openai dialect:
// the models route the health probe and discovery read, and one chat
// completion, counting what reached it and refusing a request without
// the credential the gateway holds.
type stubProvider struct {
	srv   *httptest.Server
	calls atomic.Int64
	auth  atomic.Value
}

func newStubProvider(t *testing.T) *stubProvider {
	t.Helper()
	p := &stubProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		p.auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5","object":"model","created":0,"owned_by":"stub"}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		p.auth.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+canaryCred {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"gpt-5","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// gatewayHarness is a whole gateway in process, wired as cmd/luxd wires
// it: the memory store, a stub issuer, a stub authorizer, the Key
// cache, the Limiter, the Recorder, the health job, the doors, and the
// control plane, behind one listener.
type gatewayHarness struct {
	url   string
	token string
}

func startGateway(t *testing.T) *gatewayHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st := memory.New()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	authorizer := stub.New(t)
	keys, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, auth.Options{
		Issuers: []string{iss.URL()}, Audiences: []string{"lux"},
		AuthorizerURL: authorizer.URL(), AuthorizerToken: authorizer.Token(), AuthorizerTimeout: 2 * time.Second,
		HTTP: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The gateway is published at localhost and the stub listens at
	// 127.0.0.1, because Resolve's loop check compares host names alone and
	// would take a stub on the same address for the gateway itself.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	base, _ := url.Parse("http://localhost:" + port)
	reg := metrics.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	defaults := manifest.Defaults{Timeout: 10 * time.Minute}
	keyCache := serve.NewKeyCache(serve.KeyCacheOptions{Store: st, TTL: time.Second, Tail: 20 * time.Millisecond, Metrics: reg, Logger: logger})
	credentials := &serve.StoreCredentials{Credentials: st.Credentials(), Keys: keys}
	limiter := serve.NewLimiter(serve.LimiterOptions{Store: st, Budgets: keyCache, Defaults: defaults, Flush: 100 * time.Millisecond, Logger: logger})
	catalog := &serve.Catalog{Objects: st.Objects()}
	recorder := serve.NewRecorder(serve.RecorderOptions{Store: st, Catalog: catalog, Limiter: limiter, Metrics: reg, Flush: 100 * time.Millisecond, Logger: logger})
	clients := gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: true, Version: "test"})
	healthJob := serve.NewHealth(serve.HealthOptions{Store: st, Clients: clients, Credentials: credentials, Interval: 50 * time.Millisecond, Logger: logger})
	var jobs sync.WaitGroup
	jobs.Go(func() { healthJob.Run(ctx) })
	jobs.Go(func() { keyCache.Run(ctx) })
	jobs.Go(func() { limiter.Run(ctx) })
	jobs.Go(func() { recorder.Run(ctx) })
	doors := gateway.New(gateway.Options{
		Keys: keyCache, Catalog: catalog, Credentials: credentials,
		Router:  gateway.NewTargetRouter(gateway.RouterOptions{Catalog: catalog, Health: healthJob.View, Metrics: reg}),
		Limiter: limiter, Recorder: recorder, Clients: clients, Health: healthJob, Metrics: reg, Version: "test",
	})
	control := api.New(api.Options{
		Store: st, Auth: a, Authorizer: a.Authorizer(&serve.ObjectOwners{Objects: st.Objects()}),
		PublicURL: base, Version: "0.1.0-test", RequestsPerMinute: 600, Defaults: defaults,
		AllowPrivateUpstreams: true, Keys: keys, Clients: clients, Metrics: reg, Logger: logger,
	})
	mux := http.NewServeMux()
	for _, d := range []string{"openai", "anthropic", "gemini", "lux"} {
		mux.Handle("/"+d, doors)
		mux.Handle("/"+d+"/", doors)
	}
	mux.Handle("/.well-known/lux", control)
	mux.Handle("/v1/", control)
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		cancel()
		jobs.Wait()
		_ = st.Close()
	})
	return &gatewayHarness{url: base.String(), token: iss.Mint(issuertest.Claims{Sub: "agent"})}
}

// skillManifests is the one YAML block of the skill that holds the four
// manifests, with the example Provider pointed at the stub.
func skillManifests(t *testing.T, upstream string) string {
	t.Helper()
	_, body := readSkill(t)
	for _, m := range fenced.FindAllStringSubmatch(body, -1) {
		if m[1] == "yaml" && strings.Contains(m[2], "kind: Provider") {
			return strings.Replace(m[2], "baseURL: https://api.example.com/v1", "baseURL: "+upstream+"/v1", 1)
		}
	}
	t.Fatal("the skill has no YAML block with a Provider")
	return ""
}

// TestAgentWithOnlyTheSkill is spec 014's row at the level the tree
// allows before spec 015's stub binaries: an agent given
// skills/lux/SKILL.md and the two variables applies the skill's own
// manifests, a Provider pointed at a stub on loopback, a Model, a
// Budget, and a Key under it, reads them back, asks the door which
// models the Key may call, and sends one request through the door with
// the Key, which reaches the stub with the credential the gateway holds
// and never the Key.
func TestAgentWithOnlyTheSkill(t *testing.T) {
	g := startGateway(t)
	provider := newStubProvider(t)
	env := map[string]string{"LUX_URL": g.url, "LUX_TOKEN": g.token, "OPENAI_API_KEY": canaryCred}
	stack := write(t, "stack.yaml", skillManifests(t, provider.srv.URL))

	// The skill's apply line, with the credential from the environment.
	r := run(t, env, "apply", "-f", stack, "--credential-from-env", "OPENAI_API_KEY", "-v")
	if r.code != 0 {
		t.Fatalf("apply: exit %d\nstdout %s\nstderr %s", r.code, r.stdout, r.stderr)
	}
	var key string
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	kinds := []string{}
	for dec.More() {
		var obj struct {
			Kind   string `json:"kind"`
			Status struct {
				Value string `json:"value"`
				State string `json:"state"`
			} `json:"status"`
		}
		if err := dec.Decode(&obj); err != nil {
			t.Fatalf("stdout is not a stream of objects: %v\n%s", err, r.stdout)
		}
		kinds = append(kinds, obj.Kind)
		if obj.Kind == "Key" {
			key = obj.Status.Value
			if obj.Status.State != "Active" {
				t.Fatalf("the Key is %s", obj.Status.State)
			}
		}
	}
	if strings.Join(kinds, ",") != "Provider,Model,Budget,Key" || !strings.HasPrefix(key, "lux_") {
		t.Fatalf("applied %v, key %q", kinds, key)
	}
	if strings.Contains(r.stdout, canaryCred) || strings.Contains(r.stderr, canaryCred) {
		t.Fatal("the credential was printed")
	}

	// Reading back: the Key without its value, the list as a table, the
	// caller's identity.
	r = run(t, env, "get", "key", "research-agent")
	if r.code != 0 || strings.Contains(r.stdout, key) || !strings.Contains(r.stdout, `"budget":{"name":"research"`) {
		t.Fatalf("get key: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = run(t, env, "list", "keys", "-o", "table")
	if r.code != 0 || !strings.Contains(r.stdout, "research-agent") || !strings.Contains(r.stdout, "Active") || strings.Contains(r.stdout, key) {
		t.Fatalf("list keys: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = run(t, env, "get", "provider", "openai", "-o", "wide")
	if r.code != 0 || !strings.Contains(r.stdout, "set v1") || strings.Contains(r.stdout, canaryCred) {
		t.Fatalf("get provider: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = run(t, env, "whoami")
	if r.code != 0 || !strings.Contains(r.stdout, `"sub":"agent"`) {
		t.Fatalf("whoami: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}

	// The door: the Key, and only the Key, lists the model once the health
	// job has published its availability.
	door := map[string]string{"LUX_URL": g.url, "LUX_KEY": key}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r = run(t, door, "models")
		if r.code == 0 && strings.Contains(r.stdout, `"id":"gpt-5"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lux models: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r := run(t, env, "models"); r.code != 2 || !strings.Contains(r.stderr, EnvKey) {
		t.Fatalf("lux models with the token: exit %d %q", r.code, r.stderr)
	}
	if r := run(t, door, "get", "key", "research-agent"); r.code != 2 || !strings.Contains(r.stderr, EnvToken) {
		t.Fatalf("lux get with the Key: exit %d %q", r.code, r.stderr)
	}

	// One request through the openai door with the Key, as an SDK sends
	// it: it reaches the stub carrying the Provider's credential.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, g.url+"/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"content":"hello"`) {
		t.Fatalf("door: %d %s", resp.StatusCode, body)
	}
	if provider.calls.Load() != 1 || provider.auth.Load() != "Bearer "+canaryCred {
		t.Fatalf("the stub saw %d calls with %q", provider.calls.Load(), provider.auth.Load())
	}

	// The usage of that request is readable, and the deletes close the
	// loop in dependency order.
	deadline = time.Now().Add(5 * time.Second)
	for {
		r = run(t, env, "requests", "--key", "research-agent")
		if r.code == 0 && strings.Contains(r.stdout, `"model":{"name":"gpt-5"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lux requests: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r := run(t, env, "delete", "budget", "research"); r.code != 1 || !strings.HasPrefix(r.stderr, api.CodeBudgetInUse.Message()) {
		t.Fatalf("delete an in-use budget: exit %d %q", r.code, r.stderr)
	}
	for _, args := range [][]string{{"delete", "key", "research-agent"}, {"delete", "budget", "research"}, {"delete", "model", "gpt-5"}, {"delete", "provider", "openai"}} {
		if r := run(t, env, args...); r.code != 0 || r.stdout != "" {
			t.Fatalf("%v: exit %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if r := run(t, env, "get", "key", "research-agent"); r.code != 1 || r.stderr != api.CodeNotFound.Message()+"\n" {
		t.Fatalf("after delete: exit %d %q", r.code, r.stderr)
	}

	// The token file path works against the real server too.
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(g.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := run(t, map[string]string{"LUX_URL": g.url, "LUX_TOKEN_FILE": tokenFile}, "whoami", "-o", "yaml"); r.code != 0 || !strings.Contains(r.stdout, "sub: agent\n") {
		t.Fatalf("whoami through a token file: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}
