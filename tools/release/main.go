// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command release holds the two checks of spec 017 the release pipeline
// runs from the checkout and a maintainer runs before a tag:
//
//	go run ./tools/release promise -previous v0.1.0 -tag v0.2.0
//	go run ./tools/release images -dist dist -extracted out/images
//
// promise diffs the promised surfaces of the tree, the manifest schema's
// golden outputs, the /v1 routes and schemas of api/openapi.yaml, the
// error code table, the LUX_* variable table, the event type table, and
// the usage record's members, against the same files at the previous
// tag, classifies each difference as patch, minor, or major by the
// version promise table, and fails when the tag's own bump is smaller
// than the largest class it found. Before v1.0.0 a minor may break a
// row with a CHANGELOG entry, so a major difference asks for a minor
// bump there.
//
// images compares the binaries extracted from the two published images,
// one per architecture, with the binaries the pipeline built and the
// archives it checksummed, so an image carries the bytes the archive does.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the subcommand: 0 when the check holds, 1 when a
// promise is broken, an image differs, or a file cannot be read, 2 on a
// usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "release: a subcommand is required: promise or images")
		return 2
	}
	var err error
	switch args[0] {
	case "promise":
		err = promiseCmd(args[1:], stdout, stderr)
	case "images":
		err = imagesCmd(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "release: unknown subcommand %q; promise and images are the two\n", args[0])
		return 2
	}
	var usage *usageError
	switch {
	case errors.As(err, &usage):
		return 2
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "release: %v\n", err)
		return 1
	}
	return 0
}

// usageError is a flag the parser refused; the flag package has already
// printed it.
type usageError struct{ err error }

func (u *usageError) Error() string { return u.err.Error() }

// The files the promise reads, by surface.
const (
	variablesFile = "specs/002-repository-scaffold.md"
	codesFile     = "internal/api/errors.go"
	eventsFile    = "internal/events/event.go"
	recordFile    = "metering/record.go"
	openAPIFile   = "api/openapi.yaml"
	goldenDir     = "manifest/testdata/v1/accepted"
)

// Surface is the promised surfaces of one tree reduced to sets: what a
// later build may only add to within a major.
type Surface struct {
	// Variables are the LUX_* names of the configuration table.
	Variables map[string]bool
	// Codes are the error codes of the table.
	Codes map[string]bool
	// Events are the event types of the table.
	Events map[string]bool
	// RecordFields are the JSON members of the usage record's types.
	RecordFields map[string]bool
	// Goldens map each golden output of the accepted corpus to the digest
	// of its bytes: a changed default changes the digest.
	Goldens map[string]string
	// API is every "METHOD /path" and every "Schema.property" of the
	// OpenAPI document.
	API map[string]bool
}

// Tree reads files at one point in history: the working tree, or a git
// ref.
type Tree interface {
	Read(name string) ([]byte, error)
	// List names every file under dir, slash-separated, relative to the
	// tree's root, sorted.
	List(dir string) ([]string, error)
}

// dirTree is a directory on disk.
type dirTree string

func (d dirTree) Read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(d), filepath.FromSlash(name)))
}

func (d dirTree) List(dir string) ([]string, error) {
	var out []string
	root := filepath.Join(string(d), filepath.FromSlash(dir))
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(string(d), p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// gitTree is a ref of the repository at root, read with git show and
// git ls-tree so no checkout is needed.
type gitTree struct{ root, ref string }

func (g gitTree) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", g.root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g gitTree) Read(name string) ([]byte, error) {
	return g.git("show", g.ref+":"+name)
}

func (g gitTree) List(dir string) ([]string, error) {
	out, err := g.git("ls-tree", "-r", "--name-only", g.ref, "--", dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names, nil
}

var (
	variable    = regexp.MustCompile(`LUX_[A-Z0-9_]*[A-Z0-9]`)
	code        = regexp.MustCompile(`Code = "([a-z_]+)"`)
	eventType   = regexp.MustCompile(`= "([a-z]+\.[a-z_]+)"`)
	recordField = regexp.MustCompile("json:\"([A-Za-z0-9]+)")
)

// Read reduces one tree to its Surface.
func Read(t Tree) (Surface, error) {
	s := Surface{Variables: map[string]bool{}, Codes: map[string]bool{}, Events: map[string]bool{}, RecordFields: map[string]bool{}, Goldens: map[string]string{}, API: map[string]bool{}}
	spec, err := t.Read(variablesFile)
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(spec), "\n") {
		if !strings.HasPrefix(line, "| `LUX_") {
			continue
		}
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "| "), " |")
		for _, v := range variable.FindAllString(cell, -1) {
			s.Variables[v] = true
		}
	}
	if err := collect(t, codesFile, code, s.Codes); err != nil {
		return s, err
	}
	if err := collect(t, eventsFile, eventType, s.Events); err != nil {
		return s, err
	}
	if err := collect(t, recordFile, recordField, s.RecordFields); err != nil {
		return s, err
	}
	goldens, err := t.List(goldenDir)
	if err != nil {
		return s, err
	}
	for _, name := range goldens {
		if !strings.HasSuffix(name, ".golden.json") {
			continue
		}
		data, err := t.Read(name)
		if err != nil {
			return s, err
		}
		s.Goldens[name] = digest(data)
	}
	doc, err := t.Read(openAPIFile)
	if err != nil {
		return s, err
	}
	if err := readAPI(doc, s.API); err != nil {
		return s, fmt.Errorf("%s: %w", openAPIFile, err)
	}
	return s, nil
}

