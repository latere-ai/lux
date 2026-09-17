// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// previous holds what each tagged release produced, under
// testdata/previous/<version>/: one resolved manifest per kind as
// <kind>.json, and the run's archived records as records.ndjson. The
// release pipeline of spec 017 writes the next directory at each tag;
// until the first tag the directory holds nothing and the group skips
// saying so. The pattern embeds the placeholder that keeps the directory
// in the tree.
//
//go:embed all:testdata/previous
var previous embed.FS

// The fixture group is the schema evolution promise of spec 003 and the
// record additivity promise of spec 009 executed: the previous release's
// manifests read back with an equal spec and its records decode with
// every member preserved.
var fixtureCases = []testCase{
	{group: "fixture", name: "case003PreviousReleaseManifests", spec: 3, bearer: true, mode: serverMode, fn: case003PreviousReleaseManifests},
	{group: "fixture", name: "case009PreviousReleaseRecords", spec: 9, fn: case009PreviousReleaseRecords},
}

// noPreviousRelease is the reason the group skips before the first tag.
const noPreviousRelease = "no previous release: test/conformance/testdata/previous/ holds no version directory"

// releases lists the version directories, oldest first by name.
func (c *client) releases(t testing.TB) []string {
	t.Helper()
	entries, err := fs.ReadDir(c.fixtures, "testdata/previous")
	if err != nil {
		t.Fatalf("reading the fixtures: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// releasePrefix names one release's objects apart from the rest of the
// run. A release seeds the names the suite's own door fixtures use,
// openai among them, so under the run prefix alone the apply would
// update another group's object rather than create its own, and two
// releases would meet in one name. The version becomes a DNS-1123
// label: every character that is not [a-z0-9] is a dash.
func (c *client) releasePrefix(version string) string {
	slug := []rune(strings.ToLower(version))
	for i, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			slug[i] = '-'
		}
	}
	return "conf-" + c.run + "-" + strings.Trim(string(slug), "-") + "-"
}

// case003PreviousReleaseManifests applies each release's resolved
// manifests under a prefix of that release's own and holds the
// read-back's spec to the fixture's.
func case003PreviousReleaseManifests(t testing.TB, c *client) {
	versions := c.releases(t)
	if len(versions) == 0 {
		c.skip(t, noPreviousRelease)
	}
	for _, version := range versions {
		prefix := c.releasePrefix(version)
		var applied []applied
		for _, kind := range kindOrder {
			data, err := fs.ReadFile(c.fixtures, "testdata/previous/"+version+"/"+strings.ToLower(kind)+".json")
			if err != nil {
				t.Errorf("%s: no %s fixture: %v", version, kind, err)
				continue
			}
			var tree map[string]any
			if err := json.Unmarshal(data, &tree); err != nil {
				t.Fatalf("%s/%s.json: %v", version, kind, err)
			}
			golden := map[string]any{"metadata": deepCopy(t, tree["metadata"]), "spec": deepCopy(t, tree["spec"])}
			c.renameUnder(tree, kind, prefix)
			c.renameUnder(golden, kind, prefix)
			name := str(tree, "metadata.name")
			resp := c.apply(t, kind, name, tree)
			if resp.Status != http.StatusCreated {
				t.Errorf("%s/%s: PUT answered %d: %s", version, kind, resp.Status, excerpt(resp.Body))
				continue
			}
			back := c.read(t, kind, name).json(t)
			if got, want := canonical(t, back["spec"]), canonical(t, golden["spec"]); got != want {
				t.Errorf("%s/%s: spec read back as\n%s\nthe release resolved\n%s", version, kind, got, want)
			}
			applied = append(applied, struct {
				name, kind   string
				golden, read map[string]any
			}{name, kind, golden, back})
		}
		for _, a := range slices.Backward(applied) {
			c.mustDelete(t, a.kind, str(a.read, "status.id"))
		}
	}
}

// deepCopy copies decoded JSON through its encoding.
func deepCopy(t testing.TB, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// case009PreviousReleaseRecords decodes each release's archived records
// with the current metering.Record and holds every member the fixture
// carries to the same value after a round trip, so a removed or retyped
// member fails here.
func case009PreviousReleaseRecords(t testing.TB, c *client) {
	versions := c.releases(t)
	if len(versions) == 0 {
		c.skip(t, noPreviousRelease)
	}
	for _, version := range versions {
		data, err := fs.ReadFile(c.fixtures, "testdata/previous/"+version+"/records.ndjson")
		if err != nil {
			t.Errorf("%s: no records fixture: %v", version, err)
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		n := 0
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			n++
			var rec metering.Record
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Errorf("%s: record %d does not decode: %v", version, n, err)
				continue
			}
			again, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			var was, now map[string]any
			_ = json.Unmarshal(line, &was)
			_ = json.Unmarshal(again, &now)
			for member, value := range was {
				if got, ok := now[member]; !ok {
					t.Errorf("%s: record %d: the member %s is gone", version, n, member)
				} else if canonical(t, got) != canonical(t, value) {
					t.Errorf("%s: record %d: %s was %s and reads %s", version, n, member, canonical(t, value), canonical(t, got))
				}
			}
			if !strings.HasPrefix(rec.ID, v1.PrefixRequest) {
				t.Errorf("%s: record %d: id %q", version, n, rec.ID)
			}
		}
		if n == 0 {
			t.Errorf("%s: the records fixture is empty", version)
		}
	}
}
