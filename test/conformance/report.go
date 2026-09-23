// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// outcome is what one case came to.
type outcome int

const (
	passed outcome = iota
	failed
	skipped
)

// report is what one Run learned about the server: the outcome of every
// case that ran, why each skipped case skipped, and which groups were
// dropped whole. Run prints it; the package's own tests read it.
type report struct {
	run string

	mu       sync.Mutex
	outcomes map[string]outcome // group/case
	skips    map[string]string  // group/case to the reason
	dropped  map[string]string  // group to the reason
	drifts   map[string]bool    // the tolerated drifts seen, by finding
}

func newReport(run string) *report {
	return &report{run: run, outcomes: map[string]outcome{}, skips: map[string]string{}, dropped: map[string]string{}, drifts: map[string]bool{}}
}

func (r *report) mark(name string, o outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes[name] = o
}

func (r *report) skip(name, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes[name] = skipped
	r.skips[name] = reason
}

func (r *report) drop(group, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped[group] = reason
}

// skippedCases lists the cases that skipped, by their bare case name,
// sorted.
func (r *report) skippedCases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for name, o := range r.outcomes {
		if o == skipped {
			out = append(out, name[strings.LastIndex(name, "/")+1:])
		}
	}
	sort.Strings(out)
	return out
}

// ran lists the cases that ran to a verdict, passed or failed, by bare
// name.
func (r *report) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for name, o := range r.outcomes {
		if o != skipped {
			out = append(out, name[strings.LastIndex(name, "/")+1:])
		}
	}
	sort.Strings(out)
	return out
}

// droppedGroups lists the groups dropped whole, sorted.
func (r *report) droppedGroups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.dropped))
	for g := range r.dropped {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// print writes the run's summary into the test log: the groups dropped
// with their reasons, and the stub cases skipped, so a partial run is
// never mistaken for a full one.
func (r *report) print(t testing.TB, stubCases []string) {
	t.Helper()
	for _, g := range r.droppedGroups() {
		t.Logf("dropped the %s group: %s", g, r.dropped[g])
	}
	var stubs []string
	for _, name := range r.skippedCases() {
		if slices.Contains(stubCases, name) {
			stubs = append(stubs, name)
		}
	}
	if len(stubs) > 0 {
		t.Logf("skipped without %s: %s", EnvStubsURL, strings.Join(stubs, ", "))
	}
	n := 0
	for _, o := range r.outcomes {
		if o != skipped {
			n++
		}
	}
	t.Logf("run %s: %d cases ran, %d skipped", r.run, n, len(r.outcomes)-n)
}
