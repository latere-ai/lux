// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package version

import "testing"

func TestStringCarriesEveryField(t *testing.T) {
	t.Cleanup(func() { Version, Commit, Date = "dev", "none", "unknown" })
	Version, Commit, Date = "v1.2.3", "abc1234", "2026-09-13"
	if got, want := String(), "luxd v1.2.3 (abc1234, 2026-09-13)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestDefaultsMarkADevelopmentBuild(t *testing.T) {
	if got := String(); got != "luxd dev (none, unknown)" {
		t.Fatalf("String() = %q", got)
	}
}
