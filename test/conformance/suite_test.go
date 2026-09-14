// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// knownDrift names the OpenAPI drifts of the reference server as this
// tree wires it, by finding, each with the spec that owes the fix. The
// package's own runs tolerate them and TestReferenceServerConforms fails
// when one stops appearing, so the ledger shrinks with the fixes and
// never hides a finding that came back. Run tolerates nothing.
var knownDrift = map[string]string{
	// Empty since the targetDialect enum of the served document admits the
	// empty value a refused request's record carries (spec 011's generator,
	// fixed 2026-09-14). An entry returns here only with a finding that
	// names the spec owing its fix.
}

// knownDriftCases are the cases the drift above reddens in a run that
// tolerates nothing, the ones TestExternalInvocation expects red.
var knownDriftCases []string

// referenceSkips are the cases a server mode run with stubs skips: the
// file mode's one case and the fixture group before the first release.
var referenceSkips = []string{"case003PreviousReleaseManifests", "case009PreviousReleaseRecords", "case011ReadOnlyInFileMode"}

// TestReferenceServerConforms runs the whole suite, stubs included,
// against the reference server assembled in process: the proof that the
// suite is green against luxd's own wiring, but for the ledger above,
// and the run the mutation check reddens one capability at a time.
func TestReferenceServerConforms(t *testing.T) {
	s := startServer(t, serverOptions{stubs: true})
	rep := run(t, s.config("alice"), tolerate(knownDrift))
	if dropped := rep.droppedGroups(); len(dropped) != 0 {
		t.Errorf("groups dropped against the reference server: %v", dropped)
	}
	if got := rep.skippedCases(); !slices.Equal(got, referenceSkips) {
		t.Errorf("skipped %v, want %v", got, referenceSkips)
	}
	if failed := failedCases(rep); len(failed) != 0 {
		t.Errorf("failed against the reference server: %v", failed)
	}
	for finding, owner := range knownDrift {
		if !rep.drifts[finding] {
			t.Errorf("the drift %q was not seen; drop it from knownDrift, since %s no longer drifts", finding, owner)
		}
	}
}

// failedCases lists the cases that failed, by bare name, sorted.
func failedCases(rep *report) []string {
	rep.mu.Lock()
	defer rep.mu.Unlock()
	var out []string
	for name, o := range rep.outcomes {
		if o == failed {
			out = append(out, name[strings.LastIndex(name, "/")+1:])
		}
	}
	slices.Sort(out)
	return out
}

// TestContractSkipsWithoutAURL: with LUX_TEST_URL unset the suite skips
// with one line naming the variable and the run passes.
func TestContractSkipsWithoutAURL(t *testing.T) {
	_, skip := fromEnv(func(string) string { return "" })
	if skip == "" || strings.Contains(skip, "\n") || !strings.Contains(skip, EnvURL) {
		t.Errorf("the skip line is %q", skip)
	}
	t.Setenv(EnvURL, "")
	if !t.Run("TestContract", TestContract) {
		t.Error("TestContract failed without a URL")
	}
}

// TestSkipListIsExactlyTheStubCases: without stubs the suite is green
// and skips exactly the stub table, each naming the variable.
func TestSkipListIsExactlyTheStubCases(t *testing.T) {
	s := startServer(t, serverOptions{})
	rep := run(t, s.config("alice"), tolerate(knownDrift))
	want := slices.Concat(stubCases(), referenceSkips)
	slices.Sort(want)
	if got := rep.skippedCases(); !slices.Equal(got, want) {
		t.Errorf("skipped %v, want %v", got, want)
	}
	for name, reason := range rep.skips {
		if slices.Contains(stubCases(), name[strings.LastIndex(name, "/")+1:]) && !strings.Contains(reason, EnvStubsURL) {
			t.Errorf("%s skipped with %q, which names no %s", name, reason, EnvStubsURL)
		}
	}
	if failed := failedCases(rep); len(failed) != 0 {
		t.Errorf("failed without stubs: %v", failed)
	}
	if dropped := rep.droppedGroups(); len(dropped) != 0 {
		t.Errorf("groups dropped without stubs: %v", dropped)
	}
}

// TestSkipListWithoutAToken: with a URL and no token the suite is green
// on the cases that need no bearer and drops the groups that do, each
// naming the variable.
func TestSkipListWithoutAToken(t *testing.T) {
	s := startServer(t, serverOptions{})
	cfg := s.config("alice")
	cfg.Token, cfg.Subject = nil, ""
	rep := run(t, cfg, tolerate(knownDrift))
	if got := rep.droppedGroups(); !slices.Equal(got, []string{"doors", "keys", "manifest", "usage"}) {
		t.Errorf("dropped %v", got)
	}
	for g, reason := range rep.dropped {
		if !strings.Contains(reason, EnvToken) {
			t.Errorf("the %s group was dropped with %q, which names no %s", g, reason, EnvToken)
		}
	}
	if failed := failedCases(rep); len(failed) != 0 {
		t.Errorf("failed without a token: %v", failed)
	}
	ran := rep.ran()
	for _, want := range []string{"case011WellKnown", "case011NoCORS", "case011OpenAPIValidatesEveryResponse", "case006UnauthenticatedControlPlane"} {
		if !slices.Contains(ran, want) {
			t.Errorf("%s did not run without a token", want)
		}
	}
	for _, tc := range cases {
		if slices.Contains(ran, tc.name) && tc.bearer {
			t.Errorf("%s ran without a token and needs one", tc.name)
		}
	}
}

