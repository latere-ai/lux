// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// threatModel is the spec whose threat table these tests read.
const threatModel = "specs/016-security-and-threat-model.md"

// testName matches a backticked Go test function name in a table cell,
// and the parenthesis that follows it when the tree does not hold that
// test yet: `TestX` (not built, [[011-api]], [[012-request-log-and-events]]).
var testName = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`(?:[ ]\\(not built,([^)]*)\\))?")

// specNumber matches the three digits at the head of a wikilink target,
// which is the spec that owes a test.
var specNumber = regexp.MustCompile(`\[\[(\d{3})-[a-z0-9-]+\]\]`)

// stateNumbers matches the numbers of a State cell: `built`, or
// `not built, 011, 012`.
var stateNumbers = regexp.MustCompile(`\b(\d{3})\b`)

// row is one row of the threat table.
type row struct {
	line   int      // the line number in the spec, for the failure message
	threat string   // the first cell, which names the row in a failure
	built  []string // the tests the row claims are in the tree
	owed   []string // the tests a named spec owes, in the row's order
	owers  []string // the spec numbers those tests are owed by
	state  string   // the last cell verbatim
}

// readSpec is the threat model's text, read from the module root so the
// test reads the same tree wherever it runs.
func readSpec(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// threatRows parses the threat table: the table under the "Threats and
// the answers" heading, whose header row ends in a State column. A row
// is five cells; the fourth names the tests and the fifth the state.
func threatRows(t *testing.T) []row {
	t.Helper()
	lines := strings.Split(readSpec(t, threatModel), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "### Threats and the answers") })
	if start < 0 {
		t.Fatalf("%s has no Threats and the answers heading", threatModel)
	}
	var rows []row
	seenHeader := false
	for i := start; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "### ") && i > start {
			break
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 5 {
			t.Errorf("%s:%d: the threat table has %d columns, want Threat, How, Owner, Test, State", threatModel, i+1, len(cells))
			continue
		}
		if !seenHeader {
			if want := []string{"Threat", "How the design answers it", "Owner", "Test", "State"}; !slices.Equal(trimAll(cells), want) {
				t.Fatalf("%s:%d: the header is %v, want %v", threatModel, i+1, trimAll(cells), want)
			}
			seenHeader = true
			continue
		}
		if strings.HasPrefix(cells[0], "---") {
			continue
		}
		r := row{line: i + 1, threat: strings.TrimSpace(cells[0]), state: strings.TrimSpace(cells[4])}
		for _, m := range testName.FindAllStringSubmatch(cells[3], -1) {
			if m[2] == "" {
				r.built = append(r.built, m[1])
				continue
			}
			r.owed = append(r.owed, m[1])
			for _, o := range specNumber.FindAllStringSubmatch(m[2], -1) {
				if !slices.Contains(r.owers, o[1]) {
					r.owers = append(r.owers, o[1])
				}
			}
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: the threat table has no rows", threatModel)
	}
	return rows
}

func trimAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// treeTests is every Go test function in the module, by name. A name is
// one function anywhere in the tree, because a row names the control and
// not the package that holds it.
func treeTests(t *testing.T) map[string]string {
	t.Helper()
	fn := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	dir := root(t)
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		for _, m := range fn.FindAllStringSubmatch(string(data), -1) {
			if _, ok := out[m[1]]; !ok {
				out[m[1]] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestThreatTableIsGrounded holds the threat table to the tree, one
// sub-test per row: a test the row names without an owner is in the
// tree, a test the row marks as owed is not yet, and the row's State
// says `built` when nothing is owed and repeats the owing numbers when
// something is. A row cannot claim a control the tree does not carry,
// and a row cannot keep saying a test is missing after it lands.
func TestThreatTableIsGrounded(t *testing.T) {
	tests := treeTests(t)
	for _, r := range threatRows(t) {
		t.Run(strconv.Itoa(r.line), func(t *testing.T) {
			if len(r.built) == 0 && len(r.owed) == 0 && !strings.Contains(r.state, "017") {
				t.Fatalf("%s:%d: %q names no test", threatModel, r.line, r.threat)
			}
			for _, name := range r.built {
				if _, ok := tests[name]; !ok {
					t.Errorf("%s:%d: %q names %s, which is in no _test.go of the tree; give it the spec that owes it or name the test that is there", threatModel, r.line, r.threat, name)
				}
			}
			for _, name := range r.owed {
				if where, ok := tests[name]; ok {
					t.Errorf("%s:%d: %q marks %s as not built, but it is in %s; drop the marker", threatModel, r.line, r.threat, name, where)
				}
			}
			want := append([]string(nil), r.owers...)
			sort.Strings(want)
			got := stateNumbers.FindAllStringSubmatch(r.state, -1)
			have := make([]string, 0, len(got))
			for _, m := range got {
				have = append(have, m[1])
			}
			sort.Strings(have)
			switch {
			case len(want) == 0 && len(have) == 0 && r.state != "built":
				t.Errorf("%s:%d: State is %q and nothing is owed, want built", threatModel, r.line, r.state)
			case len(want) > 0 && r.state == "built":
				t.Errorf("%s:%d: State is built but %v are owed", threatModel, r.line, r.owed)
			default:
				for _, n := range want {
					if !slices.Contains(have, n) {
						t.Errorf("%s:%d: State is %q and does not name %s, which owes a test in this row", threatModel, r.line, r.state, n)
					}
				}
			}
		})
	}
}
