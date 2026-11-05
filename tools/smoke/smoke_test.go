// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package smoke holds the test for release.sh, the live check the release
// pipeline runs against the service after a rollout.
package smoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stub serves what the public base of a healthy luxd serves: the descriptor
// and /version. An empty version answers /version with a page.
func stub(t *testing.T, version string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models/.well-known/lux", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"lux","version":"` + version + `"}`))
	})
	mux.HandleFunc("GET /v1/models/version", func(w http.ResponseWriter, _ *http.Request) {
		if version == "" {
			_, _ = w.Write([]byte("<!doctype html><title>not json</title>\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + version + `","commit":"abc1234"}` + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func script(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(filepath.Dir(file), "release.sh")
}

// run executes the script and returns its output and whether it exited
// zero. It skips when the tools the script needs are not on PATH, as in the
// gate's hermetic run.
func run(t *testing.T, env ...string) (string, bool) {
	t.Helper()
	for _, c := range []string{"bash", "curl", "grep", "mktemp"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("%s is not on PATH, so release.sh cannot run here", c)
		}
	}
	cmd := exec.CommandContext(context.Background(), "bash", script(t))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func TestSmokePassesWhenTheServedVersionMatchesTheTag(t *testing.T) {
	srv := stub(t, "v1.2.3")
	evidence := filepath.Join(t.TempDir(), "smoke.md")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/models", "TAG=v1.2.3", "COMMIT=abc1234", "OUTPUT_MD="+evidence)
	if !ok {
		t.Fatalf("the smoke failed against a matching tag:\n%s", out)
	}
	md, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Release evidence", "- Tag: `v1.2.3`", "- Commit: `abc1234`", "Served version: `v1.2.3`"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("the evidence lacks %q:\n%s", want, md)
		}
	}
}

func TestSmokeFailsOnAVersionThatIsNotTheTag(t *testing.T) {
	srv := stub(t, "v1.2.2")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/models", "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against v1.2.2:\n%s", out)
	}
	if !strings.Contains(out, "version mismatch") {
		t.Errorf("the failure does not name the mismatch:\n%s", out)
	}
}

func TestSmokeFailsOnAVersionThatIsNotJSON(t *testing.T) {
	srv := stub(t, "")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/models", "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against a /version that is not JSON:\n%s", out)
	}
}

func TestSmokeFailsWhenTheServiceIsDown(t *testing.T) {
	srv := stub(t, "v1.2.3")
	url := srv.URL
	srv.Close()
	out, ok := run(t, "BASE_URL="+url+"/v1/models", "TAG=v1.2.3")
	if ok {
		t.Fatalf("the smoke passed against a service that is down:\n%s", out)
	}
	if !strings.Contains(out, "FAIL") {
		t.Errorf("no reason given:\n%s", out)
	}
}

func TestSmokeRefusesWithNoTarget(t *testing.T) {
	out, ok := run(t, "BASE_URL=")
	if ok || !strings.Contains(out, "BASE_URL is required") {
		t.Fatalf("expected a refusal naming BASE_URL, got ok=%v:\n%s", ok, out)
	}
}