// TestFileModeSkipList: against a file mode server the api group's read
// half runs on the internal listener, its write half skips naming
// read_only, the doors and keys groups are dropped naming read_only, and
// without LUX_TEST_INTERNAL_URL the api group is dropped naming it.
func TestFileModeSkipList(t *testing.T) {
	dir, getenv := fileModeDir(t)
	s := startServer(t, serverOptions{fileMode: true, dir: dir, getenv: getenv})
	rep := run(t, Config{URL: s.URL(), InternalURL: s.internal.URL}, tolerate(knownDrift))
	if got := rep.droppedGroups(); !slices.Equal(got, []string{"doors", "identity", "keys", "manifest", "usage"}) {
		t.Errorf("dropped %v", got)
	}
	for _, g := range []string{"doors", "keys"} {
		if !strings.Contains(rep.dropped[g], "read_only") {
			t.Errorf("the %s group was dropped with %q, which names no read_only", g, rep.dropped[g])
		}
	}
	if failed := failedCases(rep); len(failed) != 0 {
		t.Errorf("failed in file mode: %v", failed)
	}
	var wantRan, wantSkipped []string
	for _, tc := range apiCases {
		if tc.mode == serverMode {
			wantSkipped = append(wantSkipped, tc.name)
		} else {
			wantRan = append(wantRan, tc.name)
		}
	}
	slices.Sort(wantRan)
	if got := rep.ran(); !slices.Equal(got, wantRan) {
		t.Errorf("ran %v, want %v", got, wantRan)
	}
	for _, name := range wantSkipped {
		if reason := rep.skips["api/"+name]; !strings.Contains(reason, "read_only") {
			t.Errorf("%s skipped with %q, which names no read_only", name, reason)
		}
	}
	t.Run("without the internal listener", func(t *testing.T) {
		rep := run(t, Config{URL: s.URL()}, tolerate(knownDrift))
		if !strings.Contains(rep.dropped["api"], EnvInternalURL) {
			t.Errorf("the api group was dropped with %q, which names no %s", rep.dropped["api"], EnvInternalURL)
		}
	})
	// The public entry point over the same server: a file mode run reads
	// no record, so nothing in it drifts and Run is green as it is.
	if !t.Run("Run", func(t *testing.T) { Run(t, Config{URL: s.URL(), InternalURL: s.internal.URL}) }) {
		t.Error("Run failed against the file mode server")
	}
}

// TestSuiteCleansUpExactlyItsOwn: an object created before the run is
// untouched, and after the run nothing carries the run's label or its
// prefix.
func TestSuiteCleansUpExactlyItsOwn(t *testing.T) {
	s := startServer(t, serverOptions{stubs: true})
	token := s.mint("alice")
	before := s.control(t, token, http.MethodPut, "/v1/budgets/pre-existing", map[string]any{"spec": budgetSpec("9")})
	if before.Status != http.StatusCreated {
		t.Fatalf("the object created before the run: %d %s", before.Status, excerpt(before.Body))
	}
	var rep *report
	t.Run("run", func(t *testing.T) { rep = run(t, s.config("alice"), tolerate(knownDrift)) })
	if failed := failedCases(rep); len(failed) != 0 {
		t.Fatalf("failed: %v", failed)
	}
	for _, kind := range kindOrder {
		for _, query := range []string{"?label=conformance=" + rep.run + "&limit=200", "?limit=200"} {
			resp := s.control(t, token, http.MethodGet, "/v1/"+plurals[kind]+query, nil)
			if resp.Status != http.StatusOK {
				t.Fatalf("GET /v1/%s%s: %d %s", plurals[kind], query, resp.Status, excerpt(resp.Body))
			}
			for _, it := range arr(resp.json(t), "items") {
				name := str(it, "metadata.name")
				if strings.HasPrefix(name, "conf-"+rep.run+"-") || str(it, "metadata.labels.conformance") == rep.run {
					t.Errorf("%s %s survived the run", kind, name)
				}
			}
		}
	}
	after := s.control(t, token, http.MethodGet, "/v1/budgets/pre-existing", nil)
	if after.Status != http.StatusOK || str(after.json(t), "status.version") != str(before.json(t), "status.version") {
		t.Errorf("the object created before the run: %d %s", after.Status, excerpt(after.Body))
	}
}

