// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const module = "latere.ai/x/lux"

// root is the module root, found from this file rather than from the
// working directory, so the test reads the same tree wherever it runs.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", dir, err)
	}
	return dir
}

// goList runs go list with the given arguments at the module root and
// returns its lines.
func goList(t *testing.T, dir string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// The directories a package may live under. Everything at the module
// root that is not one of the three exported trees is a role or a tool
// under one of these.
var rootDirs = []string{"cmd", "internal", "test", "tools", "manifest", "gateway", "metering"}

// TestRootPackagesAreTheThree is the first half of spec 001's package
// rule: every package of the module is manifest, gateway, or metering, or
// sits under cmd, internal, test, or tools.
func TestRootPackagesAreTheThree(t *testing.T) {
	dir := root(t)
	for _, pkg := range goList(t, dir, "./...") {
		rel := strings.TrimPrefix(pkg, module)
		rel = strings.TrimPrefix(rel, "/")
		first, _, _ := strings.Cut(rel, "/")
		if rel == "" || !slices.Contains(rootDirs, first) {
			t.Errorf("package %s sits at the module root outside manifest, gateway, metering, cmd, internal, test, and tools", pkg)
		}
	}
}

// allow is one root package's allow list: the module packages it may
// reach, the third-party prefixes it may reach, and the standard library
// packages it may not, because they dial something. The lists are spec
// 001's rule; the spec that builds a package refines its row when it
// lands, and a new prefix is a row here with its reason in the commit.
type allow struct {
	module   []string // import path prefixes inside this module
	external []string // import path prefixes outside it
	noStd    []string // standard library packages the package may not reach
}

var rootAllow = map[string]allow{
	// manifest computes and validates: no HTTP client, no database
	// driver, no identity library. Its one decoder is the YAML library
	// spec 003 names.
	"manifest": {
		module:   []string{module + "/manifest"},
		external: []string{"github.com/goccy/go-yaml"},
		noStd:    []string{"net/http", "database/sql", "os/exec"},
	},
	// metering folds records and computes cost; it reaches the kinds and
	// nothing that dials.
	"metering": {
		module:   []string{module + "/manifest", module + "/metering"},
		external: []string{"github.com/goccy/go-yaml"},
		noStd:    []string{"net/http", "database/sql", "os/exec"},
	},
	// gateway is the data plane and dials one thing, the providers; it
	// reaches the store and the credentials through interfaces, so it
	// imports no store implementation and no identity library. Spec 004
	// refines this row as it lands. The three rows after golang.org/x/
	// are what the OpenTelemetry HTTP instrumentation pulls in behind
	// otelhttp.NewTransport, which spec 005's upstream client carries on
	// every hop; the SDK and its exporters stay out, because they reach
	// os/exec and the package's importer, not the package, exports.
	"gateway": {
		module: []string{module + "/manifest", module + "/metering", module + "/gateway"},
		external: []string{
			"github.com/goccy/go-yaml",
			"latere.ai/x/pkg/llmdialect",
			"latere.ai/x/pkg/llmjson",
			"latere.ai/x/pkg/httpjson",
			"latere.ai/x/pkg/circuitbreaker",
			"latere.ai/x/pkg/ratelimit",
			"latere.ai/x/pkg/retry",
			// The circuit per target of spec 008 is
			// latere.ai/x/pkg/circuitbreaker, which reaches retry and,
			// through it, wait: a cancellable sleep over context and
			// time that dials nothing.
			"latere.ai/x/pkg/wait",
			"latere.ai/x/pkg/semaphore",
			"latere.ai/x/pkg/otel",
			"latere.ai/x/pkg/metrics",
			"go.opentelemetry.io/",
			"golang.org/x/",
			"github.com/felixge/httpsnoop",
			"github.com/cespare/xxhash",
			"github.com/go-logr/",
			// The lux door writes latere.ai/x/pkg/httpjson's envelope,
			// and that package reaches github.com/google/uuid for its
			// path helpers; the same row admits it for luxd in
			// .lateregate.yaml (spec 004).
			"github.com/google/uuid",
		},
		noStd: []string{"database/sql", "os/exec"},
	},
}

// Prefixes no root package may reach, whatever its row says: the
// module's own internals, the identity libraries, and the store drivers.
var rootForbid = []string{
	module + "/internal/",
	module + "/cmd/",
	"latere.ai/x/pkg/authkit",
	"latere.ai/x/pkg/authz",
	"github.com/jackc/",
	"github.com/golang-migrate/",
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// TestRootPackagesDialNothing is spec 001's dependency rule over the
// build list of each root package. A root package that does not exist
// yet is skipped by name, so the test passes on the scaffold and bites as
// each lands.
func TestRootPackagesDialNothing(t *testing.T) {
	dir := root(t)
	for _, name := range []string{"manifest", "metering", "gateway"} {
		rule := rootAllow[name]
		t.Run(name, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Skipf("%s is not built yet", name)
			}
			deps := goList(t, dir, "-deps", "-f", "{{.ImportPath}} {{.Standard}}", "./"+name+"/...")
			for _, line := range deps {
				path, std, _ := strings.Cut(line, " ")
				switch {
				case std == "true":
					if slices.Contains(rule.noStd, path) {
						t.Errorf("%s reaches %s, which dials", name, path)
					}
				case hasPrefix(path, rootForbid):
					t.Errorf("%s reaches %s, which no root package may", name, path)
				case hasPrefix(path, rule.module), hasPrefix(path, rule.external):
				default:
					t.Errorf("%s reaches %s, which its allow list does not name; a new dependency is a row in rootAllow with its reason", name, path)
				}
			}
		})
	}
}
