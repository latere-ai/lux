// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
)

// identitySpec carries the resource table every action of the
// vocabulary is a row of (spec 006).
const identitySpec = "specs/006-identity.md"

// actionHeader is the header of that table.
var actionHeader = []string{"Action", "`resource`"}

// actionSpans reads the code spans of a cell, which is how the table
// writes an action: `provider.read`, `.update`, `.delete`, `.tunnel`.
var actionSpans = regexp.MustCompile("`([a-z.]+)`")

// TestVocabularyIsSpec006sTable holds authorizer.Vocabulary to the table
// it is a reading of: the same actions, in the spec's order, each with
// the kind the spec's resource carries. The spec is the source and the
// package the one copy, so a row added to either alone fails here and a
// consumer that imports the package reads what the spec says.
func TestVocabularyIsSpec006sTable(t *testing.T) {
	got := authorizer.Vocabulary()
	rows := oneTable(t, identitySpec, readSpec(t, identitySpec), actionHeader)
	var want []authz.Action
	prefix := ""
	for _, row := range rows {
		kind := specKind(t, row[1], got.Kinds())
		for _, name := range specActions(t, row[0]) {
			if p, _, _ := strings.Cut(name, "."); p != "" {
				prefix = p
			} else {
				name = prefix + name
			}
			want = append(want, authz.Action{Name: name, Kind: kind})
		}
	}
	if !slices.Equal(got.Actions, want) {
		t.Errorf("authorizer.Vocabulary() is\n %v\nand %s's table is\n %v", got.Actions, identitySpec, want)
	}
}

// specActions is the actions one row of the first column names: the
// full action it opens with and the suffixes that share its kind.
func specActions(t *testing.T, cell string) []string {
	t.Helper()
	var out []string
	for _, m := range actionSpans.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("%s: the row %q names no action", identitySpec, cell)
	}
	return out
}

// specKind is the resource kind a row's second column names: the first
// of the vocabulary's kinds the cell says as a word, which is the kind
// inside its resource object or the one a row that says "as above"
// names in prose.
func specKind(t *testing.T, cell string, kinds []string) string {
	t.Helper()
	at, kind := len(cell), ""
	for _, k := range kinds {
		i := regexp.MustCompile(`\b` + k + `\b`).FindStringIndex(cell)
		if i != nil && i[0] < at {
			at, kind = i[0], k
		}
	}
	if kind == "" {
		t.Fatalf("%s: the resource %q names none of the kinds %v", identitySpec, cell, kinds)
	}
	return kind
}
