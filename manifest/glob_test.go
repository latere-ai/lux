// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"slices"
	"testing"
)

func sortStrings(s []string) { slices.Sort(s) }

func TestGlob(t *testing.T) {
	cases := []struct {
		selector, name string
		match          bool
	}{
		{"gpt-5", "gpt-5", true},
		{"gpt-5", "gpt-5-mini", false},
		{"gpt-5", "GPT-5", false},
		{"*", "anything/at/all", true},
		{"*", "", true},
		{"anthropic/*", "anthropic/claude-sonnet-4", true},
		{"anthropic/*", "anthropic/", true},
		{"anthropic/*", "anthropic", false},
		{"anthropic/*", "openai/anthropic/x", false},
		{"*/claude-*", "anthropic/claude-opus-4", true},
		{"*/claude-*", "claude-opus-4", false},
		{"gpt-*", "gpt-4.1", true},
		{"gpt-*", "gpt-", true},
		{"*-preview", "gpt-5-preview", true},
		{"*-preview", "gpt-5-preview-2", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxcyyb", false},
		{"a*b*c", "ac", false},
		{"laptop/llama3.1:*", "laptop/llama3.1:8b", true},
		{"**", "x", true},
	}
	for _, c := range cases {
		if got := Match(c.selector, c.name); got != c.match {
			t.Errorf("Match(%q, %q) = %v, want %v", c.selector, c.name, got, c.match)
		}
	}
	t.Run("the same function accepts a selector and matches a name", func(t *testing.T) {
		for _, ok := range []string{"*", "gpt-5", "anthropic/*", "a/b/c", "laptop/llama3.1:8b", "laptop/llama3.1:*", "x_y"} {
			if err := checkSelector(ok); err != nil {
				t.Errorf("checkSelector(%q) = %v", ok, err)
			}
		}
		for _, bad := range []string{"", "GPT-*", "a?b", "[ab]", "a b", "-a", "a/", "a//b", "a\\*", string(make([]byte, 129))} {
			if err := checkSelector(bad); err == nil {
				t.Errorf("checkSelector(%q) accepted", bad)
			}
		}
		if err := checkGlob("GPT-*"); err == nil {
			t.Error("checkGlob accepted upper case")
		}
	})
}
