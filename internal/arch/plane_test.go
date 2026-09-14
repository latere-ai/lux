// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/lux/authorizer"
)

// The spec of the plane document and the document itself, plus the
// program the document prints, which is a command of its own so a
// reader can run what is on the page.
const (
	planeSpec       = "specs/020-building-a-plane.md"
	planeDoc        = "docs/plane.md"
	planeAuthorizer = "examples/authorizer/main.go"
)

// table reads the markdown table whose header cells are want, starting
// at the first line of the section that names it, and returns its rows
// with the cells trimmed. A file that carries the header twice yields
// the tables in order.
func tables(t *testing.T, text string, want []string) [][][]string {
	t.Helper()
	var out [][][]string
	var rows [][]string
	inTable := false
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			if inTable {
				out, rows, inTable = append(out, rows), nil, false
			}
			continue
		}
		cells := trimAll(strings.Split(strings.Trim(line, "|"), "|"))
		switch {
		case !inTable && slices.Equal(cells, want):
			inTable = true
		case !inTable:
		case strings.HasPrefix(cells[0], "---"):
		default:
			rows = append(rows, cells)
		}
	}
	if inTable {
		out = append(out, rows)
	}
	return out
}

// oneTable is the single table with these header cells, and a failure
// when the file carries none or several.
func oneTable(t *testing.T, what, text string, want []string) [][]string {
	t.Helper()
	found := tables(t, text, want)
	if len(found) != 1 {
		t.Fatalf("%s carries %d tables headed %v, want one", what, len(found), want)
	}
	if len(found[0]) == 0 {
		t.Fatalf("%s: the table headed %v has no rows", what, want)
	}
	return found[0]
}

// concernsHeader is the header of the concerns table in both files.
var concernsHeader = []string{"Concern", "Door: webhooks", "Door: packages"}

// mechanisms is what a token of the concerns table may name: the
// gateway's action vocabulary, the variables it reads, the routes it
// serves, the identifiers the module and the shared authorizer contract
// declare, the JSON names they encode, and the binaries it ships.
type mechanisms struct {
	actions   map[string]bool
	variables map[string]bool
	routes    []string
	idents    map[string]bool
	tags      map[string]bool
	binaries  map[string]bool
}

// variableName matches a configuration variable, routeToken a route or
// a route prefix with its method, actionName one action of the
// vocabulary, and command a binary with its subcommand.
var (
	variableName = regexp.MustCompile(`^LUX_[A-Z0-9_]+$`)
	variableIn   = regexp.MustCompile(`LUX_[A-Z0-9_]+`)
	routeToken   = regexp.MustCompile(`^(?:(GET|PUT|POST|DELETE|PATCH) )?(/[A-Za-z0-9./_{}-]*)$`)
	actionName   = regexp.MustCompile(`^[a-z]+\.[a-z]+$`)
	command      = regexp.MustCompile(`^[a-z-]+(?: [a-z-]+)+$`)
	identPath    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(?:\.[A-Za-z][A-Za-z0-9]*)*$`)
	parameter    = regexp.MustCompile(`{[a-zA-Z]+}`)
	jsonPath     = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(?:\.[a-z][a-zA-Z0-9]*)*$`)
)