// collect adds every first capture of re in the file to set.
func collect(t Tree, name string, re *regexp.Regexp, set map[string]bool) error {
	data, err := t.Read(name)
	if err != nil {
		return err
	}
	for _, m := range re.FindAllSubmatch(data, -1) {
		set[string(m[1])] = true
	}
	return nil
}

// readAPI reads the routes and the schema properties of the document.
func readAPI(doc []byte, set map[string]bool) error {
	var api struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(doc, &api); err != nil {
		return err
	}
	for p, ops := range api.Paths {
		for method := range ops {
			switch method {
			case "get", "put", "post", "delete", "patch", "head", "options":
				set[strings.ToUpper(method)+" "+p] = true
			}
		}
	}
	for name, schema := range api.Components.Schemas {
		for prop := range schema.Properties {
			set[name+"."+prop] = true
		}
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Class is how large a version bump a difference asks for.
type Class int

// The three classes, ordered.
const (
	Patch Class = iota
	Minor
	Major
)

func (c Class) String() string {
	switch c {
	case Major:
		return "major"
	case Minor:
		return "minor"
	default:
		return "patch"
	}
}

// Difference is one change between two surfaces with its class.
type Difference struct {
	Class   Class
	Surface string
	Item    string
	Change  string
}

func (d Difference) String() string {
	return fmt.Sprintf("%s: %s %s %s", d.Class, d.Surface, d.Item, d.Change)
}

// Compare classifies every difference between prev and next by the
// version promise table: a removal of a variable, a code, an event
// type, a record member, a route, or a schema property is major; an
// addition of any of them is minor; a golden output that changed or
// disappeared is major, since a default or a rule changed under a
// manifest that was accepted; a new golden is minor.
func Compare(prev, next Surface) []Difference {
	var out []Difference
	sets := []struct {
		name       string
		prev, next map[string]bool
	}{
		{"variable", prev.Variables, next.Variables},
		{"error code", prev.Codes, next.Codes},
		{"event type", prev.Events, next.Events},
		{"record member", prev.RecordFields, next.RecordFields},
		{"api", prev.API, next.API},
	}
	for _, s := range sets {
		for _, item := range sortedKeys(s.prev) {
			if !s.next[item] {
				out = append(out, Difference{Major, s.name, item, "removed"})
			}
		}
		for _, item := range sortedKeys(s.next) {
			if !s.prev[item] {
				out = append(out, Difference{Minor, s.name, item, "added"})
			}
		}
	}
	for _, name := range sortedKeys(prev.Goldens) {
		switch got, ok := next.Goldens[name]; {
		case !ok:
			out = append(out, Difference{Major, "golden", name, "removed"})
		case got != prev.Goldens[name]:
			out = append(out, Difference{Major, "golden", name, "changed: an accepted manifest resolves differently"})
		}
	}
	for _, name := range sortedKeys(next.Goldens) {
		if _, ok := prev.Goldens[name]; !ok {
			out = append(out, Difference{Minor, "golden", name, "added"})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Required is the largest class among the differences, Patch for none.
func Required(diffs []Difference) Class {
	c := Patch
	for _, d := range diffs {
		c = max(c, d.Class)
	}
	return c
}

// semver is a release tag's three numbers.
type semver struct{ major, minor, patch int }

func parseTag(tag string) (semver, error) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if !strings.HasPrefix(tag, "v") || len(parts) != 3 {
		return semver{}, fmt.Errorf("%q is not a release tag vX.Y.Z", tag)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return semver{}, fmt.Errorf("%q is not a release tag vX.Y.Z", tag)
		}
		n[i] = v
	}
	return semver{n[0], n[1], n[2]}, nil
}

// Bump is the class of the step from prev to tag, and an error when tag
// is not later than prev.
func Bump(prev, tag string) (Class, error) {
	a, err := parseTag(prev)
	if err != nil {
		return Patch, err
	}
	b, err := parseTag(tag)
	if err != nil {
		return Patch, err
	}
	switch {
	case b.major > a.major:
		return Major, nil
	case b.major == a.major && b.minor > a.minor:
		return Minor, nil
	case b.major == a.major && b.minor == a.minor && b.patch > a.patch:
		return Patch, nil
	}
	return Patch, fmt.Errorf("%s is not later than %s", tag, prev)
}

// Check holds the tag to the differences: the bump from prev must be at
// least the class the differences ask for, where before v1.0.0 a minor
// satisfies a major difference, since the table binds from v1.0.0.
func Check(prev, tag string, diffs []Difference) error {
	bump, err := Bump(prev, tag)
	if err != nil {
		return err
	}
	need := Required(diffs)
	v, _ := parseTag(tag)
	if need == Major && v.major == 0 && bump == Minor {
		return nil
	}
	if bump >= need {
		return nil
	}
	var lines []string
	for _, d := range diffs {
		if d.Class == need {
			lines = append(lines, "  "+d.String())
		}
	}
	return fmt.Errorf("%s is a %s bump from %s, and the tree asks for a %s one:\n%s", tag, bump, prev, need, strings.Join(lines, "\n"))
}

// promiseCmd is the promise subcommand.
func promiseCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release promise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("C", ".", "the repository root")
	previous := fs.String("previous", "", "the previous release tag, or none for a first release")
	tag := fs.String("tag", "", "the tag being cut")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if *tag == "" {
		_, _ = fmt.Fprintln(stderr, "release promise: -tag is required")
		return &usageError{errors.New("-tag is required")}
	}
	if _, err := parseTag(*tag); err != nil {
		return err
	}
	if *previous == "" || *previous == "none" {
		_, _ = fmt.Fprintf(stdout, "promise: %s is the first release; nothing to compare it with\n", *tag)
		return nil
	}
	return promise(dirTree(*root), gitTree{*root, *previous}, *previous, *tag, stdout)
}

// promise reads both trees, prints every difference, and holds the tag.
func promise(next, prev Tree, previousTag, tag string, stdout io.Writer) error {
	before, err := Read(prev)
	if err != nil {
		return fmt.Errorf("reading %s: %w", previousTag, err)
	}
	after, err := Read(next)
	if err != nil {
		return fmt.Errorf("reading the tree: %w", err)
	}
	diffs := Compare(before, after)
	for _, d := range diffs {
		_, _ = fmt.Fprintf(stdout, "promise: %s\n", d)
	}
	if err := Check(previousTag, tag, diffs); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "promise: %d difference(s) since %s ask for a %s bump at most, and %s is one\n", len(diffs), previousTag, Required(diffs), tag)
	return nil
}

// The binaries an image carries and the architectures the release builds.
var (
	imageBinaries = []string{"luxd", "lux-stubs"}
	architectures = []string{"amd64", "arm64"}
)

// imagesCmd is the images subcommand.
func imagesCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release images", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dist := fs.String("dist", "dist", "the directory of built binaries <name>_linux_<arch> and archives")
	extracted := fs.String("extracted", "", "the directory of binaries extracted from the images, <name>_linux_<arch>")
	if err := fs.Parse(args); err != nil {
		return &usageError{err}
	}
	if *extracted == "" {
		_, _ = fmt.Fprintln(stderr, "release images: -extracted is required")
		return &usageError{errors.New("-extracted is required")}
	}
	return images(*dist, *extracted, stdout)
}

