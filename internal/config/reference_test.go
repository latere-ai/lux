// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestConfigurationReferenceIsCurrent holds docs/configuration.md and the
// repository-root .env.example equal to the generators of reference.go, so
// the two files never drift from their one source. LUX_CONFIG_DOC_WRITE=1
// rewrites them.
func TestConfigurationReferenceIsCurrent(t *testing.T) {
	write := os.Getenv("LUX_CONFIG_DOC_WRITE") == "1"
	for _, f := range []struct {
		path string
		want string
	}{
		{filepath.Join("..", "..", "docs", "configuration.md"), ReferenceDoc()},
		{filepath.Join("..", "..", ".env.example"), EnvExample()},
	} {
		if write {
			if err := os.WriteFile(f.path, []byte(f.want), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		got, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != f.want {
			t.Errorf("%s is not the generator's output; run LUX_CONFIG_DOC_WRITE=1 go test ./internal/config -run TestConfigurationReferenceIsCurrent\n%s",
				filepath.Base(f.path), firstLineDiff(string(got), f.want))
		}
	}
}

// TestConfigurationReferenceMatchesCode holds the documented set equal to
// the LUX_ variables this package reads through getenv, so the reference
// cannot omit a variable the binary reads or document one it does not.
func TestConfigurationReferenceMatchesCode(t *testing.T) {
	read := getenvNames(t)
	documented := map[string]bool{}
	for _, n := range referenceNames() {
		if documented[n] {
			t.Errorf("%s is documented twice", n)
		}
		documented[n] = true
	}
	for n := range read {
		if !documented[n] {
			t.Errorf("%s is read by the code but not documented; add it to reference.go", n)
		}
	}
	for n := range documented {
		if !read[n] {
			t.Errorf("%s is documented but not read by the code; remove it from reference.go", n)
		}
	}
}

// getenvNames is every LUX_ variable this package reads through getenv, from
// the non-test source, the authoritative set the binary reads.
func getenvNames(t *testing.T) map[string]bool {
	t.Helper()
	call := regexp.MustCompile(`getenv\("(LUX_[A-Z0-9_]+)"\)`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			names[m[1]] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("found no getenv(\"LUX_...\") calls; the scan is broken")
	}
	return names
}

// firstLineDiff is the first line the two texts disagree on.
func firstLineDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	n := max(len(w), len(g))
	for i := range n {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "line " + strconv.Itoa(i+1) + ":\n  file:      " + gl + "\n  generator: " + wl
		}
	}
	return ""
}