// readMechanisms collects every name the tree carries. The identifiers
// and the JSON names come from the module's own sources and from
// latere.ai/x/pkg/authz, whose envelope is the authorizer's half of the
// table and is as much a mechanism as this module's own.
func readMechanisms(t *testing.T) mechanisms {
	t.Helper()
	m := mechanisms{
		actions:   map[string]bool{},
		variables: map[string]bool{},
		idents:    map[string]bool{},
		tags:      map[string]bool{},
		binaries:  map[string]bool{},
	}
	for _, a := range authorizer.Actions() {
		m.actions[a] = true
	}
	for _, name := range goList(t, root(t), "-f", "{{.Name}}", "./...") {
		m.idents[name] = true
	}
	goFiles(t, func(rel string, file *ast.File) {
		collect(m, file)
		if strings.HasSuffix(rel, ".go") {
			for _, v := range variableIn.FindAllString(readSpec(t, rel), -1) {
				m.variables[v] = true
			}
		}
	})
	dirs := goList(t, root(t), "-f", "{{.Dir}}", "latere.ai/x/pkg/authz")
	if len(dirs) != 1 {
		t.Fatalf("go list latere.ai/x/pkg/authz named %v", dirs)
	}
	parseDir(t, dirs[0], func(file *ast.File) { collect(m, file) })
	for _, e := range dirEntries(t, filepath.Join(root(t), "cmd")) {
		m.binaries[e] = true
	}
	doc, err := os.ReadFile(filepath.Join(root(t), "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(doc), "\n") {
		if path, ok := strings.CutPrefix(line, "  /"); ok && strings.HasSuffix(path, ":") {
			m.routes = append(m.routes, "/"+strings.TrimSuffix(path, ":"))
		}
	}
	if len(m.routes) == 0 || len(m.tags) == 0 || len(m.variables) == 0 {
		t.Fatalf("the mechanisms are %d routes, %d JSON names, %d variables", len(m.routes), len(m.tags), len(m.variables))
	}
	return m
}

// collect adds one file's declared names and JSON names.
func collect(m mechanisms, file *ast.File) {
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.TypeSpec:
			m.idents[node.Name.Name] = true
		case *ast.FuncDecl:
			m.idents[node.Name.Name] = true
		case *ast.ValueSpec:
			for _, name := range node.Names {
				m.idents[name.Name] = true
			}
		case *ast.Field:
			for _, name := range node.Names {
				m.idents[name.Name] = true
			}
			if node.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(node.Tag.Value)
			if err != nil {
				return true
			}
			for _, key := range []string{"json", "yaml"} {
				name, _, _ := strings.Cut(reflect.StructTag(tag).Get(key), ",")
				if name != "" && name != "-" {
					m.tags[name] = true
				}
			}
		}
		return true
	})
}

// dirEntries is the names of the directories under dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// grounded reports whether one backticked token of the table names a
// mechanism, and what kind it is. A dotted token grounds when every one
// of its segments does, so Options.Lookup grounds on the type and the
// field alike and metering.Fold on the package and the function.
func (m mechanisms) grounded(token string) (kind string, ok bool) {
	switch {
	case variableName.MatchString(token):
		return "variable", m.variables[token]
	case routeToken.MatchString(token):
		return "route", m.route(routeToken.FindStringSubmatch(token)[2])
	case actionName.MatchString(token) && m.actions[token]:
		return "action", true
	case command.MatchString(token):
		first, _, _ := strings.Cut(token, " ")
		return "command", m.binaries[first]
	case identPath.MatchString(token), jsonPath.MatchString(token), actionName.MatchString(token):
		return "name", m.names(token)
	}
	return "", false
}

// route reports whether the path is one the OpenAPI document serves, or
// the prefix of one; a path parameter's name is not part of the route,
// so {id} and {name} are one path.
func (m mechanisms) route(path string) bool {
	want := parameter.ReplaceAllString(path, "{}")
	for _, r := range m.routes {
		if r = parameter.ReplaceAllString(r, "{}"); r == want || strings.HasPrefix(r, strings.TrimSuffix(want, "/")+"/") {
			return true
		}
	}
	return false
}

// names reports whether every dotted segment is an identifier the tree
// declares, a JSON name it encodes, or a binary it ships.
func (m mechanisms) names(token string) bool {
	for segment := range strings.SplitSeq(token, ".") {
		if !m.idents[segment] && !m.tags[segment] && !m.binaries[segment] {
			return false
		}
	}
	return true
}

// TestConcernsTableIsGrounded is spec 020's concerns table read against
// the tree: every name a row puts in backticks is an action of the
// vocabulary, a variable the server reads, a route it serves, an
// identifier this module or the shared authorizer contract declares, a
// JSON name one of them encodes, or a binary the repository ships. A
// row that promised a platform a mechanism nobody built, and a row left
// behind by a rename, both fail here.
func TestConcernsTableIsGrounded(t *testing.T) {
	m := readMechanisms(t)
	for _, row := range oneTable(t, planeSpec, readSpec(t, planeSpec), concernsHeader) {
		t.Run(row[0], func(t *testing.T) {
			named := 0
			for _, cell := range row[1:] {
				for _, token := range backticked.FindAllStringSubmatch(cell, -1) {
					kind, ok := m.grounded(token[1])
					if kind == "" {
						t.Errorf("%q names %q, which is no action, variable, route, identifier, JSON name, or binary of the tree", row[0], token[1])
						continue
					}
					if !ok {
						t.Errorf("%q names the %s %q, which the tree does not carry", row[0], kind, token[1])
						continue
					}
					named++
				}
			}
			if named == 0 {
				t.Errorf("%q names no mechanism at all; every row of the table is an endpoint the platform writes or an object it applies", row[0])
			}
		})
	}
}

