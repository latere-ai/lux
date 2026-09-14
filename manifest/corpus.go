// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"embed"
	"io/fs"
)

//go:embed testdata/v1
var corpusRoot embed.FS

// Corpus is the golden corpus of spec 003, rooted at testdata/v1:
// accepted/<kind>/<case>.yaml with its <case>.golden.json, the
// Resolved.Object of the case under the fixed options of options.json;
// refused/<code>/<case>.yaml with its <case>.golden.json, the code and the
// JSON paths of the refusal. This package's TestGoldenCorpus and the
// manifest group of the conformance suite read the same files through
// this variable, so the contract the unit test holds and the contract the
// suite proves against a server are one set of files.
var Corpus fs.FS = mustSub(corpusRoot, "testdata/v1")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("manifest: corpus: " + err.Error())
	}
	return sub
}
