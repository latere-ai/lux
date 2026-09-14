// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestSelectorsMatchAtRequestTime is spec 007's selector row: a request
// naming a Model whose name matches no selector is model_not_allowed
// after model_not_found; a Model declared or discovered after the Key
// was resolved is admitted when a selector matches its name, without a
// re-resolve; a Model that was deleted stops matching; a glob matches
// across /; and the match is manifest.Match.
func TestSelectorsMatchAtRequestTime(t *testing.T) {
	w := newWorld(t)
	const selectiveValue = "lux_selective0123456789abcdefghijklmnopqrst"
	w.key("selective", selectiveValue, "team/*", "gpt")
	ask := func(model string) *httptest.ResponseRecorder {
		r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody(model, false))
		r.Header.Set("Authorization", "Bearer "+selectiveValue)
		return w.do(r)
	}
	if code := errorCode(t, ask("team/new/deep")); code != CodeModelNotFound {
		t.Errorf("a name the catalog lacks: %s, want model_not_found before model_not_allowed", code)
	}
	rec := ask("claude")
	if code := errorCode(t, rec); code != CodeModelNotAllowed {
		t.Errorf("a Model outside the selectors: %s", code)
	}
	if detail := rec.Header().Get(HeaderErrorDetail); !strings.Contains(detail, "[team/* gpt]") || !strings.Contains(detail, `"claude"`) {
		t.Errorf("detail = %q", detail)
	}
	// Declared after the Key was resolved: the glob reaches it across /.
	w.model("team/new/deep", target("oai", "gpt-4.1"))
	if rec := ask("team/new/deep"); rec.Header().Get(HeaderError) != "" || rec.Code != http.StatusOK {
		t.Errorf("a Model declared after the Key: %d %s", rec.Code, rec.Header().Get(HeaderError))
	}
	if rec := ask("gpt"); rec.Header().Get(HeaderError) != "" {
		t.Errorf("the exact selector: %s", rec.Header().Get(HeaderError))
	}
	if code := errorCode(t, ask("team")); code != CodeModelNotFound {
		t.Errorf("the glob's prefix alone: %s", code)
	}
	// Deleted: its name is no longer in the catalog.
	w.catalog.mu.Lock()
	delete(w.catalog.models, "team/new/deep")
	w.catalog.mu.Unlock()
	if code := errorCode(t, ask("team/new/deep")); code != CodeModelNotFound {
		t.Errorf("a deleted Model: %s", code)
	}
	if w.openai.hits.Load() != 2 {
		t.Errorf("the provider saw %d requests, want the two admitted", w.openai.hits.Load())
	}
	// The match is manifest.Match: the same answers, selector by selector.
	for _, c := range []struct {
		selector, name string
		want           bool
	}{
		{"*", "anything/at/all", true},
		{"anthropic/*", "anthropic/claude-3/latest", true},
		{"anthropic/*", "anthropic", false},
		{"gpt-5", "gpt-5", true},
		{"gpt-5", "gpt-5-mini", false},
		{"*-mini", "gpt-5-mini", true},
		{"a*c", "abc", true},
		{"a*c", "ab", false},
		{"openai/*/latest", "openai/gpt-5/latest", true},
	} {
		k := &v1.Key{Spec: v1.KeySpec{Models: []string{"nothing", c.selector}}}
		if got := allowed(k, c.name); got != c.want || manifest.Match(c.selector, c.name) != c.want {
			t.Errorf("selector %q against %q: allowed %v, Match %v, want %v", c.selector, c.name, got, manifest.Match(c.selector, c.name), c.want)
		}
	}
	if allowed(&v1.Key{}, "gpt") {
		t.Error("a Key without selectors matches a Model")
	}
}
