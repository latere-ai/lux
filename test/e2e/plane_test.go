// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/test/conformance"
	"latere.ai/x/lux/test/stubs/issuer"
	"latere.ai/x/lux/test/stubs/provider"
)

// Spec 020 through the whole stack: a platform applies a Key with a
// token from its own issuer, hands it to a sandbox's egress gateway
// rather than to the sandbox, and reads the ledger by the Key's id after
// the run. Every test here drives luxd as a process, so what is asserted
// is what left the gateway.

// keyCache is what these tests set LUX_KEY_CACHE to: the floor, so a
// delete takes effect within a second and the test's deadline is short.
const keyCache = time.Second

// planeStack is a stack with the Key cache at its floor and the
// counters flushed often, which is what lets a test assert a deletion
// and a usage row without waiting on the defaults.
func planeStack(t *testing.T) *stack {
	t.Helper()
	return newStack(t, map[string]string{"LUX_KEY_CACHE": keyCache.String(), "LUX_METERING_FLUSH": "200ms"})
}

// keySpec is one Key manifest with the spec lines the caller adds, which
// is how these tests reach spec.value, spec.ttl, and spec.expiresAt that
// the tier's own builder does not carry.
func keySpec(name, spec string) string {
	return manifestHead + "kind: Key\nmetadata:\n  name: " + name + "\nspec:\n" + spec
}

// applyWith PUTs one manifest under a token of the caller's choosing,
// which is how a Key applied by a service token is told from one applied
// by a person's.
func applyWith(t *testing.T, s *stack, token, kind, name, manifest string) response {
	t.Helper()
	h := bearer(token)
	h.Set("Content-Type", "application/yaml")
	return do(t, http.MethodPut, s.gw.public+"/v1/"+kind+"s/"+name, h, manifest)
}

