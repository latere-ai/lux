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

// TestZeroRetentionRules: Provider.spec.zeroRetention decodes absent,
// empty, and with requestFields; its requestFields are held to every
// rule of spec.requestFields at paths under
// spec.zeroRetention.requestFields; a member other than requestFields is
// unknown_field; and Key.spec.zeroRetention is a boolean, false by
// default and rendered either way.
func TestZeroRetentionRules(t *testing.T) {
	o := corpusOptions(t)
	provider := func(block string) string {
		return head(v1.KindProvider, "p") + minProvider + "  zeroRetention:\n" + block
	}
	fields := func(members string) string { return provider("    requestFields:\n" + members) }
	fill := MaxRequestFieldsBytes - len(`{"k":""}`)
	refused := []struct {
		name, body string
		code       Code
		path       string
	}{
		{"model", fields("      model: gpt-5\n"), CodeReservedPrefix, `spec.zeroRetention.requestFields["model"]`},
		{"stream in another case", fields("      Stream: false\n"), CodeReservedPrefix, `spec.zeroRetention.requestFields["Stream"]`},
		{"stream_options", fields("      stream_options: {include_usage: false}\n"), CodeReservedPrefix, `spec.zeroRetention.requestFields["stream_options"]`},
		{"a null member", fields("      user: null\n"), CodeInvalidField, `spec.zeroRetention.requestFields["user"]`},
		{"a nested member left empty", fields("      provider:\n        zdr:\n"), CodeInvalidField, `spec.zeroRetention.requestFields["provider"]["zdr"]`},
		{"a list", provider("    requestFields: [1, 2]\n"), CodeInvalidField, "spec.zeroRetention.requestFields"},
		{"one byte above the bound", fields("      k: \"" + strings.Repeat("x", fill+1) + "\"\n"), CodeInvalidField, "spec.zeroRetention.requestFields"},
		{"an unknown member", provider("    zdr: true\n"), CodeUnknownField, "spec.zeroRetention.zdr"},
		{"not an object", head(v1.KindProvider, "p") + minProvider + "  zeroRetention: true\n", CodeInvalidField, "spec.zeroRetention"},
		{"a Key's flag as a string", head(v1.KindKey, "k") + minKey + "  zeroRetention: \"true\"\n", CodeInvalidField, "spec.zeroRetention"},
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
	t.Run("the general fields at the bound and the zero-retention fields at the bound", func(t *testing.T) {
		at := "k: \"" + strings.Repeat("x", fill) + "\"\n"
		mustResolve(t, head(v1.KindProvider, "p")+minProvider+"  requestFields:\n    "+at+"  zeroRetention:\n    requestFields:\n      "+at, o)
	})
	declared := func(body string) *v1.ZeroRetention {
		t.Helper()
		return mustResolve(t, body, o).Object.(*v1.Provider).Spec.ZeroRetention
	}
	if z := declared(head(v1.KindProvider, "p") + minProvider); z != nil {
		t.Errorf("absent resolves to %+v", z)
	}
	for _, body := range []string{provider("    {}\n"), provider("    requestFields: {}\n")} {
		if z := declared(body); z == nil || z.RequestFields != nil {
			t.Errorf("an empty declaration resolves to %+v\n%s", z, body)
		}
	}
	if z := declared(fields("      provider: {zdr: true}\n")); z == nil || !reflect.DeepEqual(z.RequestFields, map[string]any{"provider": map[string]any{"zdr": true}}) {
		t.Errorf("a declaration with fields resolves to %+v", z)
	}
	r, err := Resolve(context.Background(), &v1.Provider{Metadata: v1.ObjectMeta{Name: "p"}, Spec: v1.ProviderSpec{
		Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1",
		ZeroRetention: &v1.ZeroRetention{RequestFields: map[string]any{"provider": map[string]bool{"zdr": true}}},
	}}, o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r.Object)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"zeroRetention":{"requestFields":{"provider":{"zdr":true}}}`) {
		t.Errorf("the rendered Provider %s", raw)
	}
	key := mustResolve(t, head(v1.KindKey, "k")+minKey, o).Object.(*v1.Key)
	if key.Spec.ZeroRetention {
		t.Error("a Key that does not name the flag asks for zero retention")
	}
	// Written only when set: a Key that does not ask renders as it did
	// before the member existed.
	if raw, err := json.Marshal(key); err != nil || strings.Contains(string(raw), "zeroRetention") {
		t.Errorf("a Key that does not ask renders %s (%v)", raw, err)
	}
	key.Spec.ZeroRetention = true
	if raw, err := json.Marshal(key); err != nil || !strings.Contains(string(raw), `"zeroRetention":true`) {
		t.Errorf("a Key that asks renders %s (%v)", raw, err)
	}
}
