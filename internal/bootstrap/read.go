// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// kindOrder is the order documents are resolved and written in: a Model
// names a Provider, and a Key names a Budget and selects Models, so each
// reference resolves against something already resolved.
var kindOrder = []string{v1.KindProvider, v1.KindBudget, v1.KindModel, v1.KindKey}

// extensions are the spellings a bootstrap directory is read for, both
// YAML; a file of any other name is not read.
var extensions = map[string]bool{".yaml": true, ".yml": true}

// document is one decoded manifest, the file it came from, and the kind
// and name it declares, which every refusal and outcome line names.
type document struct {
	rel  string // the path under the directory
	path string // the directory joined with rel
	kind string
	name string
	obj  v1.Object
}

// readDir decodes every *.yaml and *.yml file under dir and its
// subdirectories, one document per file as Decode reads a body, in path
// order. The first file in that order that fails to decode, or that
// declares a kind and name another file already declared, ends the read
// with its refusal; a directory or a file that cannot be read is a plain
// error naming it.
func readDir(dir string) ([]*document, error) {
	type file struct {
		rel  string
		body []byte
	}
	var files []file
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !extensions[filepath.Ext(path)] {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, file{rel: rel, body: body})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap dir %s: %w", dir, err)
	}
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.rel, b.rel) })
	docs := make([]*document, 0, len(files))
	declared := map[string]string{}
	for _, f := range files {
		d := &document{rel: f.rel, path: filepath.Join(dir, f.rel)}
		obj, err := manifest.Decode(f.body, manifest.MediaYAML, manifest.Hint{})
		if err != nil {
			d.kind, d.name = envelopeOf(f.body)
			return nil, d.fail(err, "")
		}
		d.obj, d.kind, d.name = obj, obj.Kind(), obj.Name()
		if d.name != "" {
			key := d.kind + "/" + d.name
			if other, dup := declared[key]; dup {
				return nil, d.refuse(codeAlreadyExists, "the same "+d.kind+" is also declared in "+other, "metadata.name")
			}
			declared[key] = d.path
		}
		docs = append(docs, d)
	}
	return docs, nil
}

// envelopeOf reads the kind and metadata.name of a body that did not
// decode, so its refusal names the object when the body carries both as
// strings; either is empty when it does not, and a body that is not YAML
// at all names nothing.
func envelopeOf(body []byte) (kind, name string) {
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return "", ""
	}
	return doc.Kind, doc.Metadata.Name
}
