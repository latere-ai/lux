// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// update rewrites every golden file from the current output. Run once
// after a deliberate schema change, review the diff, and record the change
// in the CHANGELOG: a golden that moves is a schema change.
var update = flag.Bool("update", false, "rewrite the golden files of the corpus")

// golden compares got with the golden file beside a case, or writes it
// under -update.
func golden(t *testing.T, caseFile string, got []byte) {
	t.Helper()
	name := goldenName(caseFile)
	if *update {
		if err := os.WriteFile(filepath.Join("testdata", "v1", filepath.FromSlash(name)), got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := fs.ReadFile(Corpus, name)
	if err != nil {
		t.Fatalf("%s has no golden file; run with -update to write it", caseFile)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from %s:\n--- got\n%s\n--- want\n%s", caseFile, name, got, want)
	}
}

func TestGoldenCorpus(t *testing.T) {
	o := corpusOptions(t)
	kinds := map[string]int{}
	for _, name := range corpusCases(t, "accepted") {
		t.Run(name, func(t *testing.T) {
			obj, err := Decode(corpusFile(t, name), MediaYAML, Hint{})
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			r, err := Resolve(context.Background(), obj, o)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if dir := path.Base(path.Dir(name)); dir != strings.ToLower(obj.Kind()) {
				t.Errorf("a %s sits under accepted/%s", obj.Kind(), dir)
			}
			kinds[obj.Kind()]++
			got, err := json.MarshalIndent(r.Object, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, append(got, '\n'))
			// The golden is the resolved object: decoding it and resolving
			// again is a fixed point, which is what lets a caller GET, edit,
			// and PUT what it read.
			again, err := Decode(got, MediaJSON, Hint{})
			if err != nil {
				t.Fatalf("the golden does not decode: %v", err)
			}
			r2, err := Resolve(context.Background(), again, o)
			if err != nil {
				t.Fatalf("the golden does not resolve: %v", err)
			}
			got2, _ := json.MarshalIndent(r2.Object, "", "  ")
			if !bytes.Equal(got, got2) {
				t.Errorf("resolving the golden again moves it:\n%s\n%s", got, got2)
			}
		})
	}
	for _, kind := range []string{v1.KindProvider, v1.KindModel, v1.KindKey, v1.KindBudget} {
		if kinds[kind] == 0 {
			t.Errorf("no accepted case of kind %s", kind)
		}
	}
	for _, name := range corpusCases(t, "refused") {
		t.Run(name, func(t *testing.T) {
			obj, err := Decode(corpusFile(t, name), MediaYAML, Hint{})
			if err == nil {
				_, err = Resolve(context.Background(), obj, o)
			}
			if err == nil {
				t.Fatal("the case was accepted")
			}
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("refused with %T %v, not *Error", err, err)
			}
			if code := path.Base(path.Dir(name)); string(e.Code) != code {
				t.Errorf("refused with %s (%s), sits under refused/%s", e.Code, e.Detail, code)
			}
			if e.Detail == "" {
				t.Error("no developer detail")
			}
			paths := e.Paths
			if paths == nil {
				paths = []string{}
			}
			got, err := json.MarshalIndent(struct {
				Code  Code     `json:"code"`
				Paths []string `json:"paths"`
			}{e.Code, paths}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, append(got, '\n'))
		})
	}
}

func TestCorpusCoversEveryDecodeCode(t *testing.T) {
	// The codes Decode and stages 1 and 2 raise; the rest need a Lookup's
	// answer, an Existing object, or an authorizer's limits, and not_found
	// alone is expressible with the corpus's own Lookup.
	reachable := []Code{
		CodeMalformedBody, CodeMultiDocument, CodeUnsupportedVersion, CodeUnsupportedKind,
		CodeUnknownField, CodeMissingField, CodeInvalidField, CodeReservedPrefix,
		CodeExclusiveFields, CodeDuplicateTarget,
	}
	for _, code := range reachable {
		if len(corpusCases(t, "refused/"+string(code))) == 0 {
			t.Errorf("no refused case for %s", code)
		}
	}
	dirs, err := fs.ReadDir(Corpus, "refused")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, c := range Codes() {
		known[string(c)] = true
	}
	for _, d := range dirs {
		if !known[d.Name()] {
			t.Errorf("refused/%s is not a code of the table", d.Name())
		}
		if d.Name() == string(CodeUnsupportedMediaType) {
			t.Error("unsupported_media_type is a content type's refusal, not a file's")
		}
	}
	// Every case has its golden and every golden its case.
	err = fs.WalkDir(Corpus, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "options.json" {
			return err
		}
		switch {
		case strings.HasSuffix(p, ".golden.json"):
			if _, err := fs.Stat(Corpus, strings.TrimSuffix(p, ".golden.json")+".yaml"); err != nil {
				t.Errorf("%s has no case", p)
			}
		case strings.HasSuffix(p, ".yaml"):
			if _, err := fs.Stat(Corpus, goldenName(p)); err != nil {
				t.Errorf("%s has no golden", p)
			}
		default:
			t.Errorf("%s is neither a case nor a golden", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCodesHaveOneSentenceEach(t *testing.T) {
	seen := map[string]Code{}
	for _, c := range Codes() {
		m := c.Message()
		if m == "" {
			t.Errorf("%s has no sentence", c)
		}
		if other, dup := seen[m]; dup {
			t.Errorf("%s and %s share a sentence", c, other)
		}
		seen[m] = c
		if strings.Count(m, ". ") > 0 || !strings.HasSuffix(m, ".") {
			t.Errorf("%s: %q is not one sentence", c, m)
		}
	}
	if Code("nope").Message() != "" {
		t.Error("an unknown code has a sentence")
	}
	e := refuse(CodeInvalidField, "detail", "a", "b")
	if e.Error() != "invalid_field at a, b: detail" {
		t.Errorf("Error() = %q", e.Error())
	}
	if (&Error{Code: CodeMalformedBody}).Error() != "malformed_body" {
		t.Errorf("Error() = %q", (&Error{Code: CodeMalformedBody}).Error())
	}
}
