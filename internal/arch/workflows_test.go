// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// imageRef matches a registry reference under ghcr.io and captures the
// namespace segment: a literal there would be a fixed namespace.
var imageRef = regexp.MustCompile(`ghcr\.io/([^/\s"'` + "`" + `]+)/`)

// derived reports whether a namespace segment is computed at run time
// or is a placeholder for a reader, rather than a literal account.
func derived(segment string) bool {
	return strings.HasPrefix(segment, "${") || strings.HasPrefix(segment, "$") || strings.Contains(segment, "<") || segment == "OWNER"
}

// TestReleasePublishesUnderTheOwnersNamespace is spec 001's invariant 8
// as spec 017's test: no workflow, deploy manifest, document, script,
// or Dockerfile names a fixed image namespace under ghcr.io; the
// published references derive from github.repository_owner at run time,
// and the release workflow reads that variable.
func TestReleasePublishesUnderTheOwnersNamespace(t *testing.T) {
	dir := root(t)
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
		rel, _ := filepath.Rel(dir, path)
		if filepath.Dir(path) == filepath.Join(dir, "internal", "arch") && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range imageRef.FindAllStringSubmatch(line, -1) {
				if !derived(m[1]) {
					hits = append(hits, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+": "+m[0])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Errorf("a fixed image namespace: %s", h)
	}
	release, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(release), "GITHUB_REPOSITORY_OWNER") {
		t.Error("release.yml does not derive the namespace from the repository owner")
	}
	if strings.Contains(string(release), "tr '[:upper:]' '[:lower:]'") == false {
		t.Error("release.yml does not lower the owner, which an image reference requires")
	}
}