// mintClaims asks the stub issuer for a token with the claims given.
func mintClaims(t *testing.T, s *stack, claims issuertest.Claims) string {
	t.Helper()
	token, err := issuer.Mint(t.Context(), client, s.stubs.urls["issuer"], claims)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// actorToken is the one-hop token a platform's issuer mints for a
// signed-in person, addressed to the gateway's audience: the console's
// credential of spec 006, POST /actor-tokens with the person's own
// bearer.
func actorToken(t *testing.T, s *stack, person string) string {
	t.Helper()
	parent := mintClaims(t, s, issuertest.Claims{Sub: person})
	body := `{"audience":"` + issuer.Audience + `","ttl_seconds":300}`
	resp := do(t, http.MethodPost, s.stubs.urls["issuer"]+"/actor-tokens", jsonBearer(parent), body)
	if resp.status != http.StatusOK {
		t.Fatalf("POST /actor-tokens = %d %s", resp.status, resp.body)
	}
	token, _ := resp.json(t)["actor_token"].(string)
	if token == "" {
		t.Fatalf("POST /actor-tokens answered no token: %s", resp.body)
	}
	return token
}

// egress stands in for a sandbox's egress gateway: it substitutes the
// Key for the placeholder toward the one host the platform scoped the
// secret to, and forwards every other host's request with the
// placeholder verbatim, which is what makes the placeholder inert
// outside that host.
type egress struct {
	*httptest.Server
	placeholder, key, scoped string
	substitutions            int
}

// newEgress starts the egress gateway in front of scoped.
func newEgress(t *testing.T, placeholder, key, scoped string) *egress {
	t.Helper()
	e := &egress{placeholder: placeholder, key: key, scoped: scoped}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := e.scoped
		header := http.Header{}
		maps.Copy(header, r.Header)
		if bearer, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer "); ok && bearer == e.placeholder {
			header.Set("Authorization", "Bearer "+e.key)
			e.substitutions++
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = header
		resp, err := client.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(e.Close)
	return e
}

// TestE2ESandboxComposition is spec 020's sandbox composition end to
// end: a platform applies a Key with a service token, hands the value to
// an egress gateway scoped to the gateway's host, and starts a workload
// holding a placeholder alone. The workload reaches a door, the request
// is metered against the Key, and the Key's value is in no byte of the
// workload's environment, its file system, or its output.
func TestE2ESandboxComposition(t *testing.T) {
	s := planeStack(t)
	s.fixtures(t)
	service := mintClaims(t, s, issuertest.Claims{Sub: "svc-runs", PrincipalType: "service"})
	resp := applyWith(t, s, service, "key", "run-42", keySpec("run-42", "  models: [\"*\"]\n  ttl: 1h\n"))
	if resp.status != http.StatusCreated {
		t.Fatalf("PUT /v1/keys/run-42 = %d %s", resp.status, resp.body)
	}
	created := resp.json(t)
	value, _ := created["status"].(map[string]any)["value"].(string)
	id, _ := created["status"].(map[string]any)["id"].(string)
	if value == "" || id == "" {
		t.Fatalf("the create answer carries no value or id: %s", resp.body)
	}

	// The platform's secret: the value, scoped to the gateway's host, and
	// a placeholder the sandbox is started with.
	const placeholder = "sandbox-placeholder-0000000000000000"
	gateway := newEgress(t, placeholder, value, s.gw.public)
	elsewhere := newEgress(t, placeholder, value, s.stubs.urls["openai"])
	elsewhere.placeholder = "" // nothing is substituted toward another host

	// The sandbox: a process holding the placeholder, a directory of its
	// own, and no other environment. It reaches a door through the egress
	// gateway and never learns the Key.
	sandboxDir := t.TempDir()
	env := map[string]string{"LUX_BASE_URL": gateway.URL, "LUX_API_KEY": placeholder, "HOME": sandboxDir, "TMPDIR": sandboxDir}
	// The command exits non-zero on any refusal, so a model list at all
	// is the door answering the substituted Key. Which Models it carries
	// is the health job's business and not this test's.
	out := runSandbox(t, sandboxDir, env, "models", "-o", "json")
	if !strings.Contains(out, `"object":"list"`) {
		t.Fatalf("the sandbox's model list: %s", out)
	}

	// The sandbox's own request through the egress gateway, which is what
	// an SDK in it sends, is answered by the provider and metered.
	chat := do(t, http.MethodPost, gateway.URL+"/openai/v1/chat/completions", jsonBearer(placeholder),
		`{"model":"openai-model","messages":[{"role":"user","content":"from the sandbox"}]}`)
	if chat.status != http.StatusOK {
		t.Fatalf("the sandbox's request = %d %s %s", chat.status, chat.code(), chat.body)
	}
	if !strings.Contains(string(chat.body), provider.Content("openai", "stub-openai", "from the sandbox")) {
		t.Fatalf("the sandbox's answer: %s", chat.body)
	}
	if gateway.substitutions < 2 {
		t.Fatalf("the egress gateway substituted %d times, want the workload's and the SDK's", gateway.substitutions)
	}
	records := s.records(t, "run-42")
	if len(records) == 0 {
		t.Fatal("no record was metered against run-42")
	}
	for _, r := range records {
		if key, _ := r["key"].(map[string]any); key == nil || key["id"] != id {
			t.Fatalf("a record is attributed to %v, not to the run's Key", r["key"])
		}
	}

	// The placeholder is inert toward any other host: the provider's own
	// address sees the placeholder and refuses it.
	inert := do(t, http.MethodPost, elsewhere.URL+"/v1/chat/completions", jsonBearer(placeholder), `{"model":"stub-openai","messages":[]}`)
	if inert.status != http.StatusUnauthorized {
		t.Fatalf("the placeholder toward another host = %d %s", inert.status, inert.body)
	}

	// The Key is in no byte the sandbox can read.
	for name, v := range env {
		if strings.Contains(v, value) {
			t.Fatalf("the sandbox's environment carries the Key in %s", name)
		}
	}
	if strings.Contains(out, value) {
		t.Fatalf("the sandbox's output carries the Key: %s", out)
	}
	if where := grepTree(t, sandboxDir, value); where != "" {
		t.Fatalf("the sandbox's file system carries the Key in %s", where)
	}

	// The end of the run: the Key is deleted and the value stops opening
	// the door within the cache window.
	if del := do(t, http.MethodDelete, s.gw.public+"/v1/keys/run-42", bearer(s.token), ""); del.status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/keys/run-42 = %d %s", del.status, del.body)
	}
	refusedWithin(t, s, value, "openai-model")
}

// runSandbox runs the lux command as the sandbox's workload, with the
// environment given and nothing else, in a directory of its own, and
// returns everything it printed.
func runSandbox(t *testing.T, dir string, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), luxBin, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the sandbox's workload: %v\n%s", err, out)
	}
	return string(out)
}

