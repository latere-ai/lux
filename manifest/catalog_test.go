// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// catalogDir is the example catalog of spec 035, the directory a
// self-hoster points LUX_BOOTSTRAP_DIR at.
var catalogDir = filepath.Join("..", "deploy", "catalog")

// catalogMedia is the content type of each manifest spelling a loader
// reads. Every other file of the catalog is its README.
var catalogMedia = map[string]string{".yaml": MediaYAML, ".yml": MediaYAML, ".json": MediaJSON}

// catalogFile is one file of the catalog: its path relative to the
// catalog, its bytes, and its decoded object when it is a manifest.
type catalogFile struct {
	rel  string
	data []byte
	obj  v1.Object
}

// readCatalog reads every file of the catalog in path order and decodes
// each manifest, failing on a file that is neither a manifest nor the
// README, because a loader would skip it without a word.
func readCatalog(t *testing.T) []catalogFile {
	t.Helper()
	var files []catalogFile
	err := filepath.WalkDir(catalogDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(catalogDir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f := catalogFile{rel: filepath.ToSlash(rel), data: data}
		if media, ok := catalogMedia[filepath.Ext(path)]; ok {
			obj, err := Decode(data, media, Hint{})
			if err != nil {
				t.Errorf("%s: Decode: %v", f.rel, err)
				return nil
			}
			f.obj = obj
		} else if f.rel != "README.md" {
			t.Errorf("%s is neither a manifest nor the README", f.rel)
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestCatalogResolves is spec 035's criterion 14: every manifest of the
// catalog decodes, validates and resolves, each Model against the
// Providers the catalog itself ships, with no warning; and every Model
// carries its own currency, per, input and output rate. The pricing is
// read on the decoded object, because resolving defaults the currency
// and the per, so a file without them would pass on the resolved one.
func TestCatalogResolves(t *testing.T) {
	files := readCatalog(t)
	lookup := &stubLookup{providers: map[string]*v1.Provider{}, budgets: map[string]*v1.Budget{}}
	o := Options{Actor: Actor{Subject: "https://login.example.com|admin"}, Lookup: lookup, FileMode: true}
	ctx := context.Background()
	resolve := func(f catalogFile) {
		r, err := Resolve(ctx, f.obj, o)
		if err != nil {
			t.Errorf("%s: Resolve: %v", f.rel, err)
			return
		}
		if len(r.Warnings) != 0 {
			t.Errorf("%s: warnings: %q", f.rel, r.Warnings)
		}
		if p, ok := r.Object.(*v1.Provider); ok {
			lookup.providers[p.Metadata.Name] = p
		}
	}
	var providers, models []catalogFile
	for _, f := range files {
		switch obj := f.obj.(type) {
		case nil:
		case *v1.Provider:
			providers = append(providers, f)
		case *v1.Model:
			models = append(models, f)
		default:
			t.Errorf("%s is a %s; the catalog holds Providers and Models", f.rel, obj.Kind())
		}
	}
	if len(providers) == 0 || len(models) == 0 {
		t.Fatalf("%d Providers and %d Models under %s; the walk read nothing", len(providers), len(models), catalogDir)
	}
	for _, f := range providers {
		resolve(f)
	}
	for _, f := range models {
		resolve(f)
		m := f.obj.(*v1.Model)
		p := m.Spec.Pricing
		if p == nil {
			t.Errorf("%s: no spec.pricing", f.rel)
			continue
		}
		var missing []string
		if p.Currency == "" {
			missing = append(missing, "currency")
		}
		if p.Per == 0 {
			missing = append(missing, "per")
		}
		if p.Input == nil {
			missing = append(missing, "input")
		}
		if p.Output == nil {
			missing = append(missing, "output")
		}
		if len(missing) != 0 {
			t.Errorf("%s: spec.pricing has no %s", f.rel, strings.Join(missing, ", "))
		}
	}
	t.Logf("%d Providers and %d Models read", len(providers), len(models))
}

// TestCatalogCarriesNoCompanyValue is spec 035's criterion 15 over the
// catalog: no label or annotation of a manifest is keyed under a domain
// but this core's own API group, and no file names the maintainer's
// domain or name anywhere but in that group, as the apiVersion and a
// label prefix spell it. The words that name a particular deployment are
// the tree-wide test's in internal/arch, which reads these files too.
func TestCatalogCarriesNoCompanyValue(t *testing.T) {
	group, _, _ := strings.Cut(v1.APIVersion, "/")
	_, domain, _ := strings.Cut(group, ".")
	name, _, _ := strings.Cut(domain, ".")
	host := regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)*` + regexp.QuoteMeta(domain) + `\b`)
	word := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(name))
	for _, f := range readCatalog(t) {
		if f.obj != nil {
			meta := metaOf(f.obj)
			for _, set := range []struct {
				field string
				keys  map[string]string
			}{{"label", meta.Labels}, {"annotation", meta.Annotations}} {
				for _, k := range slices.Sorted(maps.Keys(set.keys)) {
					if prefix, _, ok := strings.Cut(k, "/"); ok && prefix != group {
						t.Errorf("%s: %s %s is keyed under %s, not this core's API group", f.rel, set.field, k, prefix)
					}
				}
			}
		}
		for i, line := range strings.Split(string(f.data), "\n") {
			// Blank each spelling of the API group, then the name may
			// appear nowhere else on the line.
			kept := line
			for _, m := range host.FindAllStringIndex(line, -1) {
				if strings.EqualFold(line[m[0]:m[1]], group) && strings.HasPrefix(line[m[1]:], "/") && !strings.HasSuffix(line[:m[0]], "://") {
					kept = kept[:m[0]] + strings.Repeat(" ", m[1]-m[0]) + kept[m[1]:]
				}
			}
			if word.MatchString(kept) {
				t.Errorf("%s:%d names the maintainer outside the API group: %s", f.rel, i+1, strings.TrimSpace(line))
			}
		}
	}
}