// workflow is the part of a workflow file these tests read.
type workflow struct {
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		If              string            `yaml:"if"`
		ContinueOnError any               `yaml:"continue-on-error"`
		Permissions     map[string]string `yaml:"permissions"`
		Needs           any               `yaml:"needs"`
		Steps           []struct {
			ContinueOnError any               `yaml:"continue-on-error"`
			Name            string            `yaml:"name"`
			Uses            string            `yaml:"uses"`
			Run             string            `yaml:"run"`
			Env             map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) (workflow, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w, string(data)
}

// pinned matches a third-party action pinned by a full commit with its
// version in a comment, as every workflow of this repository pins.
var pinned = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$`)

// TestReleaseWorkflowNeverPushesToTheDefaultBranch is spec 017's row for
// the fixture and the pipeline's shape: no step of release.yml pushes
// to the default branch, the fixture job pushes its own branch and
// opens no pull request, permissions are declared per job with contents
// read as the workflow's default and contents write on publish and
// fixture alone and no other scope on fixture, the jobs run in the
// spec's order, and every third-party action is pinned by commit.
func TestReleaseWorkflowNeverPushesToTheDefaultBranch(t *testing.T) {
	w, text := readWorkflow(t, "release.yml")
	if w.Permissions["contents"] != "read" || len(w.Permissions) != 1 {
		t.Errorf("the workflow's default permissions are %v, want contents: read alone", w.Permissions)
	}
	want := []string{"gate-green", "build", "conformance", "publish", "install-release", "release-verify", "fixture"}
	for _, name := range want {
		if _, ok := w.Jobs[name]; !ok {
			t.Errorf("release.yml has no %s job", name)
		}
	}
	if len(w.Jobs) != len(want) {
		t.Errorf("release.yml has %d jobs, want the %d of the spec", len(w.Jobs), len(want))
	}
	needs := func(name string) []string {
		switch n := w.Jobs[name].Needs.(type) {
		case string:
			return []string{n}
		case []any:
			var out []string
			for _, v := range n {
				out = append(out, v.(string))
			}
			return out
		}
		return nil
	}
	for job, before := range map[string]string{"build": "gate-green", "conformance": "build", "publish": "conformance", "install-release": "publish", "release-verify": "publish", "fixture": "publish"} {
		if !slices.Contains(needs(job), before) {
			t.Errorf("%s does not run after %s: needs %v", job, before, needs(job))
		}
	}
	pushes := regexp.MustCompile(`git push[^\n]*`)
	for name, job := range w.Jobs {
		if len(job.Permissions) == 0 {
			t.Errorf("%s declares no permissions; every job declares its own", name)
		}
		write := job.Permissions["contents"] == "write"
		if write != (name == "publish" || name == "fixture") {
			t.Errorf("%s has contents: %s; publish and fixture alone write", name, job.Permissions["contents"])
		}
		for _, step := range job.Steps {
			for _, push := range pushes.FindAllString(step.Run, -1) {
				if name != "fixture" {
					t.Errorf("%s pushes: %s", name, push)
				}
				if strings.Contains(push, "main") || strings.Contains(push, "default_branch") || !strings.Contains(push, "HEAD:refs/heads/conformance/fixture-") {
					t.Errorf("fixture pushes %q, want its own conformance/fixture-<tag> branch and never the default branch", push)
				}
			}
			if step.Uses != "" && !strings.HasPrefix(step.Uses, "./") && !pinned.MatchString(step.Uses) {
				t.Errorf("%s uses %s, which is not pinned by a full commit", name, step.Uses)
			}
		}
	}
	// The organization refuses a pull request an Action opens, so the
	// job pushes conformance/fixture-<tag> and stops; the maintainer
	// merges the branch by hand.
	fixture := w.Jobs["fixture"]
	var opensPR, pushesBranch bool
	for _, step := range fixture.Steps {
		opensPR = opensPR || strings.Contains(step.Run, "gh pr create")
		pushesBranch = pushesBranch || strings.Contains(step.Run, "HEAD:refs/heads/conformance/fixture-")
	}
	if !pushesBranch {
		t.Error("the fixture job pushes no conformance/fixture-<tag> branch")
	}
	if opensPR {
		t.Error("the fixture job runs gh pr create; the organization forbids an Action from opening a pull request, so the job pushes the branch and stops")
	}
	if len(fixture.Permissions) != 1 || fixture.Permissions["contents"] != "write" {
		t.Errorf("fixture declares %v, want contents: write alone and no pull-requests scope", fixture.Permissions)
	}
	if strings.Contains(text, "pull-requests:") {
		t.Error("release.yml declares a pull-requests permission; no job of it opens a pull request")
	}
	for _, uses := range regexp.MustCompile(`(?m)uses: (\S+)( # v[0-9][^\n]*)?`).FindAllStringSubmatch(text, -1) {
		if uses[2] == "" && !strings.HasPrefix(uses[1], "./") {
			t.Errorf("uses: %s carries no version comment", uses[1])
		}
	}
	// The pipeline's guards: the digests are pushed under no tag, the
	// suite runs before publish tags them, and the notes are the
	// CHANGELOG section through the gate's own rule.
	for _, want := range []string{"push-by-digest=true", "docker buildx imagetools create -t", "go tool lateregate release-notes", "gh release download", "cosign verify-blob", "gh attestation verify", "run-blocks.sh ../docs/install.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	verify, _ := readWorkflow(t, "verify.yml")
	if _, ok := verify.Jobs["install"]; !ok {
		t.Error("verify.yml has no install job walking docs/install.md on every push")
	}
	for name, job := range verify.Jobs {
		for _, step := range job.Steps {
			if step.Uses != "" && !pinned.MatchString(step.Uses) && !strings.HasPrefix(step.Uses, "latere-ai/ci/") {
				t.Errorf("verify.yml %s uses %s, which is not pinned by a full commit", name, step.Uses)
			}
		}
	}
}

// TestRunBlocksRunsTheFencedBlocksInOrder is the block runner of spec
// 017 over a document of its own: the named yaml block is written to
// its file first, the sh blocks run in order as one script so an export
// carries over, a failing block stops the run with its step named, and
// a prose block never runs. It needs bash, which the hermetic run lacks
// and skips by name.
func TestRunBlocksRunsTheFencedBlocksInOrder(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH, so the block runner is not exercised here")
	}
	runner := filepath.Join(root(t), "tools", "docs", "run-blocks.sh")
	dir := t.TempDir()
	doc := filepath.Join(dir, "doc.md")
	text := "# A document\n\n```yaml file=settings.yaml\nname: lux\n```\n\n```sh\nexport GREETING=hello\ntest -f settings.yaml\n```\n\nProse with a block nobody runs:\n\n```\nexit 7\n```\n\n```sh\necho \"$GREETING\" > out.txt\n```\n\n```sh\nfalse\n```\n\n```sh\necho never > never.txt\n```\n"
	if err := os.WriteFile(doc, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", runner, doc)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a failing block did not fail the run:\n%s", out)
	}
	for _, want := range []string{"run-blocks: wrote settings.yaml", "run-blocks: 4 step(s)", "run-blocks: step 1 (", "run-blocks: step 3 ("} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the runner did not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "run-blocks: step 4 (") {
		t.Errorf("the block after the failure ran:\n%s", out)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("out.txt = %q, %v: the export did not carry to the next block", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "never.txt")); err == nil {
		t.Error("the block after the failure wrote its file")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "settings.yaml")); err != nil || string(got) != "name: lux\n" {
		t.Errorf("settings.yaml = %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", runner, "--write", doc)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--write: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("--write ran a step")
	}
	cmd = exec.Command("bash", runner, filepath.Join(dir, "missing.md"))
	if err := cmd.Run(); err == nil {
		t.Error("a missing document did not fail")
	}
	install, err := os.ReadFile(filepath.Join(root(t), "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(install), "\n```sh\n"); n < 6 {
		t.Errorf("docs/install.md has %d sh blocks; the walk from nothing to a request through a door has more", n)
	}
	if !strings.Contains(string(install), "\n```yaml file=kind-lux.yaml\n") {
		t.Error("docs/install.md does not hand the runner its cluster configuration as a named block")
	}
	for _, want := range []string{"LUX_INSTALL_IMAGE", "LUX_INSTALL_MANIFESTS", "luxd check", "lux providers create", "lux keys create", "/openai/v1/chat/completions"} {
		if !strings.Contains(string(install), want) {
			t.Errorf("docs/install.md lacks %q", want)
		}
	}
}

