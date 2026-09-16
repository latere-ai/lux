// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/authorizer"
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
// root that is not one of the five exported trees is a role, a tool,
// or an example under one of these. examples holds the two programs
// docs/plane.md prints, which are built and tested like any package
// and are imported by nothing (spec 020).
var rootDirs = []string{"cmd", "internal", "test", "tools", "examples", "manifest", "gateway", "metering", "authorizer", "client"}

// TestRootPackagesAreTheThree is the first half of spec 001's package
// rule: every package of the module is manifest, gateway, metering,
// authorizer, or client, or sits under cmd, internal, test, or tools.
func TestRootPackagesAreTheThree(t *testing.T) {
	dir := root(t)
	for _, pkg := range goList(t, dir, "./...") {
		rel := strings.TrimPrefix(pkg, module)
		rel = strings.TrimPrefix(rel, "/")
		first, _, _ := strings.Cut(rel, "/")
		if rel == "" || !slices.Contains(rootDirs, first) {
			t.Errorf("package %s sits at the module root outside manifest, gateway, metering, authorizer, client, cmd, internal, test, and tools", pkg)
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
	// imports no store implementation and no identity library. The
	// llmdialect tree, the codecs and their intermediate representation,
	// and httpjson, the lux envelope, are reached through
	// latere.ai/x/pkg/llmdialect/bridge since spec 021, which gateway
	// imports beside llmdialect/ir alone; the bridge dials nothing. The
	// three rows after golang.org/x/ are what the OpenTelemetry HTTP
	// instrumentation pulls in behind otelhttp.NewTransport, which spec
	// 005's upstream client carries on every hop; the SDK and its
	// exporters stay out, because they reach os/exec and the package's
	// importer, not the package, exports.
	"gateway": {
		module: []string{module + "/manifest", module + "/metering", module + "/gateway"},
		external: []string{
			"github.com/goccy/go-yaml",
			"latere.ai/x/pkg/llmdialect",
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
	// authorizer is the vocabulary an authorizer is written against and
	// no policy at all: the actions, the resource per action, and the
	// limits an allow may carry (spec 022). It names authz.Resource and
	// authz.Decision, so it reaches latere.ai/x/pkg/authz and, behind it,
	// that package's decision cache and HTTP client, which it never
	// constructs and never dials; it reaches manifest for the ceilings a
	// limits object decodes to, and with it the one YAML library.
	"authorizer": {
		module: []string{module + "/manifest", module + "/authorizer"},
		external: []string{
			"latere.ai/x/pkg/authz",
			"latere.ai/x/pkg/cache",
			"github.com/goccy/go-yaml",
		},
		noStd: []string{"database/sql", "os/exec"},
	},
	// client is the typed client of the /v1 control plane, the one root
	// package whose purpose is to dial, and it dials one thing: the
	// address its caller hands it (spec 014). It decodes the error
	// envelope of latere.ai/x/pkg/httpjson, which reaches
	// github.com/google/uuid for its path helpers, and nothing else; the
	// kinds it carries are bytes, so it reaches no manifest package
	// either.
	"client": {
		module: []string{module + "/client"},
		external: []string{
			"latere.ai/x/pkg/httpjson",
			"github.com/google/uuid",
		},
		noStd: []string{"database/sql", "os/exec"},
	},
}

// Prefixes no root package may reach, whatever its row says: the
// module's own internals, the identity libraries, and the store
// drivers. A prefix may name the one package it does not bind: the
// shared authorizer contract is the envelope authorizer's whole purpose
// is naming, and net/http comes with it as a type that package never
// constructs (spec 022).
var rootForbid = map[string][]string{
	module + "/internal/":        nil,
	module + "/cmd/":             nil,
	"latere.ai/x/pkg/authkit":    nil,
	"latere.ai/x/pkg/authz":      {"authorizer"},
	"github.com/jackc/":          nil,
	"github.com/golang-migrate/": nil,
}

// outOfReach reports whether this root package may not reach the path,
// whatever its allow list says.
func outOfReach(name, path string) bool {
	for prefix, except := range rootForbid {
		if strings.HasPrefix(path, prefix) && !slices.Contains(except, name) {
			return true
		}
	}
	return false
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
	for _, name := range []string{"manifest", "metering", "gateway", "authorizer", "client"} {
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
				case outOfReach(name, path):
					t.Errorf("%s reaches %s, which no root package may", name, path)
				case hasPrefix(path, rule.module), hasPrefix(path, rule.external):
				default:
					t.Errorf("%s reaches %s, which its allow list does not name; a new dependency is a row in rootAllow with its reason", name, path)
				}
			}
		})
	}
}

// vocabularyHome is the one package that declares the action strings
// and the wire names of the limits object (spec 022).
const vocabularyHome = "authorizer/"

// wireLimitNames are the six members of the limits object an allow may
// carry, by the names they go over the wire under (spec 006).
var wireLimitNames = []string{
	"requests_per_minute", "max_key_requests_per_minute", "max_key_tokens_per_minute",
	"max_key_spend", "max_key_ttl", "max_keys",
}

// wireLimitsElsewhere is the one type outside the vocabulary that
// carries those names, with the reason it does.
var wireLimitsElsewhere = map[string]string{
	"SelfLimits": "GET /v1/self renders what the last allow granted, in the answer's own names (spec 011)",
}

// TestVocabularyHasOneHome is spec 022's rule over the tree's
// declarations: the action strings and the wire names of the limits
// object are declared in the authorizer package and nowhere else, so a
// platform reads one table and no copy of it drifts. An action string
// left in the tree is a JSON fixture or a subtest name inside a test,
// which declares nothing; a constant or a variable holding one is a
// second home.
func TestVocabularyHasOneHome(t *testing.T) {
	vocabulary := map[string]bool{}
	for _, a := range authorizer.Actions() {
		vocabulary[a] = true
	}
	limits := map[string]bool{}
	for _, n := range wireLimitNames {
		limits[n] = true
	}
	goFiles(t, func(rel string, file *ast.File) {
		if strings.HasPrefix(rel, vocabularyHome) {
			return
		}
		named := map[*ast.StructType]string{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec:
				for i, v := range node.Values {
					s, ok := stringLit(v)
					if !ok || !vocabulary[s] {
						continue
					}
					name := "_"
					if i < len(node.Names) {
						name = node.Names[i].Name
					}
					t.Errorf("%s declares %s = %q, a second home for an action; the vocabulary is %s's", rel, name, s, module+"/authorizer")
				}
			case *ast.TypeSpec:
				if st, ok := node.Type.(*ast.StructType); ok {
					named[st] = node.Name.Name
				}
			case *ast.StructType:
				what := named[node]
				if what == "" {
					what = "a struct"
				}
				if why, exempt := wireLimitsElsewhere[what]; exempt {
					t.Logf("%s: %s carries the limits names: %s", rel, what, why)
					return true
				}
				for _, f := range node.Fields.List {
					if f.Tag == nil {
						continue
					}
					tag, err := strconv.Unquote(f.Tag.Value)
					if err != nil {
						continue
					}
					name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
					if limits[name] {
						t.Errorf("%s declares %s with the limits member %q, a second reading of the limits object; it is %s.WireLimits", rel, what, name, module+"/authorizer")
					}
				}
			}
			return true
		})
	})
}

// stringLit is the string a declaration's value is, where it is one
// literal string and not an expression over several.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
