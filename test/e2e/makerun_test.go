// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/test/stubs/provider"
	"latere.ai/x/lux/test/stubs/sink"
)

// The two make targets need make and curl beside the toolchain, which
// every runner of the e2e job has; a machine without them fails these
// two rows rather than skipping them, as the tier's rule says.
func requireOnPath(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("%s is not on PATH, and make run needs it", name)
		}
	}
}

// freePortRange finds a base port whose nine consecutive ports are free,
// the range make run derives from RUN_PORT.
func freePortRange(t *testing.T) int {
	t.Helper()
	for range 20 {
		base := freePort(t)
		free := true
		for i := 1; i < 9; i++ {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(base+i))
			if err != nil {
				free = false
				break
			}
			_ = ln.Close()
		}
		if free {
			return base
		}
	}
	t.Fatal("no free range of nine ports")
	return 0
}

// makeRun runs one make target at the module root with its own out
// directory and port range, and stops the stack at the end.
func makeRun(t *testing.T, target string) (out string, base int) {
	t.Helper()
	requireOnPath(t, "make", "curl")
	base = freePortRange(t)
	outDir := t.TempDir()
	run := func(target string) string {
		cmd := exec.Command("make", "--no-print-directory", "-C", root, target, "RUN_PORT="+strconv.Itoa(base), "OUT_DIR="+outDir)
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make %s: %v\n%s\nluxd.log:\n%s", target, err, data, readFile(filepath.Join(outDir, "run", "luxd.log")))
		}
		return string(data)
	}
	t.Cleanup(func() { run("run-down") })
	return run(target), base
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

var export = regexp.MustCompile(`(?m)^export (LUX_[A-Z_]+)=(\S+)$`)

func exports(out string) map[string]string {
	m := map[string]string{}
	for _, match := range export.FindAllStringSubmatch(out, -1) {
		m[match[1]] = match[2]
	}
	return m
}

// TestE2EMakeRun: make run on a clean clone prints the three exports, a
// request through the /openai door with the printed Key returns a stub
// answer, produces one usage record, and the applies delivered events to
// the sink.
func TestE2EMakeRun(t *testing.T) {
	out, base := makeRun(t, "run")
	env := exports(out)
	for _, name := range []string{"LUX_URL", "LUX_TOKEN", "LUX_KEY"} {
		if env[name] == "" {
			t.Fatalf("make run printed no export %s:\n%s", name, out)
		}
	}
	if !strings.HasPrefix(env["LUX_KEY"], "lux_") || !strings.Contains(out, "/openai/v1") {
		t.Fatalf("exports = %v\n%s", env, out)
	}
	throughTheDoor(t, env["LUX_URL"], env["LUX_KEY"])
	records := do(t, http.MethodGet, env["LUX_URL"]+"/v1/requests", bearer(env["LUX_TOKEN"]), "")
	items, _ := records.json(t)["items"].([]any)
	if records.status != http.StatusOK || len(items) != 1 {
		t.Fatalf("GET /v1/requests = %d with %d records: %s", records.status, len(items), records.body)
	}
	if cost := items[0].(map[string]any)["cost"].(map[string]any); cost["amount"] != float64(450) || cost["currency"] != "USD" {
		t.Fatalf("cost = %v, want 450 micro-units of USD as the example says", cost)
	}
	// The worker delivers on its poll, so the applies' events arrive
	// within a second or two of make run returning.
	sinkURL := "http://127.0.0.1:" + strconv.Itoa(base+8)
	eventually(t, "the applies' events reaching the sink", 15*time.Second, func() bool {
		var events []sink.Event
		return json.Unmarshal(do(t, http.MethodGet, sinkURL+"/_events", nil, "").body, &events) == nil && len(events) >= 10
	})
	if resp := do(t, http.MethodGet, sinkURL+"/_events?invalid=1", nil, ""); string(resp.body) != "[]" {
		t.Fatalf("rejected deliveries: %s", resp.body)
	}
}

// throughTheDoor sends the example's request through the /openai door
// with the printed Key and asserts the stub's answer. The first request
// after start may find a Provider the probe has not yet published as
// reachable, so it is retried until the health job's next tick has run.
func throughTheDoor(t *testing.T, url, key string) {
	t.Helper()
	var last response
	eventually(t, "the /openai door answering with the printed Key", 45*time.Second, func() bool {
		last = do(t, http.MethodPost, url+"/openai/v1/chat/completions", jsonBearer(key), `{"model":"stub-openai","messages":[{"role":"user","content":"hello"}]}`)
		if last.status == http.StatusOK {
			return true
		}
		t.Logf("the door = %d %s (%s); retrying", last.status, last.code(), last.header.Get("Lux-Error-Detail"))
		return false
	})
	if !strings.Contains(string(last.body), provider.Content("openai", "stub-openai", "hello")) {
		t.Fatalf("the door's answer: %s", last.body)
	}
}

// TestE2EMakeRunFileMode: make run-file serves the same door from the
// manifest directory with the generated STUB_DEV_KEY, the control plane
// on the internal listener refuses a PUT with read_only, and the public
// listener answers not_found under /v1.
func TestE2EMakeRunFileMode(t *testing.T) {
	out, base := makeRun(t, "run-file")
	env := exports(out)
	if env["LUX_URL"] == "" || !strings.HasPrefix(env["LUX_KEY"], "lux_") || env["LUX_TOKEN"] != "" {
		t.Fatalf("exports = %v\n%s", env, out)
	}
	throughTheDoor(t, env["LUX_URL"], env["LUX_KEY"])
	internal := "http://127.0.0.1:" + strconv.Itoa(base+1)
	if !strings.Contains(out, internal+"/v1") {
		t.Fatalf("make run-file did not name the internal control plane:\n%s", out)
	}
	h := http.Header{"Content-Type": {"application/yaml"}}
	if resp := do(t, http.MethodPut, internal+"/v1/providers/x", h, providerYAML("x", "openai", "https://api.example.com/v1", "k", false)); resp.status != http.StatusMethodNotAllowed || resp.code() != "read_only" || resp.header.Get("Allow") != "GET" {
		t.Fatalf("PUT on the internal control plane = %d %s Allow %q", resp.status, resp.code(), resp.header.Get("Allow"))
	}
	if resp := do(t, http.MethodGet, internal+"/v1/keys", nil, ""); resp.status != http.StatusOK || !strings.Contains(string(resp.body), `"name":"dev"`) {
		t.Fatalf("GET /v1/keys on the internal listener = %d %s", resp.status, resp.body)
	}
	if resp := do(t, http.MethodPut, env["LUX_URL"]+"/v1/providers/x", h, providerYAML("x", "openai", "https://api.example.com/v1", "k", false)); resp.status != http.StatusNotFound || resp.code() != "not_found" {
		t.Fatalf("PUT under /v1 on the public listener = %d %s", resp.status, resp.code())
	}
}