// TestKeyGenerationNamesTheCurve: every openssl command the tree gives a
// reader for an EC key names the curve in the key rather than writing
// its parameters out. LibreSSL, the openssl of macOS, writes the explicit
// parameters unless told ec_param_enc:named_curve, and the local issuer's
// PKCS#8 parser refuses such a key as an unknown curve, so a command
// without the option works on Linux and fails on a Mac.
func TestKeyGenerationNamesTheCurve(t *testing.T) {
	dir := root(t)
	self := filepath.Join(dir, "internal", "arch")
	var commands int
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "ec_paramgen_curve") {
				continue
			}
			commands++
			if !strings.Contains(line, "ec_param_enc:named_curve") {
				t.Errorf("%s:%d generates an EC key without -pkeyopt ec_param_enc:named_curve, which LibreSSL answers with a key the local issuer refuses", filepath.ToSlash(rel), i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if commands == 0 {
		t.Error("no openssl command in the tree generates an EC key; docs/install.md gives one for the local issuer")
	}
}

// exported matches a variable a block exports, at the start of its line.
var exported = regexp.MustCompile(`(?m)^export ([A-Z_][A-Z0-9_]*)=`)

// TestInstallPartOneStartsFromACheckout is spec 035's shape of
// docs/install.md: part 1 comes before part 2, runs the server named by
// LUX_INSTALL_BIN with a local issuer key and a bootstrap directory,
// stops it, and unsets every variable it exported that part 2 does not
// name, so part 2 starts against nothing of part 1. Both install jobs
// start the stub provider on the runner before the walk and name the
// server binary for the walk step.
func TestInstallPartOneStartsFromACheckout(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(root(t), "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	one := strings.Index(text, "\n## Part 1: ")
	two := strings.Index(text, "\n## Part 2: ")
	if one < 0 || two < one {
		t.Fatalf("docs/install.md has part 1 at %d and part 2 at %d; part 1 comes first", one, two)
	}
	partOne, partTwo := text[one:two], text[two:]
	for _, want := range []string{"LUX_INSTALL_BIN", "ec_param_enc:named_curve", "LUX_LOCAL_ISSUER_KEY", "LUX_BOOTSTRAP_DIR", `"$LUX_INSTALL_BIN" serve`, `"$LUX_INSTALL_BIN" token`, "lux keys create", "/openai/v1/chat/completions", `kill "$LUXD_PID"`} {
		if !strings.Contains(partOne, want) {
			t.Errorf("part 1 of docs/install.md lacks %q", want)
		}
	}
	var unset []string
	for line := range strings.SplitSeq(partOne, "\n") {
		if rest, ok := strings.CutPrefix(line, "unset "); ok {
			unset = append(unset, strings.Fields(rest)...)
		}
	}
	for _, m := range exported.FindAllStringSubmatch(partOne, -1) {
		name := m[1]
		if strings.Contains(partTwo, name) {
			continue
		}
		if !slices.Contains(unset, name) {
			t.Errorf("part 1 exports %s, which part 2 does not name, and does not unset it", name)
		}
	}
	for file, job := range map[string]string{"verify.yml": "install", "release.yml": "install-release"} {
		w, _ := readWorkflow(t, file)
		stubs, walk := -1, -1
		for i, step := range w.Jobs[job].Steps {
			if strings.Contains(step.Run, "tools/docs/stubs-local.sh") {
				stubs = i
			}
			if strings.Contains(step.Run, "run-blocks.sh ../docs/install.md") {
				walk = i
				if !strings.HasSuffix(step.Env["LUX_INSTALL_BIN"], "/luxd") {
					t.Errorf("%s %s walks the document with LUX_INSTALL_BIN %q, want the luxd it built or unpacked", file, job, step.Env["LUX_INSTALL_BIN"])
				}
			}
		}
		if stubs < 0 || walk < 0 || stubs > walk {
			t.Errorf("%s %s starts the local stub provider at step %d and walks the document at step %d; the stubs come first", file, job, stubs, walk)
		}
	}
}

// gatewayImageRef matches a registry reference to the gateway image under
// the binary's name: the published image is `ghcr.io/<owner>/lux`, and
// `luxd` is the binary inside it and the Deployment's name, never the
// repository a release pushes to.
var gatewayImageRef = regexp.MustCompile(`(ghcr\.io|REGISTRY[ }]*)/[^/]+/luxd(?:[@:"'\s)]|$)`)

// TestGatewayImageIsLux is spec 017's artifact table as a test: every
// reference to the published gateway image names the repository `lux`,
// in the release workflow, the deploy manifests, compose.yaml, the
// documents, and the specs.
func TestGatewayImageIsLux(t *testing.T) {
	dir := root(t)
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
		if filepath.Dir(path) == filepath.Join(dir, "internal", "arch") && strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		for i, line := range strings.Split(string(data), "\n") {
			if gatewayImageRef.MatchString(line) {
				hits = append(hits, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Errorf("the gateway image is published as lux, not luxd: %s", h)
	}
	release, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(release), `/lux:${GITHUB_REF_NAME}`) {
		t.Error("release.yml does not tag the gateway image as <owner>/lux:<tag>")
	}
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "/lux:${LUX_VERSION:-") {
		t.Error("compose.yaml does not run the gateway image as <owner>/lux")
	}
}

// composeImage matches an image of compose.yaml and captures the
// repository and the tag it falls back to when LUX_VERSION is unset.
var composeImage = regexp.MustCompile(`ghcr\.io/\$\{LUX_OWNER:-[^}]+\}/(lux|lux-stubs):\$\{LUX_VERSION:-([^}]*)\}`)

// releaseVersion is a release tag, the only tag a release publishes an
// image under, and what a release stamp moves.
var releaseVersion = regexp.MustCompile(`v\d+\.\d+\.\d+`)

// composeDefaults maps each image of compose.yaml to its default tag.
func composeDefaults(compose []byte) map[string]string {
	tags := map[string]string{}
	for _, m := range composeImage.FindAllSubmatch(compose, -1) {
		tags[string(m[1])] = string(m[2])
	}
	return tags
}

// TestComposeDefaultsToARelease: a release publishes both images under its
// vX.Y.Z tag and never under `latest`, so `docker compose up` with no
// variable set pulls only when both images default to a release tag. The
// default is the release the quick start names, and a stamp of
// .lateregate.yaml moves both at the cut, so it never lags the newest tag.
func TestComposeDefaultsToARelease(t *testing.T) {
	dir := root(t)
	compose, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tags := composeDefaults(compose)
	for _, repo := range []string{"lux", "lux-stubs"} {
		tag, ok := tags[repo]
		if !ok {
			t.Errorf("compose.yaml runs no %s image with a LUX_VERSION default", repo)
			continue
		}
		if tag == "" || releaseVersion.FindString(tag) != tag {
			t.Errorf("compose.yaml defaults the %s image to %q, a tag no release publishes; want a vX.Y.Z release tag", repo, tag)
		}
	}
	if tags["lux"] != tags["lux-stubs"] {
		t.Errorf("compose.yaml defaults lux to %q and lux-stubs to %q; a release publishes both under one tag", tags["lux"], tags["lux-stubs"])
	}

	quickstart, err := os.ReadFile(filepath.Join(dir, "docs", "quickstart.md"))
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`export LUX_VERSION=(v\d+\.\d+\.\d+)`).FindSubmatch(quickstart); m == nil || string(m[1]) != tags["lux"] {
		t.Errorf("compose.yaml defaults to %q, not the release docs/quickstart.md names", tags["lux"])
	}

	data, err := os.ReadFile(filepath.Join(dir, ".lateregate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Release struct {
			Stamp []struct {
				File    string `yaml:"file"`
				Pattern string `yaml:"pattern"`
			} `yaml:"stamp"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	// Apply the stamps the way `lateregate release` does: each entry must
	// match its file once, the versions inside the match move, and each
	// entry rewrites the file as read, not as an earlier entry left it, so
	// the last entry naming a file is the one that lands.
	const cut = "v999.0.0"
	stamped := compose
	for _, s := range cfg.Release.Stamp {
		if s.File != "compose.yaml" {
			continue
		}
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			t.Fatalf(".lateregate.yaml stamps compose.yaml with %q: %v", s.Pattern, err)
		}
		locs := re.FindAllIndex(compose, -1)
		if len(locs) != 1 {
			t.Fatalf(".lateregate.yaml stamps compose.yaml with %q, which matches %d times; the cut refuses anything but one", s.Pattern, len(locs))
		}
		lo, hi := locs[0][0], locs[0][1]
		stamped = slices.Concat(compose[:lo], releaseVersion.ReplaceAll(compose[lo:hi], []byte(cut)), compose[hi:])
	}
	moved := true
	for repo, tag := range composeDefaults(stamped) {
		if tag != cut {
			moved = false
			t.Errorf("a release cut leaves compose.yaml's %s image at %q; .lateregate.yaml's release.stamp must move it", repo, tag)
		}
	}
	want := bytes.ReplaceAll(compose, []byte("${LUX_VERSION:-"+tags["lux"]+"}"), []byte("${LUX_VERSION:-"+cut+"}"))
	if moved && !bytes.Equal(stamped, want) {
		t.Error("a release cut rewrites more of compose.yaml than its two image defaults")
	}
}

// TestWorkflowOutputsAreWritten holds every output a workflow reads to a
// write of that name in the same file: a step output `steps.<id>.outputs.X`
// needs a `X=` appended to GITHUB_OUTPUT or a variable assigned `=X` that
// is, and a job output `needs.<job>.outputs.X` needs an `X:` entry under
// a job's `outputs:`. A name computed at run time or misspelled on one
// side reads as an empty string and fails only at the first tag; the test
// fails here instead.
func TestWorkflowOutputsAreWritten(t *testing.T) {
	dir := root(t)
	stepReads := regexp.MustCompile(`steps\.[a-z_]+\.outputs\.([a-z_]+)`)
	jobReads := regexp.MustCompile(`needs\.[a-z_]+\.outputs\.([a-z_]+)`)
	for _, name := range []string{"release.yml", "verify.yml"} {
		data, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, m := range stepReads.FindAllStringSubmatch(text, -1) {
			out := m[1]
			if !strings.Contains(text, out+"=") && !strings.Contains(text, "="+out) {
				t.Errorf("%s reads a step output %s that no step writes", name, out)
			}
		}
		declared := regexp.MustCompile(`(?m)^\s+([a-z_]+): \$\{\{ steps\.`)
		keys := map[string]bool{}
		for _, m := range declared.FindAllStringSubmatch(text, -1) {
			keys[m[1]] = true
		}
		for _, m := range jobReads.FindAllStringSubmatch(text, -1) {
			if !keys[m[1]] {
				t.Errorf("%s reads a job output %s that no job declares", name, m[1])
			}
		}
	}
}

// TestReleaseExtractsEachPlatformByItsOwnDigest: a released image is a
// multi-arch index, and `docker create --platform` on the index reference
// stores each platform under the index digest and refuses the second, so
// the workflows resolve a platform's manifest digest and create by that.
func TestReleaseExtractsEachPlatformByItsOwnDigest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "docker create --platform") {
		t.Error("release.yml creates a container from the index reference with --platform")
	}
	if !strings.Contains(string(data), "imagetools inspect --raw") {
		t.Error("release.yml does not resolve a platform's manifest digest from the index")
	}
}

// The two image repositories a release publishes to. `luxd` and
// `lux-stubs` are the binaries; the gateway's binary rides in the `lux`
// repository, and only the stubs' names coincide.
var publishedRepositories = map[string]bool{"lux": true, "lux-stubs": true}

// imageSegment captures the repository segment of a registry reference
// in a workflow, whether the file names it or a shell variable holds it.
var imageSegment = regexp.MustCompile(`(?:ghcr\.io|REGISTRY[ }]*)/[^/\s]+/(\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|[A-Za-z0-9._-]+)`)

// plainWord is a word of a for list that names a thing rather than
// expanding to one, so a list holding a glob or a variable resolves to
// what it names and the rest is left to the reader.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// shellValues is every value a workflow's shell gives one variable: what
// an assignment writes, anywhere on a line, and what a for list iterates.
func shellValues(text, name string) []string {
	var values []string
	assigned := regexp.MustCompile(`(^|[\s;&|(])` + regexp.QuoteMeta(name) + `=([A-Za-z0-9._-]+)`)
	for _, m := range assigned.FindAllStringSubmatch(text, -1) {
		values = append(values, m[2])
	}
	loops := regexp.MustCompile(`(?m)^\s*for\s+` + regexp.QuoteMeta(name) + `\s+in\s+([^;\n]+)`)
	for _, m := range loops.FindAllStringSubmatch(text, -1) {
		for word := range strings.FieldsSeq(m[1]) {
			if plainWord.MatchString(word) {
				values = append(values, word)
			}
		}
	}
	slices.Sort(values)
	return slices.Compact(values)
}

// TestEveryImageReferenceNamesAPublishedRepository is the reading of
// spec 017's artifact table that a literal cannot give: a reference
// assembled from a shell variable asks the registry for whatever the
// variable holds, so the values are resolved and every one of them must
// be a repository this release pushes to. A step that iterates the
// binaries `luxd lux-stubs` and hands the name straight to the registry
// asks for `<owner>/luxd`, which is another repository's image and
// answers DENIED.
func TestEveryImageReferenceNamesAPublishedRepository(t *testing.T) {
	for _, name := range []string{"release.yml", "verify.yml"} {
		data, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for i, line := range strings.Split(text, "\n") {
			for _, m := range imageSegment.FindAllStringSubmatch(line, -1) {
				where := name + ":" + strconv.Itoa(i+1)
				segment := m[1]
				if !strings.HasPrefix(segment, "$") {
					if !publishedRepositories[segment] {
						t.Errorf("%s: %s names %q, which no release publishes", where, m[0], segment)
					}
					continue
				}
				variable := strings.Trim(strings.TrimPrefix(segment, "$"), "{}")
				values := shellValues(text, variable)
				if len(values) == 0 {
					t.Errorf("%s: %s reads %s, which nothing in the file assigns", where, m[0], segment)
					continue
				}
				for _, v := range values {
					if !publishedRepositories[v] {
						t.Errorf("%s: %s reads %s, which holds %v; %q is no repository a release publishes to", where, m[0], segment, values, v)
					}
				}
			}
		}
	}
}

// TestReleaseBodyCheckReadsBothSidesTheSameWay runs the release-verify
// step that holds the release body against the CHANGELOG section, with
// the two `gh` fetches replaced by fixtures. `lateregate release` writes
// `## vX.Y.Z - date` followed by the blank line that stood under
// `## Unreleased`, so the extracted section begins blank; `lateregate
// release-notes`, which writes the body, trims it. A step that trims
// only one side reads that blank line as a difference and fails a
// release whose body is correct, as v0.3.0's run did. The step is the
// file's own text, so a change to it is a change to what runs here.
func TestReleaseBodyCheckReadsBothSidesTheSameWay(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH, so the step's script is not exercised here")
	}
	const stepName = "The body is the CHANGELOG section for the tag"
	w, _ := readWorkflow(t, "release.yml")
	var script string
	for _, step := range w.Jobs["release-verify"].Steps {
		if step.Name == stepName {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatalf("release-verify has no %q step", stepName)
	}
	// Only the two fetches go; the fixtures below stand in for what they
	// download. Their count is asserted, so a reworded fetch fails here
	// rather than leaving a script that reads no fixture and passes.
	var kept []string
	fetches := 0
	for line := range strings.SplitSeq(script, "\n") {
		if strings.Contains(line, "gh release view") || strings.Contains(line, "gh api ") {
			fetches++
			continue
		}
		kept = append(kept, line)
	}
	if fetches != 2 {
		t.Fatalf("%d fetching lines removed from the step, want the 2 `gh` calls; the script has changed shape", fetches)
	}
	fragment := strings.Join(kept, "\n")

	const changelog = "# Changelog\n\n## Unreleased\n\n## v9.9.9 - 2026-09-17\n\n" +
		"- The section under the heading opens with the blank line the\n  release cut leaves there.\n\n- A second note.\n\n" +
		"## v9.8.0 - 2026-09-16\n\n- An older note, which is no part of the tag's section.\n"
	const notes = "- The section under the heading opens with the blank line the\n  release cut leaves there.\n\n- A second note.\n"

	for _, c := range []struct {
		name  string
		body  string
		match bool
	}{
		{"the body as release-notes writes it", notes, true},
		{"a body padded with blank lines of its own", "\n\n" + notes + "\n\n", true},
		{"a body carrying carriage returns", strings.ReplaceAll(notes, "\n", "\r\n"), true},
		{"a body that is not the section", "- Something else entirely.\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", fragment)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GITHUB_REF_NAME=v9.9.9")
			out, err := cmd.CombinedOutput()
			if c.match && err != nil {
				t.Errorf("the step read a correct body as a difference: %v\n%s", err, out)
			}
			if !c.match && err == nil {
				t.Errorf("the step passed a body that is not the section:\n%s", out)
			}
			section, readErr := os.ReadFile(filepath.Join(dir, "section.md"))
			if readErr != nil {
				t.Fatalf("the step wrote no section.md: %v", readErr)
			}
			if string(section) != notes {
				t.Errorf("section.md = %q, want the section trimmed on both ends: %q", section, notes)
			}
		})
	}
}

// TestInstallBlocksParse holds docs/install.md to what CI runs of it:
// tools/docs/run-blocks.sh joins every sh block of the document into one
// bash script, so a block that does not parse fails the whole walk, and a
// quote left open in one block swallows the next. Inside
// "${VAR:?message}" bash reads a single quote as quoting, so an apostrophe
// in such a message opens a quote that another one elsewhere may or may
// not close.
func TestInstallBlocksParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH, so the document's blocks are not parsed here")
	}
	doc, err := os.ReadFile(filepath.Join(root(t), "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	var script strings.Builder
	in := false
	for line := range strings.SplitSeq(string(doc), "\n") {
		switch {
		case line == "```sh":
			in = true
		case line == "```" && in:
			in = false
		case in:
			script.WriteString(line + "\n")
		}
	}
	if script.Len() == 0 {
		t.Fatal("docs/install.md has no sh block")
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(script.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the sh blocks of docs/install.md do not parse as one bash script: %v\n%s", err, stderr.String())
	}
}
