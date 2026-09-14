// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"strings"
	"testing"

	"latere.ai/x/lux/test/conformance"
)

// TestE2EConformance runs the conformance suite of spec 018 in process
// against a server this tier started, through runConformance, rather
// than shelling out to TestContract, which is the entry point for a
// server the tree did not start.
func TestE2EConformance(t *testing.T) {
	s := newStack(t, nil)
	key := s.fixtures(t)
	runConformance(t, s, key)
}

// runConformance is the one seam between this tier and test/conformance:
// the suite runs against the stack's public URL with a token minted from
// the stack's own issuer for whichever subject a case asks for, so every
// case that needs a second subject runs rather than skips. The suite
// mints and deletes its own Key, so the tier's fixture Key is not handed
// over, and the stub table's cases run against the same lux-stubs the
// stack is built on, whose index names every stub at its root.
func runConformance(t *testing.T, s *stack, _ string) {
	t.Helper()
	issuer := strings.TrimRight(s.stubs.urls["issuer"], "/")
	conformance.Run(t, conformance.Config{
		URL:      s.gw.public,
		StubsURL: s.stubs.urls["index"],
		Subject:  issuer + "|dev",
		Token: func(subject string) (string, bool) {
			i := strings.LastIndexByte(subject, '|')
			if i < 0 || strings.TrimRight(subject[:i], "/") != issuer {
				return "", false
			}
			return mint(t, s.stubs, subject[i+1:]), true
		},
	})
}
