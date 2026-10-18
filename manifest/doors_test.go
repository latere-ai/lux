// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestKeyDoorsRules is spec 049's Key member: spec.doors decodes from
// YAML and JSON, is absent by default and rendered only when set, keeps
// the order written, resolves to a fixed point, and is mutable; an item
// that is not a door, an item named twice, an empty list, and a list
// that is not a list of strings are refused at their paths; and
// OpensDoor reads every door from an absent list and the listed doors
// from a present one.
func TestKeyDoorsRules(t *testing.T) {
	o := corpusOptions(t)
	key := func(doors string) string { return head(v1.KindKey, "k") + minKey + "  doors: " + doors + "\n" }
	refused := []struct {
		name, body string
		code       Code
		path       string
	}{
		{"an unknown door", key("[openai, azure]"), CodeInvalidField, "spec.doors[1]"},
		{"a door spelled as its path", key("[/openai]"), CodeInvalidField, "spec.doors[0]"},
		{"a door in another case", key("[OpenAI]"), CodeInvalidField, "spec.doors[0]"},
		{"a door named twice", key("[lux, openai, lux]"), CodeInvalidField, "spec.doors[2]"},
		{"an empty list", key("[]"), CodeInvalidField, "spec.doors"},
		{"a string", key("lux"), CodeInvalidField, "spec.doors"},
		{"a list of numbers", key("[1]"), CodeInvalidField, "spec.doors[0]"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			e := resolveErr(t, c.body, o)
			wantErr(t, e, c.code, c.path)
			if e.Detail == "" {
				t.Error("no developer detail")
			}
		})
	}
	t.Run("an empty list in JSON", func(t *testing.T) {
		obj, err := Decode([]byte(`{"apiVersion":"lux.latere.ai/v1beta1","kind":"Key","metadata":{"name":"k"},"spec":{"models":["gpt-5"],"doors":[]}}`), MediaJSON, Hint{})
		if err == nil {
			_, err = Resolve(context.Background(), obj, o)
		}
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("Resolve = %v, want an *Error", err)
		}
		wantErr(t, e, CodeInvalidField, "spec.doors")
	})

	// Absent: every door, and no member on the wire.
	plain := mustResolve(t, head(v1.KindKey, "k")+minKey, o).Object.(*v1.Key)
	if plain.Spec.Doors != nil {
		t.Errorf("absent resolves to %v", plain.Spec.Doors)
	}
	if raw, err := json.Marshal(plain); err != nil || strings.Contains(string(raw), "doors") {
		t.Errorf("a Key without doors renders %s (%v)", raw, err)
	}
	for _, d := range []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux} {
		if !plain.Spec.OpensDoor(d) {
			t.Errorf("a Key without doors does not open %s", d)
		}
	}

	// Present: the doors as written, rendered, and a fixed point.
	r := mustResolve(t, key("[lux, anthropic]"), o)
	k := r.Object.(*v1.Key)
	if !slices.Equal(k.Spec.Doors, []v1.Dialect{v1.DialectLux, v1.DialectAnthropic}) {
		t.Fatalf("doors resolve to %v", k.Spec.Doors)
	}
	raw, err := json.Marshal(k)
	if err != nil || !strings.Contains(string(raw), `"doors":["lux","anthropic"]`) {
		t.Fatalf("a Key with doors renders %s (%v)", raw, err)
	}
	again, err := Decode(raw, MediaJSON, Hint{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Resolve(context.Background(), again, o)
	if err != nil {
		t.Fatal(err)
	}
	if raw2, err := json.Marshal(r2.Object); err != nil || string(raw2) != string(raw) {
		t.Errorf("resolve is not a fixed point:\n%s\n%s", raw, raw2)
	}
	for d, want := range map[v1.Dialect]bool{v1.DialectLux: true, v1.DialectAnthropic: true, v1.DialectOpenAI: false, v1.DialectGemini: false} {
		if got := k.Spec.OpensDoor(d); got != want {
			t.Errorf("OpensDoor(%s) = %v, want %v", d, got, want)
		}
	}

	// Mutable: an update over the stored Key changes, adds, and drops
	// the list.
	stored := mustResolve(t, key("[lux]"), o).Object.(*v1.Key)
	stored.Status.ID = "key_01J9ZZZZZZZZZZZZZZZZZZZZZZ"
	update := o
	update.Existing = stored
	for _, body := range []string{key("[openai, gemini]"), head(v1.KindKey, "k") + minKey} {
		if _, err := Resolve(context.Background(), mustDecode(t, body), update); err != nil {
			t.Errorf("an update to doors: %v\n%s", err, body)
		}
	}

	// A disabled Key may name doors too.
	mustResolve(t, head(v1.KindKey, "k")+"spec:\n  disabled: true\n  doors: [gemini]\n", o)
}
