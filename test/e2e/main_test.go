// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration && !postgres

// Package e2e is the integration tier of spec 015: luxd and lux-stubs as
// processes on loopback, built into a directory the tier removes at the
// end, so the real HTTP clients, listeners, and shutdown path are what
// the tests drive. Every test here begins with TestE2E and the file is
// tagged integration, so the gate's untagged run never sees it:
//
//	go test -tags=integration -run '^TestE2E' -v ./...
package e2e

import (
	"fmt"
	"os"
	"testing"
)

// TestMain builds the two binaries once, runs the tier, and removes the
// build directory.
func TestMain(m *testing.M) {
	dir, err := build()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
