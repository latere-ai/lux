// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The manifest group is spec 003's golden corpus applied through the
// API: the accepted cases in kind order, each read back and held to its
// golden, then the refused cases, each held to its code and paths, then
// the deletes. The corpus is manifest.Corpus, read through the import,
// and every name in it is prefixed with the run's conf-<run>- on the
// manifest and on the golden alike, because a suite that applied a
// Provider named openai against a serving installation would replace
// the operator's own, and because two runs against one server must not
// meet in one object.
var manifestCases = []testCase{
	{group: "manifest", name: "case003AcceptedCorpus", spec: 3, bearer: true, mode: serverMode, fn: case003AcceptedCorpus},
	{group: "manifest", name: "case003DefaultsAreVisible", spec: 3, bearer: true, mode: serverMode, fn: case003DefaultsAreVisible},
	{group: "manifest", name: "case003RefusedCorpus", spec: 3, bearer: true, mode: serverMode, fn: case003RefusedCorpus},
	{group: "manifest", name: "case003UnknownField", spec: 3, bearer: true, mode: serverMode, fn: case003UnknownField},
	{group: "manifest", name: "case003AcceptedCorpusDeletes", spec: 3, bearer: true, mode: serverMode, fn: case003AcceptedCorpusDeletes},
}

// corpusCases lists the .yaml files under one corpus directory, in kind
// order for the accepted half and by path otherwise.
func corpusCases(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(manifest.Corpus, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	rank := func(p string) int {
		return slices.Index(kindOrder, kindOfDir(path.Base(path.Dir(p))))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// kindOfDir maps an accepted/<kind> directory to the kind.
func kindOfDir(dir string) string {
	for k := range plurals {
		if strings.ToLower(k) == dir {
			return k
		}
	}
	return ""
}

// corpusFile reads one corpus file.
func corpusFile(t testing.TB, name string) []byte {
	t.Helper()
	data, err := fs.ReadFile(manifest.Corpus, name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}

// goldenOf reads the golden beside a case as a tree.
func goldenOf(t testing.TB, name string) map[string]any {
	t.Helper()
	data := corpusFile(t, strings.TrimSuffix(name, ".yaml")+".golden.json")
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatalf("%s: the golden is not a JSON object: %v", name, err)
	}
	return tree
}

// parseYAML reads one YAML mapping into a tree; ok is false for a body
// that is not one mapping, a second document, or an alias, which the
// suite sends as it is because the refusal is the parser's.
func parseYAML(data []byte) (map[string]any, bool) {
	text := string(data)
	if strings.Contains(text, "\n---") || strings.Contains(text, " &") {
		return nil, false
	}
	var tree map[string]any
	if err := yaml.Unmarshal(data, &tree); err != nil || tree == nil {
		return nil, false
	}
	return tree, true
}

// rename prefixes every name and every reference in a manifest tree of
// the kind with the run's prefix, and labels it, so the golden and the
// read-back are compared under one renaming. A name that begins with an
// id prefix is left as it is, since the case is about the prefix. A
// target whose upstream name is the Model's own name follows the name,
// because that equality is what the case says.
func (c *client) rename(tree map[string]any, kind string) {
	c.renameUnder(tree, kind, "conf-"+c.run+"-")
}

// renameUnder is rename under a prefix the caller narrows, which the
// fixture group does per release so one release's names meet neither
// another's nor the door fixtures the run already applied. The label is
// the run's either way, so teardown still collects what it created.
func (c *client) renameUnder(tree map[string]any, kind, prefix string) {
	meta, _ := tree["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		tree["metadata"] = meta
	}
	original, _ := meta["name"].(string)
	if original != "" && !hasKindPrefix(original) {
		meta["name"] = prefix + original
	}
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
		meta["labels"] = labels
	}
	labels["conformance"] = c.run
	spec, _ := tree["spec"].(map[string]any)
	if spec == nil {
		return
	}
	switch kind {
	case v1.KindModel:
		for _, tg := range arr(spec, "targets") {
			if target, ok := tg.(map[string]any); ok {
				if p, ok := target["provider"].(string); ok && p != "" && !hasKindPrefix(p) {
					target["provider"] = prefix + p
				}
				if m, ok := target["model"].(string); ok && m != "" && m == original {
					target["model"] = prefix + m
				}
			}
		}
	case v1.KindKey:
		if b, ok := spec["budget"].(string); ok && b != "" && !hasKindPrefix(b) {
			spec["budget"] = prefix + b
		}
		// A supplied value is unique across the installation, so a valid
		// one is this run's; a short one stays short, since its case is
		// about the length.
		if v, ok := spec["value"].(string); ok && len(v) >= 32 {
			spec["value"] = prefix + v
		}
		if sels, ok := spec["models"].([]any); ok {
			for i, s := range sels {
				if sel, ok := s.(string); ok && sel != "*" && sel != "" {
					sels[i] = prefix + sel
				}
			}
		}
	}
}

// hasKindPrefix reports a string that begins with one of the four id
// prefixes.
func hasKindPrefix(s string) bool {
	for _, p := range v1.KindPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// tunnelRefused reports a refusal of spec.tunnel, the answer of a server
// that does not enable tunneled Providers, which spec 013 has yet to
// build.
func tunnelRefused(e envelope) bool {
	return e.code == "invalid_field" && slices.Equal(e.paths, []string{"spec.tunnel"})
}

// case003AcceptedCorpus applies every accepted case in kind order,
// reads each back, and holds the read-back's metadata to the golden's.
// A tunneled Provider the server refuses, and the Models that target
// it, skip by name: the tunnel is spec 013's and not built.
func case003AcceptedCorpus(t testing.TB, c *client) {
	tunnelled := map[string]bool{}
	for _, name := range corpusCases(t, "accepted") {
		tree, ok := parseYAML(corpusFile(t, name))
		if !ok {
			t.Fatalf("%s does not parse as one YAML mapping", name)
		}
		kind, _ := tree["kind"].(string)
		golden := goldenOf(t, name)
		c.rename(tree, kind)
		c.rename(golden, kind)
		if kind == v1.KindModel {
			skip := false
			for _, tg := range arr(tree, "spec.targets") {
				if tunnelled[str(tg, "provider")] {
					skip = true
				}
			}
			if skip {
				t.Logf("%s: skipped, its Provider is tunnelled and the server refused the tunnel", name)
				continue
			}
		}
		objName := str(golden, "metadata.name")
		resp := c.apply(t, kind, objName, tree)
		if kind == v1.KindProvider && resp.Status == http.StatusBadRequest {
			if e := c.envelope(t, resp); tunnelRefused(e) {
				t.Logf("%s: skipped, the server does not enable tunnelled Providers: %s", name, e.detail)
				tunnelled[objName] = true
				continue
			}
		}
		if resp.Status != http.StatusCreated {
			t.Errorf("%s: PUT /%s/%s answered %d: %s", name, plurals[kind], objName, resp.Status, excerpt(resp.Body))
			continue
		}
		read := c.read(t, kind, objName)
		if read.Status != http.StatusOK {
			t.Errorf("%s: the read-back answered %d: %s", name, read.Status, excerpt(read.Body))
			continue
		}
		back := read.json(t)
		if got, want := canonical(t, back["metadata"]), canonical(t, golden["metadata"]); got != want {
			t.Errorf("%s: metadata read back as %s, the golden has %s", name, got, want)
		}
		if str(back, "apiVersion") != v1.APIVersion || str(back, "kind") != kind {
			t.Errorf("%s: read back as %s %s", name, str(back, "apiVersion"), str(back, "kind"))
		}
		c.mu.Lock()
		c.corpus = append(c.corpus, applied{name: name, kind: kind, golden: golden, read: back})
		c.mu.Unlock()
	}
	if len(c.corpus) == 0 {
		t.Fatal("no accepted case was applied")
	}
}

// case003DefaultsAreVisible holds each read-back's spec to the golden's
// byte for byte: every default Resolve fills is in the read-back, and
// nothing the caller set was changed.
func case003DefaultsAreVisible(t testing.TB, c *client) {
	if len(c.corpus) == 0 {
		c.skip(t, "case003AcceptedCorpus applied nothing")
	}
	for _, a := range c.corpus {
		if got, want := canonical(t, a.read["spec"]), canonical(t, a.golden["spec"]); got != want {
			t.Errorf("%s: spec read back as\n%s\nthe golden resolves to\n%s", a.name, got, want)
		}
	}
}

// refusedCase is one refused corpus case as the suite sends it.
type refusedCase struct {
	name  string
	code  string
	paths []string
	kind  string
	route string // the name in the path
	body  []byte
	ct    string
	skip  string // why the case is not reachable over HTTP
}

// prepareRefused reads a refused case: the code and paths from its
// golden, and the body renamed under the run where the YAML parses,
// sent as the file is otherwise. A case the route cannot reach is
// marked with why.
func (c *client) prepareRefused(t testing.TB, name string) refusedCase {
	t.Helper()
	golden := goldenOf(t, name)
	rc := refusedCase{name: name, code: str(golden, "code"), kind: v1.KindBudget, route: c.name("raw"), ct: "application/yaml"}
	for _, p := range arr(golden, "paths") {
		if s, ok := p.(string); ok {
			rc.paths = append(rc.paths, s)
		}
	}
	data := corpusFile(t, name)
	// A case the parser refuses is sent as the file is: a rewrite would
	// parse it first, and a YAML parser reads a broken JSON body as flow
	// syntax the API's own parser does not.
	if rc.code == "malformed_body" || rc.code == "multi_document" {
		rc.body = data
		return rc
	}
	tree, ok := parseYAML(data)
	if !ok {
		rc.body = data
		return rc
	}
	if _, has := tree["apiVersion"]; !has {
		rc.skip = "the route supplies apiVersion, so the case is a whole manifest over HTTP"
		return rc
	}
	kind, has := tree["kind"].(string)
	if !has {
		rc.skip = "the route supplies kind, so the case is a whole manifest over HTTP"
		return rc
	}
	if _, known := plurals[kind]; known {
		rc.kind = kind
	}
	if n := str(tree, "metadata.name"); hasKindPrefix(n) {
		rc.skip = "an id-shaped path segment is invalid_field at the route before the manifest is decoded (spec 011)"
		return rc
	}
	c.rename(tree, rc.kind)
	if rc.kind == v1.KindProvider {
		if base, err := url.Parse(str(tree, "spec.baseURL")); err == nil && strings.EqualFold(base.Hostname(), "lux.example.com") {
			// The corpus's own-host case names the corpus's public URL;
			// against this server the loop is this server's host.
			if own, err := url.Parse(c.cfg.URL); err == nil {
				base.Host = own.Host
				obj(tree, "spec")["baseURL"] = base.String()
			}
		}
	}
	if n := str(tree, "metadata.name"); n != "" {
		if path.Clean("/"+n) != "/"+n {
			rc.skip = "the name is not a clean path, and a path that is not clean is answered before the manifest is decoded (spec 011)"
			return rc
		}
		rc.route = n
	}
	rc.body = encode(t, tree)
	rc.ct = "application/json"
	return rc
}

// sendRefused sends one refused case and holds the answer to its code
// and paths. A Provider whose host the rule refuses without
// LUX_UPSTREAM_ALLOW_PRIVATE is admitted with a warning under it, which
// the rule allows, and the suite reads the warning as the server's
// answer and deletes what it created.
func (c *client) sendRefused(t testing.TB, rc refusedCase) {
	t.Helper()
	resp := c.apply(t, rc.kind, rc.route, rc.body, header("Content-Type", rc.ct))
	if resp.Status == http.StatusCreated && rc.code == "invalid_field" && slices.Equal(rc.paths, []string{"spec.baseURL"}) {
		body := resp.json(t)
		if warnings := arr(body, "status.warnings"); len(warnings) > 0 {
			t.Logf("%s: admitted with a warning, the server allows private upstreams: %v", rc.name, warnings[0])
			c.mustDelete(t, rc.kind, str(body, "status.id"))
			return
		}
	}
	if resp.Status < 400 {
		t.Errorf("%s: accepted with %d, want %s: %s", rc.name, resp.Status, rc.code, excerpt(resp.Body))
		// A 201 created an object of this case's own; a 200 updated one
		// the accepted half applied, which its own case deletes.
		if id := str(resp.json(t), "status.id"); id != "" && resp.Status == http.StatusCreated {
			c.mustDelete(t, rc.kind, id)
		}
		return
	}
	e := c.expect(t, resp, rc.code)
	if !slices.Equal(e.paths, rc.paths) && (len(e.paths) != 0 || len(rc.paths) != 0) {
		t.Errorf("%s: paths %v, want %v", rc.name, e.paths, rc.paths)
	}
}

// case003RefusedCorpus sends every refused case but the unknown_field
// ones through the route of its kind and holds the answer to the
// golden's code and paths.
func case003RefusedCorpus(t testing.TB, c *client) {
	n := 0
	for _, name := range corpusCases(t, "refused") {
		if strings.HasPrefix(name, "refused/unknown_field/") {
			continue
		}
		rc := c.prepareRefused(t, name)
		if rc.skip != "" {
			t.Logf("%s: skipped, %s", name, rc.skip)
			continue
		}
		c.sendRefused(t, rc)
		n++
	}
	if n == 0 {
		t.Fatal("no refused case was sent")
	}
}

// case003UnknownField sends the unknown_field cases: a field the schema
// does not know is refused with its path, at any depth, in each kind.
func case003UnknownField(t testing.TB, c *client) {
	n := 0
	for _, name := range corpusCases(t, "refused/unknown_field") {
		rc := c.prepareRefused(t, name)
		if rc.skip != "" {
			t.Logf("%s: skipped, %s", name, rc.skip)
			continue
		}
		c.sendRefused(t, rc)
		n++
	}
	if n == 0 {
		t.Fatal("no unknown_field case was sent")
	}
}

// case003AcceptedCorpusDeletes deletes what the accepted half applied,
// in reverse kind order, and reads not_found after each.
func case003AcceptedCorpusDeletes(t testing.TB, c *client) {
	if len(c.corpus) == 0 {
		c.skip(t, "case003AcceptedCorpus applied nothing")
	}
	for _, a := range slices.Backward(c.corpus) {
		id := str(a.read, "status.id")
		if resp := c.del(t, a.kind, id); resp.Status != http.StatusNoContent {
			t.Errorf("%s: DELETE %s answered %d: %s", a.name, id, resp.Status, excerpt(resp.Body))
			continue
		}
		c.expect(t, c.read(t, a.kind, id), "not_found")
	}
	c.mu.Lock()
	c.corpus = nil
	c.mu.Unlock()
}
