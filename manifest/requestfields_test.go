// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestRequestFieldsRules: spec.requestFields is a JSON object of
// non-null members, at most MaxRequestFieldsBytes encoded, that names
// none of the members the gateway writes; the refusals carry the codes
// and paths of the Provider.spec table.
func TestRequestFieldsRules(t *testing.T) {
	o := corpusOptions(t)
	provider := func(fields string) string {
		return head(v1.KindProvider, "p") + minProvider + "  requestFields:\n" + fields
	}
	// The longest string value whose object encodes to the bound exactly.
	fill := MaxRequestFieldsBytes - len(`{"k":""}`)
	refused := []struct {
		name, body string
		code       Code
		path       string
	}{
		{"model", provider("    model: gpt-5\n"), CodeReservedPrefix, `spec.requestFields["model"]`},
		{"stream in another case", provider("    Stream: false\n"), CodeReservedPrefix, `spec.requestFields["Stream"]`},
		{"stream_options", provider("    STREAM_OPTIONS: {include_usage: false}\n"), CodeReservedPrefix, `spec.requestFields["STREAM_OPTIONS"]`},
		{"a null member", provider("    user: null\n"), CodeInvalidField, `spec.requestFields["user"]`},
		{"a nested member left empty", provider("    provider:\n      zdr:\n      data_collection: deny\n"), CodeInvalidField, `spec.requestFields["provider"]["zdr"]`},
		{"a list", head(v1.KindProvider, "p") + minProvider + "  requestFields: [1, 2]\n", CodeInvalidField, "spec.requestFields"},
		{"a string", head(v1.KindProvider, "p") + minProvider + "  requestFields: zdr\n", CodeInvalidField, "spec.requestFields"},
		{"one byte above the bound", provider("    k: \"" + strings.Repeat("x", fill+1) + "\"\n"), CodeInvalidField, "spec.requestFields"},
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
	t.Run("the values the rules admit", func(t *testing.T) {
		for _, body := range []string{
			provider("    provider: {zdr: true, data_collection: deny, order: [a, b], max_price: {prompt: 1.5}}\n"),
			provider("    provider: {model: x, stream: true}\n    temperature: 0\n"),
			provider("    stop: [null, \"END\"]\n    metadata: {tags: [{a: null}]}\n"),
			provider("    k: \"" + strings.Repeat("x", fill) + "\"\n"),
			provider("    empty: {}\n"),
		} {
			mustResolve(t, body, o)
		}
	})
	t.Run("the reserved set is a copy", func(t *testing.T) {
		got := ReservedRequestFields()
		if !reflect.DeepEqual(got, []string{"model", "stream", "stream_options"}) {
			t.Fatalf("ReservedRequestFields() = %v", got)
		}
		got[0] = "changed"
		if ReservedRequestFields()[0] != "model" {
			t.Error("a caller's edit reached the reserved set")
		}
	})
	t.Run("an empty object is absent", func(t *testing.T) {
		r := mustResolve(t, provider("    {}\n"), o)
		if fields := r.Object.(*v1.Provider).Spec.RequestFields; fields != nil {
			t.Errorf("requestFields %v", fields)
		}
	})
}

// TestRequestFieldsInGoForm: a Provider built in Go is held to the same
// rules through the JSON form of its requestFields, and resolves to the
// JSON form, so a nested map of any Go type is an object the gateway
// merges.
func TestRequestFieldsInGoForm(t *testing.T) {
	o := corpusOptions(t)
	p := func(fields map[string]any) *v1.Provider {
		return &v1.Provider{Metadata: v1.ObjectMeta{Name: "p"}, Spec: v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", RequestFields: fields}}
	}
	refused := []struct {
		name   string
		fields map[string]any
		code   Code
		path   string
	}{
		{"a value with no JSON form", map[string]any{"f": func() {}}, CodeInvalidField, "spec.requestFields"},
		{"a nil member inside a typed map", map[string]any{"provider": map[string]*bool{"zdr": nil}}, CodeInvalidField, `spec.requestFields["provider"]["zdr"]`},
		{"a reserved member", map[string]any{"model": "x"}, CodeReservedPrefix, `spec.requestFields["model"]`},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			_, err := Resolve(context.Background(), p(c.fields), o)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("Resolve returned %v, not *Error", err)
			}
			wantErr(t, e, c.code, c.path)
		})
	}
	r, err := Resolve(context.Background(), p(map[string]any{"provider": map[string]bool{"zdr": true}, "n": 3}), o)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"provider": map[string]any{"zdr": true}, "n": float64(3)}
	if got := r.Object.(*v1.Provider).Spec.RequestFields; !reflect.DeepEqual(got, want) {
		t.Errorf("resolved requestFields %#v, want %#v", got, want)
	}
	raw, err := json.Marshal(r.Object)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"requestFields":{"n":3,"provider":{"zdr":true}}`) {
		t.Errorf("the rendered Provider %s", raw)
	}
}
