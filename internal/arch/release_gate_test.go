// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func releaseCommand(t *testing.T, job, name string) string {
	t.Helper()
	w, _ := readWorkflow(t, "release.yml")
	for _, step := range w.Jobs[job].Steps {
		if step.Name == name && step.Run != "" {
			return step.Run
		}
	}
	t.Fatalf("release workflow has no %s step %q", job, name)
	return ""
}

// Run the workflow's command, not a second implementation of its gate. The
// candidate answers HTTP successfully but violates the discovery contract.
func TestReleaseConformanceCommandRejectsNonconformantCandidate(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"not":"a Lux discovery document"}`))
	}))
	t.Cleanup(candidate.Close)
	t.Setenv("LUX_TEST_URL", candidate.URL)
	for _, name := range []string{"LUX_TEST_TOKEN", "LUX_TEST_SUBJECT", "LUX_TEST_ISSUER_URL", "LUX_TEST_STUBS_URL", "LUX_TEST_INTERNAL_URL"} {
		t.Setenv(name, "")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", releaseCommand(t, "conformance", "TestContract against the candidate"))
	cmd.Dir = root(t)
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "--- FAIL: TestContract") || strings.Contains(string(out), "[build failed]") {
		t.Fatalf("candidate did not fail the actual conformance assertion: %v\n%s", err, out)
	}
}

// A red verify response must stop the exact gate-green script before it reaches
// a retry. The fake gh is confined to this child process and never contacts GitHub.
func TestReleaseVerifyCommandRejectsRedCommit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	dir := t.TempDir()
	for name, script := range map[string]string{"gh": "#!/bin/sh\nprintf '%s\\n' failure\n", "sleep": "#!/bin/sh\nexit 99\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_REPOSITORY", "example/gateway")
	t.Setenv("GITHUB_SHA", "red-fixture")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", releaseCommand(t, "gate-green", "Wait for the verify run on this commit and require success"))
	cmd.Dir = root(t)
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "verify concluded 'failure' on red-fixture") {
		t.Fatalf("red verify result did not stop the release gate: %v\n%s", err, out)
	}
}

func TestReleaseFailuresCannotBeIgnoredByPublish(t *testing.T) {
	w, _ := readWorkflow(t, "release.yml")
	for _, name := range []string{"gate-green", "build", "conformance", "publish"} {
		job := w.Jobs[name]
		if job.If != "" && job.If != "success()" {
			t.Errorf("%s overrides the successful-dependencies gate with %q", name, job.If)
		}
		if job.ContinueOnError != nil && job.ContinueOnError != false {
			t.Errorf("%s can ignore a failed gate", name)
		}
		for _, step := range job.Steps {
			if step.ContinueOnError != nil && step.ContinueOnError != false {
				t.Errorf("%s step %q can ignore failure", name, step.Name)
			}
		}
	}
	needs, ok := w.Jobs["publish"].Needs.([]any)
	if !ok || !slices.Contains(needs, any("build")) || !slices.Contains(needs, any("conformance")) {
		t.Fatal("publish must depend on both build and conformance")
	}
}
