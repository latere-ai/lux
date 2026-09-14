// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"os"
	"testing"
)

// TestContract is the entry point for a server this tree did not start:
//
//	LUX_TEST_URL=https://api.example.com LUX_TEST_TOKEN=$(platform-token) \
//	  go test latere.ai/x/lux/test/conformance -run TestContract -v
//
// It builds Config from LUX_TEST_URL, LUX_TEST_TOKEN, LUX_TEST_SUBJECT,
// LUX_TEST_ISSUER_URL, LUX_TEST_STUBS_URL, and LUX_TEST_INTERNAL_URL and
// calls Run; with LUX_TEST_URL unset it skips with one line saying so.
func TestContract(t *testing.T) {
	cfg, skip := fromEnv(os.Getenv)
	if skip != "" {
		t.Skip(skip)
	}
	Run(t, cfg)
}
