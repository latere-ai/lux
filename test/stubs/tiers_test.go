// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The tiers spec 015 selects by build tag and test name prefix.
var tiers = map[string]string{"integration": "TestE2E", "postgres": "TestPostgres"}

// skipDirs are never read: the repository's metadata, build output, and
// an agent tool's worktrees, each a tree of its own.
var skipDirs = map[string]bool{".git": true, ".claude": true, "out": true, "node_modules": true, "testdata": true}

// root is the module root, found from this file.
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

// testFile is one _test.go file as the rule reads it: the tags its
// build constraint requires and the names of its test functions.
type testFile struct {
	path  string
	tags  []string
	tests []string
}

// parseTestFiles reads every _test.go file under dir.
func parseTestFiles(t *testing.T, dir string) []testFile {
	t.Helper()
	var files []testFile
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, testFile{path: rel, tags: requiredTags(f), tests: testNames(f)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// requiredTags is every tag a file's //go:build line names without a
// negation: the tiers a file belongs to.
func requiredTags(f *ast.File) []string {
	var tags []string
	for _, g := range f.Comments {
		for _, c := range g.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				continue
			}
			tags = append(tags, positiveTags(expr)...)
		}
	}
	return tags
}

// positiveTags walks a constraint for the tags required rather than
// excluded.
func positiveTags(e constraint.Expr) []string {
	switch x := e.(type) {
	case *constraint.TagExpr:
		return []string{x.Tag}
	case *constraint.AndExpr:
		return append(positiveTags(x.X), positiveTags(x.Y)...)
	case *constraint.OrExpr:
		return append(positiveTags(x.X), positiveTags(x.Y)...)
	case *constraint.NotExpr:
		return nil
	}
	return nil
}

// testNames lists the file's test functions: a top-level func named
// Test* taking one *testing.T. TestMain takes *testing.M and is not one.
func testNames(f *ast.File) []string {
	var names []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Params.NumFields() != 1 {
			continue
		}
		star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "T" {
			continue
		}
		names = append(names, fn.Name.Name)
	}
	return names
}

// TestEveryTestIsInATier: every test function in a file tagged
// integration begins with TestE2E and every one in a file tagged postgres
// begins with TestPostgres, wherever in the tree the file sits; a file
// tagged for both tiers holds no test of its own; and test/conformance,
// once it exists, has TestContract as its one server-driven entry point.
func TestEveryTestIsInATier(t *testing.T) {
	dir := root(t)
	files := parseTestFiles(t, dir)
	var tagged int
	for _, f := range files {
		var prefixes []string
		for tag, prefix := range tiers {
			if slices.Contains(f.tags, tag) {
				prefixes = append(prefixes, prefix)
			}
		}
		if len(prefixes) == 0 {
			continue
		}
		tagged++
		if len(prefixes) > 1 && len(f.tests) > 0 {
			t.Errorf("%s is tagged for two tiers and holds tests %v; a shared file holds helpers alone", f.path, f.tests)
			continue
		}
		for _, name := range f.tests {
			if !strings.HasPrefix(name, prefixes[0]) {
				t.Errorf("%s: %s is in a %s file and does not begin with %s", f.path, name, f.tags, prefixes[0])
			}
		}
	}
	if tagged == 0 {
		t.Fatal("no tagged test file was found; the tiers would pass vacuously")
	}
	conformance := filepath.Join(dir, "test", "conformance")
	if _, err := os.Stat(conformance); err != nil {
		return // spec 018's package is not in this tree yet
	}
	var contract bool
	for _, f := range parseTestFiles(t, conformance) {
		if len(f.tags) > 0 {
			t.Errorf("test/conformance/%s is tagged %v; the conformance tier runs untagged", f.path, f.tags)
		}
		contract = contract || slices.Contains(f.tests, "TestContract")
	}
	if !contract {
		t.Error("test/conformance has no TestContract, its one server-driven entry point")
	}
}

// TestPostgresMainRefusesWithoutAURL: the postgres tier run with
// LUX_DB_URL unset fails in TestMain naming the variable and one way to
// get a database, and never reports a skip. This test is untagged because
// the tagged run it drives cannot host it; its name shares the tier's
// prefix so make test-postgres runs it too.
func TestPostgresMainRefusesWithoutAURL(t *testing.T) {
	cmd := exec.Command("go", "test", "-count=1", "-tags=postgres", "-run", "^TestPostgres", "./test/e2e/")
	cmd.Dir = root(t)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LUX_DB_URL=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the postgres tier ran without LUX_DB_URL:\n%s", out)
	}
	text := string(out)
	for _, want := range []string{"LUX_DB_URL is unset", "docker run --rm -p 5432:5432 -e POSTGRES_PASSWORD=lux postgres:17-alpine"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SKIP") || strings.Contains(text, "no tests to run") {
		t.Errorf("the refusal reads as a skip:\n%s", text)
	}
}
