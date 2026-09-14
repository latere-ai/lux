// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The API group is the one place a Latere name belongs in the public
// contract; it appears as a prefix and never as a host.
const apiGroup = "lux.latere.ai"

// hostname matches any subdomain of the maintainer's domain. A match is
// allowed only when it is the API group followed by a slash and not
// preceded by a scheme, which is the group or the label prefix and never
// an address.
var hostname = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+latere\.ai\b`)

// forbidden are the words that name a particular deployment of Lux, a
// component internal to one, a private document, or the maintainer's
// other coordinates. The words are assembled at run time so this file
// does not trip its own test.
var forbidden = []*regexp.Regexp{
	regexp.MustCompile(`(?i)` + strings.Join([]string{"llm", "gateway"}, "-")),
	regexp.MustCompile(`(?i)hosted (plane|gateway|lux)\b`),
	regexp.MustCompile(`(?i)\b(platform|sandbox|topos)` + "d" + `\b`),
	regexp.MustCompile(`(?i)\b(latere-cli|origo-web|origod)\b`),
	regexp.MustCompile(`latere-ai/(specs|sandbox|agents|auth|platform|drive|insula|origo|origo-web|latere-cli|llm-gateway)\b`),
	regexp.MustCompile(`(?i)\bsixt\b`),
	regexp.MustCompile(`(?i)decisions/20\d\d-|infrastructure/identity|open-cores\.md|platform-surface\.md|\bid-(0[1-9]|10)-[a-z]`),
}

// skipDirs are never released and never read: the repository's own
// metadata, build output, and the worktrees an agent tool checks out
// under .claude, each a tree of its own with its own copy of this test.
var skipDirs = map[string]bool{".git": true, ".claude": true, "out": true, "node_modules": true, "dist": true}

// TestNoLatereCoordinatesInReleasedArtifacts is spec 001's invariant 8
// as a test over the whole tree: no document, manifest, workflow,
// default, comment, or string names a hostname of the maintainer's, a
// particular deployment of Lux, a component internal to one, or a
// private document. The module path and the shared library are the
// project's own coordinates and carry no subdomain, so they never match.
func TestNoLatereCoordinatesInReleasedArtifacts(t *testing.T) {
	dir := root(t)
	self := filepath.Join(dir, "internal", "arch")
	var hits []string
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
		if filepath.Dir(path) == self && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > 2<<20 {
			return nil // a binary asset, never a document
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil // not text
		}
		rel, _ := filepath.Rel(dir, path)
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range hostname.FindAllStringIndex(line, -1) {
				host := strings.ToLower(line[m[0]:m[1]])
				after := line[m[1]:]
				before := line[:m[0]]
				if host == apiGroup && strings.HasPrefix(after, "/") && !strings.HasSuffix(before, "://") {
					continue
				}
				// The gate declares the same group as a bare value,
				// `api_group: lux.latere.ai`, which is the coordinate and
				// not an address.
				if host == apiGroup && strings.HasSuffix(before, "api_group: ") {
					continue
				}
				hits = append(hits, rel+":"+strconv.Itoa(i+1)+": hostname "+line[m[0]:m[1]])
			}
			for _, re := range forbidden {
				if m := re.FindString(line); m != "" {
					hits = append(hits, rel+":"+strconv.Itoa(i+1)+": "+m)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Error(h)
	}
}