// TestConcurrentRuns: two runs against one server, as two subjects, both
// pass.
func TestConcurrentRuns(t *testing.T) {
	s := startServer(t, serverOptions{})
	for _, subject := range []string{"alice", "bob"} {
		t.Run(subject, func(t *testing.T) {
			t.Parallel()
			rep := run(t, s.config(subject), tolerate(knownDrift))
			if failed := failedCases(rep); len(failed) != 0 {
				t.Errorf("failed: %v", failed)
			}
			if dropped := rep.droppedGroups(); len(dropped) != 0 {
				t.Errorf("dropped: %v", dropped)
			}
		})
	}
}

// packageDir is this package's directory, found from this file.
func packageDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	return filepath.Dir(file)
}

// moduleRoot is the module's root directory.
func moduleRoot(t testing.TB) string {
	t.Helper()
	root := filepath.Clean(filepath.Join(packageDir(t), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	return root
}

// testEvent is one line of go test -json.
type testEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

// goTest runs the toolchain's go test with the arguments and environment
// and returns its events; a run that produced none fails with what the
// toolchain wrote.
func goTest(t testing.TB, dir string, env []string, args ...string) []testEvent {
	t.Helper()
	cmd := exec.Command("go", append([]string{"test"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var events []testEvent
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e testEvent
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Action != "" {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		t.Fatalf("go test %s produced no events: %v\n%s\n%s", strings.Join(args, " "), err, stderr.String(), out)
	}
	return events
}

// failedLeaves lists the cases go test reported failed, by bare name,
// sorted: the third segment of a test name when it is a case.
func failedLeaves(events []testEvent) []string {
	var out []string
	for _, e := range events {
		parts := strings.Split(e.Test, "/")
		if e.Action == "fail" && len(parts) == 3 && strings.HasPrefix(parts[2], "case") {
			out = append(out, parts[2])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestExternalInvocation: the documented command runs against an
// external URL with the variables alone, every group runs, and the only
// failures are the ledger's, since Run tolerates none of them; and the
// suite's own source reads no file, so the only files it reads are the
// embedded corpus and fixtures.
func TestExternalInvocation(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(packageDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(packageDir(t), e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "os" || path == "os/exec" || path == "path/filepath" || path == "io/ioutil" {
				t.Errorf("%s imports %s, and the suite reads no file of the caller's", e.Name(), path)
			}
		}
	}

	s := startServer(t, serverOptions{stubs: true})
	env := []string{
		EnvURL + "=" + s.URL(), EnvToken + "=" + s.mint("alice"),
		EnvIssuerURL + "=" + s.iss.URL(), EnvStubsURL + "=" + s.stubs.URL(),
	}
	events := goTest(t, moduleRoot(t), env, "latere.ai/x/lux/test/conformance", "-run", "^TestContract$", "-count=1", "-json")
	seen := map[string]bool{}
	for _, e := range events {
		if parts := strings.Split(e.Test, "/"); len(parts) == 2 && parts[0] == "TestContract" && e.Action == "run" {
			seen[parts[1]] = true
		}
	}
	for _, g := range groups {
		if !seen[g] {
			t.Errorf("the %s group did not run under the documented command", g)
		}
	}
	if got := failedLeaves(events); !slices.Equal(got, knownDriftCases) {
		t.Errorf("failed under the documented command: %v, want the ledger's %v", got, knownDriftCases)
	}
}

// mutations is spec 018's mutation table: the build tag and the cases
// it must redden, and no others.
var mutations = []struct {
	tag   string
	cases []string
}{
	{"mut_ifmatch", []string{"case011Preconditions"}},
	{"mut_default", []string{"case003DefaultsAreVisible"}},
	{"mut_loss", []string{"case004TranslationLoss"}},
	{"mut_unknownfield", []string{"case003UnknownField"}},
	{"mut_unpriced", []string{"case007UnpricedUnderABudget"}},
	{"mut_retryafter", []string{"case007RateLimited"}},
	{"mut_keyvalue", []string{"case011SecretsNeverInResponses"}},
	{"mut_forwardcred", []string{"case004CallerCredentialsNeverForwarded"}},
}

// TestSuiteCatchesADroppedCapability builds the reference server's run
// with one capability removed, through go test -tags=<tag> from the
// toolchain on PATH, and asserts that exactly the row's cases fail. A
// mutation that reddens more says two cases assert one thing; one that
// reddens nothing says the capability is not covered.
func TestSuiteCatchesADroppedCapability(t *testing.T) {
	for _, m := range mutations {
		t.Run(m.tag, func(t *testing.T) {
			t.Parallel()
			events := goTest(t, packageDir(t), nil, "-tags="+m.tag, "-count=1", "-json", "-run", "^TestReferenceServerConforms$", ".")
			if got := failedLeaves(events); !slices.Equal(got, m.cases) {
				t.Errorf("under %s the failing cases are %v, want exactly %v", m.tag, got, m.cases)
			}
		})
	}
}
