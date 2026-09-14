// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeT is the testing.TB the package's own tests hand a case they expect
// to fail or skip: it records what the case reported instead of failing
// the test that drives it. A Fatal or a Skip ends the case's goroutine as
// the testing package's does.
type fakeT struct {
	testing.TB
	name string
	ctx  context.Context

	mu       sync.Mutex
	failed   bool
	skipped  bool
	logs     []string
	cleanups []func()
}

func (f *fakeT) Helper()                  {}
func (f *fakeT) Name() string             { return f.name }
func (f *fakeT) Context() context.Context { return f.ctx }

func (f *fakeT) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeT) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, strings.TrimSuffix(s, "\n"))
}

func (f *fakeT) fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = true
}

func (f *fakeT) Log(args ...any)                 { f.record(fmt.Sprintln(args...)) }
func (f *fakeT) Logf(format string, args ...any) { f.record(fmt.Sprintf(format, args...)) }
func (f *fakeT) Error(args ...any)               { f.fail(); f.Log(args...) }
func (f *fakeT) Errorf(format string, args ...any) {
	f.fail()
	f.Logf(format, args...)
}
func (f *fakeT) Fail()    { f.fail() }
func (f *fakeT) FailNow() { f.fail(); runtime.Goexit() }
func (f *fakeT) Fatal(args ...any) {
	f.Error(args...)
	runtime.Goexit()
}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	runtime.Goexit()
}
func (f *fakeT) Skip(args ...any) {
	f.mu.Lock()
	f.skipped = true
	f.mu.Unlock()
	f.Log(args...)
	runtime.Goexit()
}
func (f *fakeT) Skipf(format string, args ...any) {
	f.mu.Lock()
	f.skipped = true
	f.mu.Unlock()
	f.Logf(format, args...)
	runtime.Goexit()
}

func (f *fakeT) Failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

func (f *fakeT) Skipped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.skipped
}

// output is everything the case logged, one line each.
func (f *fakeT) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.logs, "\n")
}

// drive runs fn on a fresh fakeT named name in a goroutine of its own, so
// a Fatal or a Skip ends it, runs its cleanups, and returns the record.
func drive(t testing.TB, name string, fn func(t testing.TB)) *fakeT {
	t.Helper()
	f := &fakeT{name: name, ctx: t.Context()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			f.mu.Lock()
			cleanups := f.cleanups
			f.mu.Unlock()
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		}()
		fn(f)
	}()
	<-done
	return f
}

// TestFakeTRecordsWhatACaseReports: the fake fails on Error and Fatal,
// skips on Skip, ends the case on Fatal and Skip, and runs its cleanups.
func TestFakeTRecordsWhatACaseReports(t *testing.T) {
	cleaned := false
	f := drive(t, "a/b", func(t testing.TB) {
		t.Cleanup(func() { cleaned = true })
		t.Helper()
		t.Logf("%s", "note")
		t.Errorf("one %d", 1)
		t.Fatal("two")
		t.Error("never")
	})
	if !f.Failed() || f.Skipped() || !cleaned || f.Name() != "a/b" || f.output() != "note\none 1\ntwo" {
		t.Errorf("failed %v skipped %v cleaned %v output %q", f.Failed(), f.Skipped(), cleaned, f.output())
	}
	s := drive(t, "c", func(t testing.TB) { t.Skipf("why %s", "not"); t.Error("never") })
	if s.Failed() || !s.Skipped() || s.output() != "why not" {
		t.Errorf("skip: failed %v skipped %v output %q", s.Failed(), s.Skipped(), s.output())
	}
	s = drive(t, "d", func(t testing.TB) { t.Skip("plain"); t.Error("never") })
	if !s.Skipped() {
		t.Error("Skip did not skip")
	}
	n := drive(t, "e", func(t testing.TB) { t.Fail(); t.FailNow(); t.Error("never") })
	if !n.Failed() || n.Context() == nil {
		t.Error("Fail and FailNow did not fail")
	}
	if p := drive(t, "f", func(t testing.TB) { t.Log("fine") }); p.Failed() || p.Skipped() {
		t.Error("a passing case was marked")
	}
}
