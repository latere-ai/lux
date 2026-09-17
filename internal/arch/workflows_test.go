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
		Permissions map[string]string `yaml:"permissions"`
		Needs       any               `yaml:"needs"`
		Steps       []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
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
	// The organisation refuses a pull request an Action opens, so the
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
		t.Error("the fixture job runs gh pr create; the organisation forbids an Action from opening a pull request, so the job pushes the branch and stops")
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
	if !strings.Contains(string(compose), "/lux:${LUX_VERSION:-latest}") {
		t.Error("compose.yaml does not run the gateway image as <owner>/lux")
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
