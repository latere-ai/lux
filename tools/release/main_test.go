// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// root is the module root, found from this file.
func root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", dir, err)
	}
	return dir
}

// copyTree copies the five promised files and the golden corpus into a
// fresh directory, so a test mutates a copy and never the tree.
func copyTree(t *testing.T) string {
	t.Helper()
	src, dst := dirTree(root(t)), t.TempDir()
	names := []string{variablesFile, codesFile, eventsFile, recordFile, openAPIFile}
	goldens, err := src.List(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(names, goldens...) {
		data, err := src.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dst, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// edit rewrites one file of the copy through fn.
func edit(t *testing.T, dir, name string, fn func(string) string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := fn(string(data))
	if next == string(data) {
		t.Fatalf("%s: the edit changed nothing", name)
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVersionPromise is spec 017's row for the version promise: the tree
// is read into its promised surfaces, a synthetic removal of a variable,
// an error code, an event type, a record member, and a route, and a
// change to one golden output, are each classified major, additions of
// the same are minor, and Check refuses a tag whose bump is smaller than
// the largest class found, with the pre-v1.0.0 allowance for a minor.
func TestVersionPromise(t *testing.T) {
	prev, err := Read(dirTree(root(t)))
	if err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]map[string]bool{"variables": prev.Variables, "codes": prev.Codes, "events": prev.Events, "record members": prev.RecordFields, "api": prev.API} {
		if len(set) == 0 {
			t.Fatalf("the tree has no %s", name)
		}
	}
	if len(prev.Goldens) < 4 {
		t.Fatalf("the tree has %d golden outputs", len(prev.Goldens))
	}
	for _, want := range []string{"LUX_PUBLIC_URL", "LUX_DB_MAX_CONNS", "LUX_S3_PREFIX"} {
		if !prev.Variables[want] {
			t.Errorf("the variable table lacks %s: %v", want, sortedKeys(prev.Variables))
		}
	}
	for _, name := range []string{"LUX_TEST_", "LUX_INSTALL_", "LUX_"} {
		if prev.Variables[name] {
			t.Errorf("%q read as a variable", name)
		}
	}
	if !prev.Codes["malformed_body"] || !prev.Events["check.ping"] || !prev.RecordFields["id"] || !prev.API["GET /v1/self"] {
		t.Errorf("a known item is missing: codes %v events %v fields %v", prev.Codes["malformed_body"], prev.Events["check.ping"], prev.RecordFields["id"])
	}

	t.Run("no difference is a patch", func(t *testing.T) {
		if diffs := Compare(prev, prev); len(diffs) != 0 || Required(diffs) != Patch {
			t.Fatalf("differences against itself: %v", diffs)
		}
		for _, tag := range []string{"v0.1.1", "v0.2.0", "v1.0.0"} {
			if err := Check("v0.1.0", tag, nil); err != nil {
				t.Errorf("%s after v0.1.0 with no difference: %v", tag, err)
			}
		}
	})

	t.Run("removals are major", func(t *testing.T) {
		dir := copyTree(t)
		edit(t, dir, variablesFile, func(s string) string { return strings.Replace(s, "`LUX_S3_PREFIX`", "`LUX_S3_ARCHIVE_PREFIX`", 1) })
		edit(t, dir, codesFile, func(s string) string {
			return strings.Replace(s, `Code = "multi_document"`, `Code = "many_documents"`, 1)
		})
		edit(t, dir, eventsFile, func(s string) string { return strings.Replace(s, `= "check.ping"`, `= "check.hello"`, 1) })
		edit(t, dir, recordFile, func(s string) string { return strings.Replace(s, `json:"endedAt"`, `json:"finishedAt"`, 1) })
		edit(t, dir, openAPIFile, func(s string) string { return strings.Replace(s, "\n  /v1/self:\n", "\n  /v1/whoami:\n", 1) })
		var golden string
		for name := range prev.Goldens {
			if strings.HasSuffix(name, "/openai.golden.json") {
				golden = name
			}
		}
		if golden == "" {
			t.Fatal("no openai golden")
		}
		edit(t, dir, golden, func(s string) string { return strings.Replace(s, "{", "{\"changed\": true, ", 1) })
		next, err := Read(dirTree(dir))
		if err != nil {
			t.Fatal(err)
		}
		diffs := Compare(prev, next)
		var majors, minors []string
		for _, d := range diffs {
			switch d.Class {
			case Major:
				majors = append(majors, d.Surface+" "+d.Item)
			case Minor:
				minors = append(minors, d.Surface+" "+d.Item)
			}
		}
		for _, want := range []string{"variable LUX_S3_PREFIX", "error code multi_document", "event type check.ping", "record member endedAt", "api GET /v1/self", "golden " + golden} {
			if !slices.Contains(majors, want) {
				t.Errorf("%s removed or changed is not major: %v", want, majors)
			}
		}
		for _, want := range []string{"variable LUX_S3_ARCHIVE_PREFIX", "error code many_documents", "event type check.hello", "record member finishedAt", "api GET /v1/whoami"} {
			if !slices.Contains(minors, want) {
				t.Errorf("%s added is not minor: %v", want, minors)
			}
		}
		if Required(diffs) != Major {
			t.Fatalf("required %s", Required(diffs))
		}
		if err := Check("v1.2.3", "v1.2.4", diffs); err == nil || !strings.Contains(err.Error(), "asks for a major one") || !strings.Contains(err.Error(), "variable LUX_S3_PREFIX removed") {
			t.Errorf("a patch over a removal: %v", err)
		}
		if err := Check("v1.2.3", "v1.3.0", diffs); err == nil {
			t.Error("a minor over a removal passed after v1.0.0")
		}
		if err := Check("v1.2.3", "v2.0.0", diffs); err != nil {
			t.Errorf("a major over a removal: %v", err)
		}
		if err := Check("v0.1.0", "v0.2.0", diffs); err != nil {
			t.Errorf("before v1.0.0 a minor may break a row: %v", err)
		}
		if err := Check("v0.1.0", "v0.1.1", diffs); err == nil {
			t.Error("a patch over a removal passed before v1.0.0")
		}
		var out bytes.Buffer
		if err := promise(dirTree(dir), dirTree(root(t)), "v0.1.0", "v0.1.1", &out); err == nil {
			t.Error("promise passed a patch over removals")
		}
		if !strings.Contains(out.String(), "promise: major: golden "+golden+" changed") {
			t.Errorf("the differences are not printed:\n%s", out.String())
		}
	})

	t.Run("additions are minor", func(t *testing.T) {
		dir := copyTree(t)
		edit(t, dir, variablesFile, func(s string) string {
			return strings.Replace(s, "`LUX_S3_PREFIX` |", "`LUX_S3_PREFIX`, `LUX_S3_NEW_THING` |", 1)
		})
		edit(t, dir, recordFile, func(s string) string {
			return strings.Replace(s, "\tOwner string `json:\"owner\"`", "\tOwner string `json:\"owner\"`\n\tExtra string `json:\"extra\"`", 1)
		})
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(goldenDir), "provider", "new.golden.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		next, err := Read(dirTree(dir))
		if err != nil {
			t.Fatal(err)
		}
		diffs := Compare(prev, next)
		if len(diffs) != 3 || Required(diffs) != Minor {
			t.Fatalf("differences %v", diffs)
		}
		if err := Check("v1.0.0", "v1.0.1", diffs); err == nil {
			t.Error("a patch over additions passed")
		}
		if err := Check("v1.0.0", "v1.1.0", diffs); err != nil {
			t.Errorf("a minor over additions: %v", err)
		}
		var out bytes.Buffer
		if err := promise(dirTree(dir), dirTree(root(t)), "v1.0.0", "v1.1.0", &out); err != nil {
			t.Errorf("promise: %v", err)
		}
		if !strings.Contains(out.String(), "3 difference(s) since v1.0.0 ask for a minor bump at most, and v1.1.0 is one") {
			t.Errorf("stdout:\n%s", out.String())
		}
	})

	t.Run("tags", func(t *testing.T) {
		for _, tc := range []struct {
			prev, tag string
			want      Class
			bad       bool
		}{
			{"v0.1.0", "v0.1.1", Patch, false},
			{"v0.1.0", "v0.2.0", Minor, false},
			{"v0.9.9", "v1.0.0", Major, false},
			{"v1.0.0", "v1.0.0", Patch, true},
			{"v1.1.0", "v1.0.5", Patch, true},
			{"1.0.0", "v1.0.1", Patch, true},
			{"v1.0.0", "v1.0", Patch, true},
			{"v1.0.0", "v1.0.x", Patch, true},
		} {
			got, err := Bump(tc.prev, tc.tag)
			if (err != nil) != tc.bad || got != tc.want {
				t.Errorf("Bump(%s, %s) = %s, %v", tc.prev, tc.tag, got, err)
			}
		}
		for c, want := range map[Class]string{Patch: "patch", Minor: "minor", Major: "major"} {
			if c.String() != want {
				t.Errorf("%d prints %s", c, c)
			}
		}
	})

	t.Run("the previous tag is read through git", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not on PATH, so the tag reader is not exercised here")
		}
		g := gitTree{root(t), "HEAD"}
		data, err := g.Read("go.mod")
		if err != nil || !strings.HasPrefix(string(data), "module latere.ai/x/lux") {
			t.Fatalf("Read(go.mod) = %q, %v", data, err)
		}
		names, err := g.List(goldenDir)
		if err != nil || len(names) == 0 {
			t.Fatalf("List = %v, %v", names, err)
		}
		if _, err := g.Read("no/such/file"); err == nil {
			t.Error("a missing file read")
		}
		// HEAD against itself: both sides are read through git, so the
		// comparison exercises the tag reader and holds whatever the
		// working tree carries uncommitted. A working tree that adds a
		// variable asks for a minor bump against HEAD by design, and the
		// gate runs before that change is committed.
		var out bytes.Buffer
		if err := promise(g, g, "v0.1.0", "v0.1.1", &out); err != nil {
			t.Errorf("HEAD against itself: %v", err)
		}
		if got := run([]string{"promise", "-C", root(t), "-previous", "HEAD", "-tag", "v0.1.1"}, &out, &out); got != 1 || !strings.Contains(out.String(), `"HEAD" is not a release tag`) {
			t.Errorf("a previous ref that is no release tag: exit %d:\n%s", got, out.String())
		}
	})
}

// TestPromiseCommand: the first release has nothing to compare with, a
// tag that is not a release tag is refused, and the flags are checked.
func TestPromiseCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"promise", "-tag", "v0.1.0", "-previous", "none"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "v0.1.0 is the first release") {
		t.Errorf("exit %d: %s%s", code, out.String(), errOut.String())
	}
	if code := run([]string{"promise", "-tag", "v0.1.0"}, &out, &errOut); code != 0 {
		t.Errorf("no -previous: exit %d", code)
	}
	if code := run([]string{"promise", "-tag", "0.1.0", "-previous", "none"}, &out, &errOut); code != 1 {
		t.Errorf("a bad tag: exit %d", code)
	}
	if code := run([]string{"promise", "-previous", "none"}, &out, &errOut); code != 2 {
		t.Errorf("no -tag: exit %d", code)
	}
	if code := run([]string{"promise", "-no-such-flag"}, &out, &errOut); code != 2 {
		t.Errorf("a bad flag: exit %d", code)
	}
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("no subcommand: exit %d", code)
	}
	if code := run([]string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("an unknown subcommand: exit %d", code)
	}
	if code := run([]string{"promise", "-C", t.TempDir(), "-previous", "v0.0.1", "-tag", "v0.0.2"}, &out, &errOut); code != 1 {
		t.Errorf("an unreadable previous tree: exit %d", code)
	}
}

