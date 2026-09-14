// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"
	"testing"

	"latere.ai/x/lux/manifest"
)

// TestCodesHaveOneSentence: every code has one fixed user sentence, the
// unavailability one is manifest's, and the developer's line carries the
// code and the detail apart from it.
func TestCodesHaveOneSentence(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Codes() {
		msg := c.Message()
		if msg == "" || !strings.HasSuffix(msg, ".") || strings.Count(msg, ". ") != 0 || seen[msg] {
			t.Errorf("code %s: sentence %q", c, msg)
		}
		seen[msg] = true
	}
	if CodeAuthorizerUnavailable.Message() != manifest.CodeAuthorizerUnavailable.Message() {
		t.Error("authorizer_unavailable reads differently here and in manifest")
	}
	if Code("nope").Message() != "" {
		t.Error("an unknown code has a sentence")
	}
	e := refuse(CodeForbidden, "authz deny: not yours")
	if e.Message != "You do not have permission to do this." || e.Error() != "forbidden: authz deny: not yours" {
		t.Errorf("refuse() = %+v", e)
	}
	if (&Error{Code: CodeUnauthenticated}).Error() != "unauthenticated" {
		t.Error("an Error without detail renders more than its code")
	}
}
