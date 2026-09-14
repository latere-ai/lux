// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/api"
)

// TestRunWritesTheDocument: the command writes the generated YAML to the
// path -o names, creating the directory, and says what it wrote.
func TestRunWritesTheDocument(t *testing.T) {
	out := filepath.Join(t.TempDir(), "api", "openapi.yaml")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-o", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := api.OpenAPIYAML(); !bytes.Equal(got, want) {
		t.Error("the file differs from the generator's output")
	}
	if !strings.HasPrefix(stdout.String(), "apidoc: wrote "+out) {
		t.Errorf("stdout %q", stdout.String())
	}
}

func TestRunRefusesABadFlagAndAnUnwritablePath(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"-no-such-flag"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("a bad flag: exit %d", code)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := run([]string{"-o", filepath.Join(file, "openapi.yaml")}, &bytes.Buffer{}, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "apidoc: ") {
		t.Errorf("a path under a file: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-o", t.TempDir()}, &bytes.Buffer{}, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "apidoc: ") {
		t.Errorf("a path that is a directory: exit %d, stderr %q", code, stderr.String())
	}
}