// grepTree names the first file under dir whose bytes carry the needle,
// or the empty string.
func grepTree(t *testing.T, dir, needle string) string {
	t.Helper()
	found := ""
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), needle) {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// refusedWithin asserts the value stops opening the door within the Key
// cache window, and never before the delete.
func refusedWithin(t *testing.T, s *stack, value, model string) {
	t.Helper()
	deadline := time.Now().Add(4 * keyCache)
	for {
		resp := s.chat(t, value, model, "after the run", false)
		if resp.status == http.StatusUnauthorized && resp.code() == "unauthenticated" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the value still answered %d %s more than %s after the delete", resp.status, resp.code(), 4*keyCache)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestE2EOwnerFollowsTheToken: a Key applied with a service token is
// owned by the service account and one applied with an actor token by
// the person, and GET /v1/usage?by=owner attributes each Key's requests
// to its owner.
func TestE2EOwnerFollowsTheToken(t *testing.T) {
	s := planeStack(t)
	s.fixtures(t)
	service := mintClaims(t, s, issuertest.Claims{Sub: "svc-runs", PrincipalType: "service"})
	person := actorToken(t, s, "ada")
	subjectOf := func(sub string) string { return issuer.Subject(s.stubs.urls["issuer"], sub) }

	values := map[string]string{}
	for _, tc := range []struct{ name, token, owner string }{
		{"by-service", service, subjectOf("svc-runs")},
		{"by-person", person, subjectOf("ada")},
	} {
		resp := applyWith(t, s, tc.token, "key", tc.name, keySpec(tc.name, "  models: [\"*\"]\n"))
		if resp.status != http.StatusCreated {
			t.Fatalf("PUT /v1/keys/%s = %d %s", tc.name, resp.status, resp.body)
		}
		status, _ := resp.json(t)["status"].(map[string]any)
		if status["owner"] != tc.owner {
			t.Errorf("%s is owned by %v, want %s", tc.name, status["owner"], tc.owner)
		}
		values[tc.owner], _ = status["value"].(string)
		if resp := s.chat(t, values[tc.owner], "openai-model", tc.name, false); resp.status != http.StatusOK {
			t.Fatalf("%s through the door = %d %s %s", tc.name, resp.status, resp.code(), resp.body)
		}
	}

	var rows []map[string]any
	eventually(t, "the usage rows reaching the store", 15*time.Second, func() bool {
		resp := do(t, http.MethodGet, s.gw.public+"/v1/usage?by=owner", bearer(s.token), "")
		if resp.status != http.StatusOK {
			t.Fatalf("GET /v1/usage?by=owner = %d %s", resp.status, resp.body)
		}
		rows = nil
		items, _ := resp.json(t)["items"].([]any)
		for _, it := range items {
			rows = append(rows, it.(map[string]any))
		}
		return len(rows) >= 2
	})
	byOwner := map[string]int64{}
	for _, row := range rows {
		dimensions, _ := row["dimensions"].(map[string]any)
		owner, _ := dimensions["owner"].(string)
		requests, _ := row["requests"].(float64)
		byOwner[owner] += int64(requests)
	}
	for owner := range values {
		if byOwner[owner] != 1 {
			t.Errorf("GET /v1/usage?by=owner attributes %d requests to %s, want 1: %v", byOwner[owner], owner, byOwner)
		}
	}
}

// TestE2EPlatformCredentialAsKey: a Key created with the platform's own
// credential as spec.value opens a door by that string, the issuer that
// signed it is never called, the Key's expiresAt is the only expiry the
// door knows, and the value is unauthenticated within LUX_KEY_CACHE of
// the delete.
func TestE2EPlatformCredentialAsKey(t *testing.T) {
	s := planeStack(t)
	s.fixtures(t)
	// The platform's own credential: a token its issuer signed, whose exp
	// has already passed, so what the door honours can only be the Key.
	expired := mintClaims(t, s, issuertest.Claims{Sub: "dev-1", Exp: time.Now().Add(-time.Hour).Unix()})
	resp := applyWith(t, s, s.token, "key", "developer", keySpec("developer", "  models: [\"*\"]\n  value: "+expired+"\n"))
	if resp.status != http.StatusCreated {
		t.Fatalf("PUT /v1/keys/developer = %d %s", resp.status, resp.body)
	}
	status, _ := resp.json(t)["status"].(map[string]any)
	if status["value"] != nil {
		t.Error("the create answer echoes a supplied value")
	}
	prefix, _ := status["prefix"].(string)
	if !strings.HasPrefix(prefix, "sup_") {
		t.Errorf("status.prefix is %q, want the supplied value's sup_ handle", prefix)
	}
	if strings.HasPrefix(expired, prefix) {
		t.Errorf("status.prefix %q is the token's own first characters, which every token of that issuer shares", prefix)
	}

	// The door opens by the exact bytes, and the issuer is not called.
	if del := do(t, http.MethodDelete, s.stubs.urls["issuer"]+"/requests", nil, ""); del.status != http.StatusNoContent {
		t.Fatalf("DELETE the issuer's record = %d", del.status)
	}
	if chat := s.chat(t, expired, "openai-model", "hello", false); chat.status != http.StatusOK {
		t.Fatalf("the platform's credential at the door = %d %s %s", chat.status, chat.code(), chat.body)
	}
	if chat := s.chat(t, expired+"x", "openai-model", "hello", false); chat.status != http.StatusUnauthorized {
		t.Fatalf("a value with one byte more = %d, want unauthenticated", chat.status)
	}
	if got := do(t, http.MethodGet, s.stubs.urls["issuer"]+"/requests", nil, ""); strings.TrimSpace(string(got.body)) != "[]" && strings.TrimSpace(string(got.body)) != "null" {
		t.Fatalf("the issuer was called while the door was opened: %s", got.body)
	}
	// The same token on the control plane is no credential at all.
	if self := do(t, http.MethodGet, s.gw.public+"/v1/self", bearer(expired), ""); self.status != http.StatusUnauthorized {
		t.Fatalf("the expired token on /v1 = %d, want unauthenticated", self.status)
	}

	// The Key's expiresAt is the only expiry the door knows: a second
	// credential whose own exp is far away stops working when the Key does.
	live := mintClaims(t, s, issuertest.Claims{Sub: "dev-2", Exp: time.Now().Add(24 * time.Hour).Unix()})
	expiresAt := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)
	resp = applyWith(t, s, s.token, "key", "short", keySpec("short", "  models: [\"*\"]\n  expiresAt: "+expiresAt+"\n  value: "+live+"\n"))
	if resp.status != http.StatusCreated {
		t.Fatalf("PUT /v1/keys/short = %d %s", resp.status, resp.body)
	}
	if chat := s.chat(t, live, "openai-model", "before", false); chat.status != http.StatusOK {
		t.Fatalf("the short Key before its expiry = %d %s", chat.status, chat.code())
	}
	eventually(t, "the Key expiring at its own expiresAt", 15*time.Second, func() bool {
		return s.chat(t, live, "openai-model", "after", false).code() == "key_expired"
	})

	// Revoking at the gateway is the delete, and it takes effect within
	// the cache window.
	if del := do(t, http.MethodDelete, s.gw.public+"/v1/keys/developer", bearer(s.token), ""); del.status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/keys/developer = %d %s", del.status, del.body)
	}
	refusedWithin(t, s, expired, "openai-model")
}

// TestE2ERunKeyDeletionLeavesTheLedger: deleting the Key at the end of a
// run refuses the next request within LUX_KEY_CACHE while the run's
// usage stays readable by the Key's id, which is the ledger line for
// that run.
func TestE2ERunKeyDeletionLeavesTheLedger(t *testing.T) {
	s := planeStack(t)
	s.fixtures(t)
	resp := applyWith(t, s, s.token, "key", "run-x", keySpec("run-x", "  models: [\"*\"]\n"))
	if resp.status != http.StatusCreated {
		t.Fatalf("PUT /v1/keys/run-x = %d %s", resp.status, resp.body)
	}
	status, _ := resp.json(t)["status"].(map[string]any)
	value, _ := status["value"].(string)
	id, _ := status["id"].(string)
	for range 3 {
		if chat := s.chat(t, value, "openai-model", "during the run", false); chat.status != http.StatusOK {
			t.Fatalf("a request of the run = %d %s %s", chat.status, chat.code(), chat.body)
		}
	}
	if del := do(t, http.MethodDelete, s.gw.public+"/v1/keys/run-x", bearer(s.token), ""); del.status != http.StatusNoContent {
		t.Fatalf("DELETE /v1/keys/run-x = %d %s", del.status, del.body)
	}
	refusedWithin(t, s, value, "openai-model")
	if read := do(t, http.MethodGet, s.gw.public+"/v1/keys/"+id, bearer(s.token), ""); read.status != http.StatusNotFound {
		t.Fatalf("the deleted Key reads %d, want not_found", read.status)
	}

	// The ledger: the aggregate rows and the records are still readable by
	// the id, which is why a run's line survives its Key.
	var requests int64
	eventually(t, "the run's usage staying readable by the Key's id", 15*time.Second, func() bool {
		resp := do(t, http.MethodGet, s.gw.public+"/v1/usage?key="+id, bearer(s.token), "")
		if resp.status != http.StatusOK {
			t.Fatalf("GET /v1/usage?key=%s = %d %s", id, resp.status, resp.body)
		}
		requests = 0
		items, _ := resp.json(t)["items"].([]any)
		for _, it := range items {
			n, _ := it.(map[string]any)["requests"].(float64)
			requests += int64(n)
		}
		return requests >= 3
	})
	// The count is the run's three and whatever the cache admitted between
	// the delete and the first refusal, which is why the assertion is a
	// floor and not a figure.
	records := do(t, http.MethodGet, s.gw.public+"/v1/requests?key="+id, bearer(s.token), "")
	items, _ := records.json(t)["items"].([]any)
	if records.status != http.StatusOK || len(items) == 0 {
		t.Fatalf("GET /v1/requests?key=%s = %d with %d records", id, records.status, len(items))
	}
}

// TestE2EOneCredentialKindPerHop is the two credential tables of spec
// 020 over the capture of one run: every hop carries the kind the table
// names and no other, the gateway verifies a supplied value by its hash
// and never as a token, and no plane verifies a credential another plane
// minted.
func TestE2EOneCredentialKindPerHop(t *testing.T) {
	// The authorizer hop is read through a recorder in front of the stub,
	// because the stub records the envelope and not the bearer it came
	// under, so the gateway is started with the recorder as its
	// authorizer rather than the stub itself.
	seen := &recordedHeaders{}
	stubs := startStubs(t)
	recorder := httptest.NewServer(seen.proxy(t, stubs.urls["authorizer"]))
	t.Cleanup(recorder.Close)
	gw := startLuxd(t, serverEnv(t, stubs, map[string]string{"LUX_AUTHORIZER_URL": recorder.URL, "LUX_KEY_CACHE": keyCache.String()}))
	s := &stack{stubs: stubs, gw: gw, token: mint(t, stubs, "dev")}
	key := s.fixtures(t)

	// The platform's credential, registered as a Key's spec.value: a
	// string the gateway matches by hash and decodes nothing of, here one
	// whose signature is not the issuer's at all.
	forged := mintClaims(t, s, issuertest.Claims{Sub: "dev-3"}) + "-not-the-issuers-signature"
	if resp := applyWith(t, s, s.token, "key", "developer", keySpec("developer", "  models: [\"*\"]\n  value: "+forged+"\n")); resp.status != http.StatusCreated {
		t.Fatalf("PUT /v1/keys/developer = %d %s", resp.status, resp.body)
	}
	if chat := s.chat(t, forged, "openai-model", "by the hash", false); chat.status != http.StatusOK {
		t.Fatalf("a supplied value whose signature is broken = %d %s; the door reads bytes and not a token", chat.status, chat.code())
	}
	if chat := s.chat(t, key, "openai-model", "hi", false); chat.status != http.StatusOK {
		t.Fatalf("the minted Key = %d %s %s", chat.status, chat.code(), chat.body)
	}

	// A token no Key was created with opens no door, and a Key opens no
	// control plane.
	if chat := s.chat(t, s.token, "openai-model", "hi", false); chat.status != http.StatusUnauthorized {
		t.Fatalf("an issuer token at a door = %d, want unauthenticated", chat.status)
	}
	if self := do(t, http.MethodGet, s.gw.public+"/v1/self", bearer(key), ""); self.status != http.StatusUnauthorized {
		t.Fatalf("a Key on /v1 = %d, want unauthenticated", self.status)
	}

	// The authorizer hop carries its own bearer and nothing else.
	bearers := seen.bearers()
	if len(bearers) == 0 {
		t.Fatal("the authorizer was never called during the applies")
	}
	for _, b := range bearers {
		if b != stub.DefaultToken {
			t.Errorf("the authorizer hop carried %q, want LUX_AUTHORIZER_TOKEN alone", b)
		}
	}
	// The provider hop carries the Provider's credential and nothing else.
	for _, got := range s.received(t, "openai") {
		if auth := got.Header.Get("Authorization"); auth != "Bearer "+provider.DefaultCredential {
			t.Errorf("the provider hop carried %q, want the Provider's credential", auth)
		}
		recorded := marshal(got)
		for what, credential := range map[string]string{"the minted Key": key, "the supplied value": forged, "the caller's token": s.token} {
			if strings.Contains(recorded, credential) {
				t.Errorf("%s reached the provider", what)
			}
		}
	}
}

// TestE2EPlaneDocAuthorizerConforms is spec 020's first acceptance row
// through the stack: the authorizer docs/plane.md prints, compiled and
// run beside luxd as the installation's LUX_AUTHORIZER_URL, denies the
// probe id and carries the whole conformance suite, the identity and
// manifest groups among them. The suite's tokens carry the plan claim
// the endpoint reads, which is the platform's own claim and no
// gateway's.
func TestE2EPlaneDocAuthorizerConforms(t *testing.T) {
	const token = "the-platform-authorizer-token"
	s := startStubs(t)
	port := strconv.Itoa(freePort(t))
	decider := startProcess(t, authorizerBin, nil, "-addr", "127.0.0.1:"+port, "-token", token)
	waitFor(t, "the authorizer's output", 10*time.Second, decider.errOut.String, "deciding at")

	// luxd check's own question, asked of the endpoint before the suite:
	// the reserved id is denied whatever the subject.
	probe := do(t, http.MethodPost, "http://127.0.0.1:"+port, jsonBearer(token),
		`{"subject":"http://localhost|dev","action":"key.read","resource":{"kind":"Key","id":"`+authz.ProbeID+`","owner":"http://localhost|dev"}}`)
	if probe.status != http.StatusOK || !strings.Contains(string(probe.body), `"allow":false`) {
		t.Fatalf("the probe = %d %s", probe.status, probe.body)
	}

	gw := startLuxd(t, serverEnv(t, s, map[string]string{
		"LUX_AUTHORIZER_URL": "http://127.0.0.1:" + port, "LUX_AUTHORIZER_TOKEN": token,
	}))
	issuerURL := strings.TrimRight(s.urls["issuer"], "/")
	conformance.Run(t, conformance.Config{
		URL:     gw.public,
		Subject: issuerURL + "|dev",
		Token: func(subject string) (string, bool) {
			i := strings.LastIndexByte(subject, '|')
			if i < 0 || strings.TrimRight(subject[:i], "/") != issuerURL {
				return "", false
			}
			return mintClaims(t, &stack{stubs: s}, issuertest.Claims{
				Sub: subject[i+1:], Extra: map[string]any{"plan": "admin"},
			}), true
		},
	})
}

// TestE2EPlaneDocCommand runs the conformance command docs/plane.md
// prints, as it is printed, against the installation make run gives a
// clean clone: the example host and the token command are replaced by
// this one's, and nothing else. A command that has drifted from the
// suite's entry point, or a document that prints one no shell can run,
// fails here.
func TestE2EPlaneDocCommand(t *testing.T) {
	requireOnPath(t, "sh")
	out, _ := makeRun(t, "run")
	env := exports(out)
	if env["LUX_URL"] == "" || env["LUX_TOKEN"] == "" {
		t.Fatalf("make run printed no URL or token:\n%s", out)
	}
	script := docCommand(t)
	script = strings.ReplaceAll(script, "https://api.example.com", env["LUX_URL"])
	script = strings.ReplaceAll(script, "$(platform-token)", env["LUX_TOKEN"])
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.Dir = root
	cmd.Env = os.Environ()
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the document's command against make run: %v\n%s\n%s", err, script, data)
	}
	if !strings.Contains(string(data), "--- PASS: TestContract") {
		t.Fatalf("the document's command ran no suite:\n%s", data)
	}
}

// docCommand is the shell block of docs/plane.md, which is the one
// command the document tells a platform to put in its pipeline.
func docCommand(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", "plane.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(data), "```sh\n")
	if !ok {
		t.Fatal("docs/plane.md carries no shell block")
	}
	block, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatal("docs/plane.md's shell block is not closed")
	}
	return block
}

// recordedHeaders is the capture of one hop: the headers every request
// through the recorder carried.
type recordedHeaders struct {
	headers []http.Header
}

// proxy records each request's headers and forwards it to the endpoint.
func (h *recordedHeaders) proxy(t *testing.T, target string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.headers = append(h.headers, r.Header.Clone())
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := client.Do(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

// bearers is every Authorization header the hop carried, without its
// scheme.
func (h *recordedHeaders) bearers() []string {
	out := make([]string, 0, len(h.headers))
	for _, header := range h.headers {
		bearer, _ := strings.CutPrefix(header.Get("Authorization"), "Bearer ")
		out = append(out, bearer)
	}
	return out
}
