// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import "testing"

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
// it calls conformance.Run(t, cfg) with the stack's public URL, token,
// and Key. Spec 018 builds that package on another branch; at the merge
// the call replaces the skip below and this file changes nowhere else.
func runConformance(t *testing.T, s *stack, keyValue string) {
	t.Helper()
	_, _, _ = s.gw.public, s.token, keyValue
	t.Skip("test/conformance is spec 018's and lands on another branch; runConformance calls conformance.Run at the merge")
}