// images compares every extracted binary with the built one and, where
// an archive carries that binary, with the archive's member.
func images(dist, extracted string, stdout io.Writer) error {
	var problems []string
	for _, binary := range imageBinaries {
		for _, arch := range architectures {
			name := binary + "_linux_" + arch
			got, err := fileDigest(filepath.Join(extracted, name))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: not extracted from the image: %v", name, err))
				continue
			}
			built, err := fileDigest(filepath.Join(dist, name))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: not built: %v", name, err))
				continue
			}
			if got != built {
				problems = append(problems, fmt.Sprintf("%s: the image carries %s and the build produced %s", name, got, built))
				continue
			}
			archives, _ := filepath.Glob(filepath.Join(dist, binary+"_*_linux_"+arch+".tar.gz"))
			for _, archive := range archives {
				member, err := memberDigest(archive, binary)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(archive), err))
					continue
				}
				if member != got {
					problems = append(problems, fmt.Sprintf("%s: the image carries %s and the archive %s", name, got, member))
				}
			}
			_, _ = fmt.Fprintf(stdout, "images: %s %s matches the build and %d archive(s)\n", name, got[:12], len(archives))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// fileDigest is the SHA-256 of a file.
func fileDigest(name string) (string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

// memberDigest is the SHA-256 of one member of a gzipped tar archive.
func memberDigest(archive, member string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("no member %s in the archive", member)
		}
		if err != nil {
			return "", err
		}
		if path.Base(h.Name) == member && h.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				return "", err
			}
			return digest(data), nil
		}
	}
}