// writeArchive writes a gzipped tar with one member.
func writeArchive(t *testing.T, path, member string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestImagesCarryTheReleasedBinaries is spec 017's row for the images:
// the binary extracted from each architecture's image has the digest of
// the binary the build produced and of the archive's member; a differing
// byte, a missing extraction, and an archive without the member each
// fail naming the pair.
func TestImagesCarryTheReleasedBinaries(t *testing.T) {
	dist, extracted := t.TempDir(), t.TempDir()
	write := func(dir, name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, binary := range imageBinaries {
		for _, arch := range architectures {
			data := []byte(binary + " for " + arch)
			write(dist, binary+"_linux_"+arch, data)
			write(extracted, binary+"_linux_"+arch, data)
			if binary == "luxd" {
				writeArchive(t, filepath.Join(dist, "luxd_v0.1.0_linux_"+arch+".tar.gz"), "luxd", data)
			}
		}
	}
	var out bytes.Buffer
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "images: luxd_linux_amd64 ") || !strings.Contains(out.String(), "matches the build and 1 archive(s)") || !strings.Contains(out.String(), "images: lux-stubs_linux_arm64 ") {
		t.Errorf("stdout:\n%s", out.String())
	}

	write(extracted, "luxd_linux_arm64", []byte("luxd for arm64 but other bytes"))
	out.Reset()
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 1 || !strings.Contains(out.String(), "luxd_linux_arm64: the image carries") {
		t.Errorf("a differing byte: exit %d:\n%s", code, out.String())
	}
	write(extracted, "luxd_linux_arm64", []byte("luxd for arm64"))
	writeArchive(t, filepath.Join(dist, "luxd_v0.1.0_linux_arm64.tar.gz"), "luxd", []byte("another build"))
	out.Reset()
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 1 || !strings.Contains(out.String(), "and the archive") {
		t.Errorf("an archive with other bytes: exit %d:\n%s", code, out.String())
	}
	writeArchive(t, filepath.Join(dist, "luxd_v0.1.0_linux_arm64.tar.gz"), "README", []byte("no binary"))
	out.Reset()
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 1 || !strings.Contains(out.String(), "no member luxd in the archive") {
		t.Errorf("an archive without the member: exit %d:\n%s", code, out.String())
	}
	if err := os.Remove(filepath.Join(extracted, "lux-stubs_linux_amd64")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 1 || !strings.Contains(out.String(), "lux-stubs_linux_amd64: not extracted from the image") {
		t.Errorf("a missing extraction: exit %d:\n%s", code, out.String())
	}
	if err := os.Remove(filepath.Join(dist, "lux-stubs_linux_arm64")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 1 || !strings.Contains(out.String(), "lux-stubs_linux_arm64: not built") {
		t.Errorf("a missing build: exit %d:\n%s", code, out.String())
	}
	if code := run([]string{"images", "-dist", dist}, &out, &out); code != 2 {
		t.Errorf("no -extracted: exit %d", code)
	}
	if code := run([]string{"images", "-no-such-flag"}, &out, &out); code != 2 {
		t.Errorf("a bad flag: exit %d", code)
	}
	if err := os.WriteFile(filepath.Join(dist, "luxd_v0.1.0_linux_amd64.tar.gz"), []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"images", "-dist", dist, "-extracted", extracted}, &out, &out); code != 1 || !strings.Contains(out.String(), "luxd_v0.1.0_linux_amd64.tar.gz: ") {
		t.Errorf("a corrupt archive: exit %d:\n%s", code, out.String())
	}
}

// memTree is a tree held in memory, so a test names exactly which read
// fails. A name mapped to nil is listed but cannot be read, and listErr
// fails every List.
type memTree struct {
	files   map[string][]byte
	listErr error
}

func (m memTree) Read(name string) ([]byte, error) {
	data, ok := m.files[name]
	if !ok || data == nil {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return data, nil
}

func (m memTree) List(dir string) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []string
	for name := range m.files {
		if strings.HasPrefix(name, dir+"/") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadTree reads the promised files and the golden corpus of the module
// into a memTree.
func loadTree(t *testing.T) memTree {
	t.Helper()
	src := dirTree(root(t))
	goldens, err := src.List(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	m := memTree{files: map[string][]byte{}}
	for _, name := range append([]string{variablesFile, codesFile, eventsFile, recordFile, openAPIFile}, goldens...) {
		data, err := src.Read(name)
		if err != nil {
			t.Fatal(err)
		}
		m.files[name] = data
	}
	return m
}

// TestReadRefusesATreeMissingASurface: a tree without one of the promised
// files, with a golden it lists and cannot read, with a golden directory it
// cannot list, or with an OpenAPI document that does not parse is an error
// and never a Surface with that surface empty, which Compare would read as
// every item of it removed.
func TestReadRefusesATreeMissingASurface(t *testing.T) {
	if _, err := Read(loadTree(t)); err != nil {
		t.Fatalf("the whole tree: %v", err)
	}
	var golden string
	for name := range loadTree(t).files {
		if strings.HasSuffix(name, ".golden.json") {
			golden = name
			break
		}
	}
	listFails := errors.New("the golden directory cannot be listed")
	for _, tc := range []struct {
		name   string
		change func(memTree) memTree
		want   string
	}{
		{"no variable table", func(m memTree) memTree { delete(m.files, variablesFile); return m }, variablesFile},
		{"no error codes", func(m memTree) memTree { delete(m.files, codesFile); return m }, codesFile},
		{"no event types", func(m memTree) memTree { delete(m.files, eventsFile); return m }, eventsFile},
		{"no usage record", func(m memTree) memTree { delete(m.files, recordFile); return m }, recordFile},
		{"no OpenAPI document", func(m memTree) memTree { delete(m.files, openAPIFile); return m }, openAPIFile},
		{"an unreadable golden", func(m memTree) memTree { m.files[golden] = nil; return m }, golden},
		{"an unlistable golden directory", func(m memTree) memTree { m.listErr = listFails; return m }, listFails.Error()},
		{"an OpenAPI document that does not parse", func(m memTree) memTree { m.files[openAPIFile] = []byte("paths: [\n"); return m }, openAPIFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(tc.change(loadTree(t)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Read = %v, want an error naming %s", err, tc.want)
			}
		})
	}
	if _, err := dirTree(t.TempDir()).List(goldenDir); err == nil {
		t.Error("a directory without the golden corpus listed")
	}
}

// TestPromiseRefusesAnUnreadableTree: the tree being cut is read as the
// previous tag is, and a tree that cannot be read fails the promise rather
// than passing it with nothing compared.
func TestPromiseRefusesAnUnreadableTree(t *testing.T) {
	var out bytes.Buffer
	err := promise(memTree{files: map[string][]byte{}}, loadTree(t), "v0.1.0", "v0.1.1", &out)
	if err == nil || !strings.Contains(err.Error(), "reading the tree") {
		t.Fatalf("promise = %v, want the tree named", err)
	}
	if out.Len() != 0 {
		t.Errorf("differences printed for a tree that was not read:\n%s", out.String())
	}
}

// TestCompareAndCheckEdges: a golden output that disappeared is major, and
// Check refuses a tag that is not later than the previous one whatever
// the differences are.
func TestCompareAndCheckEdges(t *testing.T) {
	prev := Surface{Goldens: map[string]string{"a.golden.json": "1", "b.golden.json": "2"}}
	next := Surface{Goldens: map[string]string{"a.golden.json": "1"}}
	diffs := Compare(prev, next)
	if len(diffs) != 1 || diffs[0].Class != Major || diffs[0].Item != "b.golden.json" || diffs[0].Change != "removed" {
		t.Fatalf("a removed golden: %v", diffs)
	}
	if err := Check("v1.2.0", "v1.1.0", nil); err == nil || !strings.Contains(err.Error(), "not later than") {
		t.Errorf("an earlier tag: %v", err)
	}
}

// TestGitTreeOutsideARepository: a ref read from a directory git does not
// know is an error naming the git command, for a file and for a listing,
// whether or not git itself is on PATH.
func TestGitTreeOutsideARepository(t *testing.T) {
	g := gitTree{t.TempDir(), "v0.1.0"}
	if _, err := g.Read(variablesFile); err == nil || !strings.Contains(err.Error(), "git show v0.1.0:"+variablesFile) {
		t.Errorf("Read = %v", err)
	}
	if _, err := g.List(goldenDir); err == nil || !strings.Contains(err.Error(), "git ls-tree -r --name-only v0.1.0 -- "+goldenDir) {
		t.Errorf("List = %v", err)
	}
}

// TestUsageErrorCarriesTheParserMessage: the flag parser has printed the
// refusal already, and the error run sorts to exit 2 keeps its text.
func TestUsageErrorCarriesTheParserMessage(t *testing.T) {
	err := &usageError{errors.New("flag provided but not defined: -x")}
	if err.Error() != "flag provided but not defined: -x" {
		t.Fatalf("Error = %q", err.Error())
	}
}

// TestMemberDigestRefusesABrokenArchive: an archive that is not there, and
// a gzip stream whose tar inside is cut short, are errors and never a
// digest.
func TestMemberDigestRefusesABrokenArchive(t *testing.T) {
	dir := t.TempDir()
	if _, err := memberDigest(filepath.Join(dir, "absent.tar.gz"), "luxd"); err == nil {
		t.Error("an absent archive has a digest")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(bytes.Repeat([]byte{'x'}, 100)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.tar.gz")
	if err := os.WriteFile(short, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := memberDigest(short, "luxd"); err == nil || strings.Contains(err.Error(), "no member") {
		t.Errorf("a tar cut short: %v", err)
	}
}
