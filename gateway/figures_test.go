// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

func money(t *testing.T, s string) *v1.Money {
	t.Helper()
	m, err := v1.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return &m
}

// figured is a declared Model with every figure, priced per million
// tokens as the manifest holds it.
func figured(t *testing.T, w *world, name string) *v1.Model {
	t.Helper()
	m := w.model(name, target("oai", "gpt-4.1"))
	m.Spec.ContextWindow, m.Spec.MaxOutputTokens = 400000, 128000
	m.Spec.Modalities.Input = []v1.Modality{v1.ModalityText, v1.ModalityImage}
	m.Spec.Pricing = &v1.Pricing{Currency: "USD", Per: 1_000_000,
		Input: money(t, "1.25"), Output: money(t, "10"), CachedInput: money(t, "0.125"), CacheWrite: money(t, "1.25")}
	m.Status.Source = v1.SourceDeclared
	return m
}

// TestModelFiguresPerDoor: each door's entry carries the Model's
// figures after the members its dialect's clients read, in the members
// that door names them by, and the list carries the same entry as the
// read.
func TestModelFiguresPerDoor(t *testing.T) {
	w := newWorld(t)
	figured(t, w, "figured")
	const snakePricing = `"pricing":{"currency":"USD","per":1000000,"input":"1.25","output":"10","cached_input":"0.125","cache_write":"1.25"}`
	want := map[v1.Dialect]string{
		v1.DialectOpenAI: `{"id":"figured","object":"model","created":0,"owned_by":"lux",` +
			`"context_window":400000,"max_output_tokens":128000,"input_modalities":["text","image"],` + snakePricing + `}`,
		v1.DialectAnthropic: `{"type":"model","id":"figured","display_name":"figured","created_at":"1970-01-01T00:00:00Z",` +
			`"max_input_tokens":400000,"max_tokens":128000,"input_modalities":["text","image"],` + snakePricing + `}`,
		v1.DialectGemini: `{"name":"models/figured","displayName":"figured","supportedGenerationMethods":["generateContent","countTokens"],` +
			`"inputTokenLimit":400000,"outputTokenLimit":128000}`,
		v1.DialectLux: `{"id":"figured","object":"model","created":0,"owned_by":"lux",` +
			`"context_window":400000,"max_output_tokens":128000,"input_modalities":["text","image"],` + snakePricing + `}`,
	}
	for _, d := range doors {
		base := "/" + string(d) + versionPrefix(d) + "/models"
		rec := w.do(w.request("GET", base+"/figured", ""))
		if rec.Code != 200 || rec.Body.String() != want[d]+"\n" {
			t.Errorf("%s read: %d\n got %s\nwant %s", d, rec.Code, rec.Body.String(), want[d])
		}
		list := w.do(w.request("GET", base, "")).Body.String()
		if !strings.Contains(list, want[d]) {
			t.Errorf("%s list does not carry the read's entry:\n%s", d, list)
		}
		var decoded any
		if err := json.Unmarshal([]byte(list), &decoded); err != nil {
			t.Errorf("%s list is not JSON: %v", d, err)
		}
	}
}

// TestModelFiguresOmitted: a figure the Model does not have is left out,
// never written as zero; a discovered Model's modalities, which are the
// kind's default and not a datum about it, are left out; and a price
// quoted per a count that does not divide a million by a power of ten
// leaves the prices out rather than state them wrong.
func TestModelFiguresOmitted(t *testing.T) {
	w := newWorld(t)
	discovered := w.model("found", target("oai", "gpt-4.1"))
	discovered.Status.Source = v1.SourceDiscovered
	discovered.Spec.Modalities.Input = []v1.Modality{v1.ModalityText}
	discovered.Spec.ContextWindow = 8192
	odd := figured(t, w, "odd")
	odd.Spec.Pricing.Per = 3
	window := w.model("window", target("oai", "gpt-4.1"))
	window.Spec.ContextWindow = 1000
	partial := w.model("partial", target("oai", "gpt-4.1"))
	partial.Spec.Pricing = &v1.Pricing{Currency: "EUR", Per: 1000, Input: money(t, "0.001"), Output: money(t, "0.002")}
	for name, want := range map[string]string{
		"found":   `{"id":"found","object":"model","created":0,"owned_by":"lux","context_window":8192}`,
		"odd":     `{"id":"odd","object":"model","created":0,"owned_by":"lux","context_window":400000,"max_output_tokens":128000,"input_modalities":["text","image"]}`,
		"window":  `{"id":"window","object":"model","created":0,"owned_by":"lux","context_window":1000}`,
		"partial": `{"id":"partial","object":"model","created":0,"owned_by":"lux","pricing":{"currency":"EUR","per":1000000,"input":"1","output":"2"}}`,
		"gpt":     `{"id":"gpt","object":"model","created":0,"owned_by":"lux"}`,
	} {
		if got := w.do(w.request("GET", "/openai/v1/models/"+name, "")).Body.String(); got != want+"\n" {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
	if got := w.do(w.request("GET", "/lux/v1/models/found", "")).Body.String(); strings.Contains(got, "modalities") {
		t.Errorf("a discovered Model lists its default modalities on the lux door: %s", got)
	}
	if got := modelEntry(odd); got.Pricing != nil {
		t.Errorf("a per the kind refuses gave prices %+v", got.Pricing)
	}
}

// TestPerMillion: a price quoted per 1, 1,000 or 1,000,000 tokens is the
// same digits per million with the point moved, exact at the largest
// price the kind admits; a negative price and a per that does not divide
// a million by a power of ten have no price per million.
func TestPerMillion(t *testing.T) {
	for _, tc := range []struct {
		price string
		per   int
		want  string
	}{
		{"1.25", 1_000_000, "1.25"},
		{"0", 1_000_000, "0"},
		{"30", 1_000_000, "30"},
		{"0.000001", 1_000_000, "0.000001"},
		{"0.00125", 1000, "1.25"},
		{"0.001", 1000, "1"},
		{"2", 1000, "2000"},
		{"0.000001", 1000, "0.001"},
		{"0.000001", 1, "1"},
		{"1.5", 1, "1500000"},
		{"999999999999.999999", 1, "999999999999999999"},
	} {
		got, ok := perMillion(*money(t, tc.price), tc.per)
		if !ok || got != tc.want {
			t.Errorf("perMillion(%s, %d) = %q %v, want %q", tc.price, tc.per, got, ok, tc.want)
		}
	}
	for _, tc := range []struct {
		price v1.Money
		per   int
	}{{-1, 1_000_000}, {1, 3}, {1, 0}, {1, 2_000_000}, {1, 500}} {
		if got, ok := perMillion(tc.price, tc.per); ok {
			t.Errorf("perMillion(%d, %d) = %q, want none", tc.price, tc.per, got)
		}
	}
}
