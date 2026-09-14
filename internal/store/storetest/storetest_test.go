// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"errors"
	"testing"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestStoreConformance runs the suite against the memory store, which
// is what measures the suite's own coverage and proves it passes on the
// one implementation this phase builds.
func TestStoreConformance(t *testing.T) {
	Run(t, func(*testing.T) store.Store { return memory.New() })
}

// recorder is a testing.TB that records a failure instead of stopping
// the test, so the helpers' failure branches can be driven.
type recorder struct {
	testing.TB
	failed string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = format
}

func TestHelpersReportFailures(t *testing.T) {
	cases := map[string]func(tb testing.TB){
		"noErr":     func(tb testing.TB) { noErr(tb, errors.New("boom"), "op") },
		"wantErr":   func(tb testing.TB) { wantErr(tb, nil, store.ErrNotFound, "op") },
		"equal":     func(tb testing.TB) { equal(tb, 1, 2, "n") },
		"truth":     func(tb testing.TB) { truth(tb, false, "claim") },
		"sameNames": func(tb testing.TB) { sameNames(tb, []v1.Object{provider("a")}, "b") },
		"sameBytes": func(tb testing.TB) { sameBytes(tb, []byte("a"), []byte("b"), "bytes") },
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			run(r)
			if r.failed == "" {
				t.Fatalf("%s did not report the failure", name)
			}
		})
	}
	// isNamed tells the contract's errors from any other.
	if !isNamed(store.ErrNested) || isNamed(errors.New("other")) {
		t.Fatal("isNamed does not tell the contract's errors apart")
	}
	if h := hash(3); len(h) != 64 || h[0] != '3' {
		t.Fatalf("hash(3) = %q", h)
	}
}