// sections are the Design headings of the spec, which are the sections
// the document owes, and the one heading that is about the document
// rather than in it.
func planeSections(t *testing.T) []string {
	t.Helper()
	var out []string
	inDesign := false
	for line := range strings.SplitSeq(readSpec(t, planeSpec), "\n") {
		switch {
		case line == "## Design":
			inDesign = true
		case strings.HasPrefix(line, "## "):
			inDesign = false
		case inDesign && strings.HasPrefix(line, "### "):
			// Two headings are about the document and the build rather
			// than sections of the document itself.
			if title := strings.TrimPrefix(line, "### "); title != "The document" && title != "What the build changed" {
				out = append(out, title)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no Design headings", planeSpec)
	}
	return out
}

// TestPlaneDocIsCurrent holds docs/plane.md to spec 020: every section
// the spec's design names is a heading of the document in the same
// order, the concerns table carries the same concerns, both credential
// tables carry the same hops, the Go block is the runnable program of
// examples/authorizer word for word, and the conformance command is the
// suite's own entry point with the variables it reads.
func TestPlaneDocIsCurrent(t *testing.T) {
	doc := readSpec(t, planeDoc)
	var headings []string
	for line := range strings.SplitSeq(doc, "\n") {
		if title, ok := strings.CutPrefix(line, "## "); ok {
			headings = append(headings, title)
		}
	}
	if want := planeSections(t); !slices.Equal(headings, want) {
		t.Errorf("%s has the sections %v, and %s names %v", planeDoc, headings, planeSpec, want)
	}

	spec := readSpec(t, planeSpec)
	if got, want := firstCells(oneTable(t, planeDoc, doc, concernsHeader)), firstCells(oneTable(t, planeSpec, spec, concernsHeader)); !slices.Equal(got, want) {
		t.Errorf("%s answers the concerns %v, and %s names %v", planeDoc, got, planeSpec, want)
	}
	hopHeader := []string{"Hop", "Credential", "Verified by"}
	docHops, specHops := tables(t, doc, hopHeader), tables(t, spec, hopHeader)
	if len(docHops) != 2 || len(specHops) != 2 {
		t.Fatalf("%s carries %d credential tables and %s %d, want two each", planeDoc, len(docHops), planeSpec, len(specHops))
	}
	for i := range docHops {
		if got, want := firstCells(docHops[i]), firstCells(specHops[i]); !slices.Equal(got, want) {
			t.Errorf("credential table %d in %s carries the hops %v, and %s %v", i+1, planeDoc, got, planeSpec, want)
		}
	}

	if got, want := codeBlock(t, doc, "go"), authorizerSource(t); got != want {
		t.Errorf("the Go block of %s is not %s word for word; the document prints a program nobody compiles", planeDoc, planeAuthorizer)
	}
	sh := codeBlock(t, doc, "sh")
	for _, want := range []string{"latere.ai/x/lux/test/conformance", "-run TestContract", "LUX_TEST_URL", "LUX_TEST_TOKEN"} {
		if !strings.Contains(sh, want) {
			t.Errorf("the conformance command of %s does not name %s:\n%s", planeDoc, want, sh)
		}
	}
}

// firstCells is the first cell of each row.
func firstCells(rows [][]string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[0])
	}
	return out
}

// codeBlock is the one fenced block of the language, and a failure when
// the document carries none or several.
func codeBlock(t *testing.T, text, language string) string {
	t.Helper()
	var blocks []string
	var current []string
	open := false
	for line := range strings.SplitSeq(text, "\n") {
		switch {
		case !open && line == "```"+language:
			open, current = true, nil
		case open && line == "```":
			blocks, open = append(blocks, strings.Join(current, "\n")+"\n"), false
		case open:
			current = append(current, line)
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("%s carries %d %s blocks, want one", planeDoc, len(blocks), language)
	}
	return blocks[0]
}

// authorizerSource is the program the document prints: the file without
// the two licence lines, which are the repository's and not the
// reader's.
func authorizerSource(t *testing.T) string {
	t.Helper()
	body := readSpec(t, planeAuthorizer)
	_, body, ok := strings.Cut(body, "// SPDX-License-Identifier: Apache-2.0\n\n")
	if !ok {
		t.Fatalf("%s does not begin with the two licence lines", planeAuthorizer)
	}
	return body
}

// parseFile parses one Go source and hands it to fn.
func parseFile(t *testing.T, path string, fn func(*ast.File)) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	fn(file)
}

// parseDir hands every Go source of one directory to fn.
func parseDir(t *testing.T, dir string, fn func(*ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		parseFile(t, filepath.Join(dir, e.Name()), fn)
	}
}
