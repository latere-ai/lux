// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	v1 "latere.ai/x/lux/manifest/v1"
)

// examples are the four manifests of spec 003's design text, as the
// corpus holds them.
var examples = []string{
	"accepted/provider/openai.yaml",
	"accepted/model/gpt-5.yaml",
	"accepted/key/run-42.yaml",
	"accepted/budget/team-research.yaml",
}

func corpusFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(Corpus, name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// decodeErr decodes and returns the *Error, failing on any other outcome.
func decodeErr(t *testing.T, body, contentType string, hint Hint) *Error {
	t.Helper()
	_, err := Decode([]byte(body), contentType, hint)
	if err == nil {
		t.Fatalf("Decode accepted:\n%s", body)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("Decode returned %T %v, not *Error", err, err)
	}
	if e.Message != e.Code.Message() || e.Message == "" {
		t.Errorf("%s carries message %q, want the fixed sentence %q", e.Code, e.Message, e.Code.Message())
	}
	return e
}

func wantErr(t *testing.T, e *Error, code Code, paths ...string) {
	t.Helper()
	if e.Code != code {
		t.Errorf("code = %s (%s), want %s", e.Code, e.Detail, code)
	}
	if len(paths) > 0 && !reflect.DeepEqual(e.Paths, paths) {
		t.Errorf("paths = %q, want %q", e.Paths, paths)
	}
}

func TestDecodeYAMLAndJSONAgree(t *testing.T) {
	for _, name := range examples {
		t.Run(name, func(t *testing.T) {
			y := corpusFile(t, name)
			fromYAML, err := Decode(y, MediaYAML, Hint{})
			if err != nil {
				t.Fatal(err)
			}
			j, err := yaml.YAMLToJSON(y)
			if err != nil {
				t.Fatal(err)
			}
			fromJSON, err := Decode(j, MediaJSON, Hint{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fromYAML, fromJSON) {
				t.Errorf("the YAML and JSON forms decode differently:\n%#v\n%#v", fromYAML, fromJSON)
			}
			// The same body under the JSON-in-YAML rule.
			braced, err := Decode(j, MediaText, Hint{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fromYAML, braced) {
				t.Error("a body beginning with { under a YAML type decodes differently")
			}
		})
	}
}

func TestUnknownFieldNamesThePath(t *testing.T) {
	cases := []struct {
		kind, body, path string
	}{
		{"Provider", "spec:\n  baseUrl: x\n", "spec.baseUrl"},
		{"Provider", "metadata:\n  namespace: x\n", "metadata.namespace"},
		{"Provider", "spec:\n  credential:\n    token: x\n", "spec.credential.token"},
		{"Provider", "spec:\n  credential:\n    valueFrom:\n      file: x\n", "spec.credential.valueFrom.file"},
		{"Provider", "spec:\n  discovery:\n    interval: 1h\n", "spec.discovery.interval"},
		{"Provider", "spec:\n  health:\n    interval: 1h\n", "spec.health.interval"},
		{"Provider", "extra: 1\n", "extra"},
		{"Model", "spec:\n  target: []\n", "spec.target"},
		{"Model", "spec:\n  targets:\n    - provider: a\n      weigth: 1\n", "spec.targets[0].weigth"},
		{"Model", "spec:\n  targets:\n    - provider: a\n    - provider: b\n      region: eu\n", "spec.targets[1].region"},
		{"Model", "spec:\n  pricing:\n    inputs: \"1\"\n", "spec.pricing.inputs"},
		{"Model", "spec:\n  modalities:\n    both: [text]\n", "spec.modalities.both"},
		{"Model", "spec:\n  context_window: 1\n", "spec.context_window"},
		{"Key", "spec:\n  model: x\n", "spec.model"},
		{"Key", "spec:\n  limits:\n    requestsPerSecond: 1\n", "spec.limits.requestsPerSecond"},
		{"Key", "spec:\n  limits:\n    spend:\n      ammount: \"1\"\n", "spec.limits.spend.ammount"},
		{"Key", "spec:\n  valueFrom:\n    secret: x\n", "spec.valueFrom.secret"},
		{"Key", "spec:\n  expires: never\n", "spec.expires"},
		{"Budget", "spec:\n  ammount: \"1\"\n", "spec.ammount"},
		{"Budget", "spec:\n  soft: true\n", "spec.soft"},
	}
	if len(cases) < 20 {
		t.Fatalf("%d cases, the criterion asks for twenty", len(cases))
	}
	for _, c := range cases {
		body := "apiVersion: lux.latere.ai/v1beta1\nkind: " + c.kind + "\n" + c.body
		t.Run(c.path+" yaml", func(t *testing.T) {
			wantErr(t, decodeErr(t, body, MediaYAML, Hint{}), CodeUnknownField, c.path)
		})
		t.Run(c.path+" json", func(t *testing.T) {
			j, err := yaml.YAMLToJSON([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			wantErr(t, decodeErr(t, string(j), MediaJSON, Hint{}), CodeUnknownField, c.path)
		})
	}
}

func TestDecodeRefusals(t *testing.T) {
	const envelope = "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata: {name: b}\n"
	cases := []struct {
		name, body, media string
		code              Code
		detail            string
	}{
		{"second document", envelope + "---\n" + envelope, MediaYAML, CodeMultiDocument, "2 YAML documents"},
		{"trailing document marker", envelope + "---\n", MediaYAML, CodeMultiDocument, ""},
		{"wrong version", "apiVersion: lux.latere.ai/v1\nkind: Budget\n", MediaYAML, CodeUnsupportedVersion, `"lux.latere.ai/v1"`},
		{"wrong kind", "apiVersion: lux.latere.ai/v1beta1\nkind: Pod\n", MediaYAML, CodeUnsupportedKind, `"Pod"`},
		{"version before kind", "apiVersion: v2\nkind: Pod\n", MediaYAML, CodeUnsupportedVersion, ""},
		{"version not a string", "apiVersion: 1\nkind: Budget\n", MediaYAML, CodeUnsupportedVersion, "not a string"},
		{"kind not a string", "apiVersion: lux.latere.ai/v1beta1\nkind: [Budget]\n", MediaYAML, CodeUnsupportedKind, "not a string"},
		{"unsupported media type", envelope, "text/plain", CodeUnsupportedMediaType, `"text/plain"`},
		{"no media type", envelope, "", CodeUnsupportedMediaType, ""},
		{"unparsable media type", envelope, "application/", CodeUnsupportedMediaType, ""},
		{"bad yaml", "spec: [1, 2\n", MediaYAML, CodeMalformedBody, "line 1, column"},
		{"bad json", "{\n  \"spec\": \n", MediaJSON, CodeMalformedBody, "line 3, column 1"},
		{"bad json syntax", "{\"spec\": }", MediaJSON, CodeMalformedBody, "line 1, column 10"},
		{"json under yaml type", "{\"spec\": }", MediaXYAML, CodeMalformedBody, "json:"},
		{"two json values", "{} {}", MediaJSON, CodeMalformedBody, "more than one value"},
		{"json array", "[]", MediaJSON, CodeMalformedBody, "not an object"},
		{"yaml scalar", "just a string\n", MediaYAML, CodeMalformedBody, "not a mapping"},
		{"yaml list", "- a\n- b\n", MediaYAML, CodeMalformedBody, "not a mapping"},
		{"empty yaml", "", MediaYAML, CodeMalformedBody, "empty"},
		{"empty json", "", MediaJSON, CodeMalformedBody, "ends before"},
		{"duplicate yaml key", envelope + "spec:\n  amount: \"1\"\n  amount: \"2\"\n", MediaYAML, CodeMalformedBody, `mapping key "amount" already defined`},
		{"duplicate json key", `{"spec": {"amount": "1", "amount": "2"}}`, MediaJSON, CodeMalformedBody, `duplicate key "amount" in spec`},
		{"duplicate root json key", `{"spec": {}, "spec": {}}`, MediaJSON, CodeMalformedBody, "in the document"},
		{"alias without anchor", envelope + "spec:\n  amount: *nothing\n", MediaYAML, CodeMalformedBody, "names no anchor"},
		{"null key", envelope + "spec:\n  ~: 1\n", MediaYAML, CodeMalformedBody, "key must be a string"},
		{"merge of a scalar", envelope + "spec:\n  <<: 5\n", MediaYAML, CodeMalformedBody, "merge key"},
		{"json object key not string", `{"a": {1: 2}}`, MediaJSON, CodeMalformedBody, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := decodeErr(t, c.body, c.media, Hint{})
			wantErr(t, e, c.code)
			if !strings.Contains(e.Detail, c.detail) {
				t.Errorf("detail %q does not carry %q", e.Detail, c.detail)
			}
		})
	}
}

func TestMalformedBodyCarriesLineAndColumn(t *testing.T) {
	e := decodeErr(t, "apiVersion: x\nkind: Budget\nspec:\n  amount: [1,\n", MediaYAML, Hint{})
	wantErr(t, e, CodeMalformedBody)
	if !strings.Contains(e.Detail, "line 4") {
		t.Errorf("detail %q names no line 4", e.Detail)
	}
	e = decodeErr(t, "{\"apiVersion\": \"x\",\n \"kind\": Budget}", MediaJSON, Hint{})
	wantErr(t, e, CodeMalformedBody)
	if !strings.Contains(e.Detail, "line 2, column 10") {
		t.Errorf("detail %q names no line 2, column 10", e.Detail)
	}
}

func TestHintFillsTheEnvelope(t *testing.T) {
	hint := Hint{APIVersion: v1.APIVersion, Kind: v1.KindKey, Name: "run-42"}
	bare, err := Decode([]byte(`{"spec": {"models": ["gpt-5"]}}`), MediaJSON, hint)
	if err != nil {
		t.Fatal(err)
	}
	full, err := Decode([]byte(`{"apiVersion": "lux.latere.ai/v1beta1", "kind": "Key", "metadata": {"name": "run-42"}, "spec": {"models": ["gpt-5"]}}`), MediaJSON, Hint{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(bare)
	b, _ := json.Marshal(full)
	if string(a) != string(b) {
		t.Errorf("hinted body encodes\n%s\nthe full envelope\n%s", a, b)
	}
	if bare.Kind() != v1.KindKey || bare.Name() != "run-42" {
		t.Errorf("hinted object is %s %q", bare.Kind(), bare.Name())
	}
	// The hint fills metadata.name when metadata is present without one.
	withMeta, err := Decode([]byte(`{"metadata": {"labels": {"a": "b"}}, "spec": {"models": ["gpt-5"]}}`), MediaJSON, hint)
	if err != nil {
		t.Fatal(err)
	}
	if withMeta.Name() != "run-42" {
		t.Errorf("name = %q", withMeta.Name())
	}
	// An agreeing envelope is not a disagreement.
	if _, err := Decode([]byte(`{"kind": "Key", "metadata": {"name": "run-42"}, "spec": {}}`), MediaJSON, hint); err != nil {
		t.Errorf("an agreeing envelope was refused: %v", err)
	}
}

func TestHintDisagreementIsRefused(t *testing.T) {
	hint := Hint{APIVersion: v1.APIVersion, Kind: v1.KindKey, Name: "run-42"}
	cases := []struct {
		name, body string
		hint       Hint
		code       Code
		path       string
	}{
		{"version", `{"apiVersion": "lux.latere.ai/v2", "spec": {}}`, hint, CodeUnsupportedVersion, "apiVersion"},
		{"kind", `{"kind": "Model", "spec": {}}`, hint, CodeUnsupportedKind, "kind"},
		{"name", `{"metadata": {"name": "other"}, "spec": {}}`, hint, CodeInvalidField, "metadata.name"},
		{"no version without a hint", `{"kind": "Key", "spec": {}}`, Hint{}, CodeMissingField, "apiVersion"},
		{"no kind without a hint", `{"apiVersion": "lux.latere.ai/v1beta1", "spec": {}}`, Hint{}, CodeMissingField, "kind"},
		{"a hint outside the schema", `{"spec": {}}`, Hint{APIVersion: "lux.latere.ai/v0", Kind: "Key"}, CodeUnsupportedVersion, "apiVersion"},
		{"a hinted kind outside the four", `{"spec": {}}`, Hint{APIVersion: v1.APIVersion, Kind: "Pod"}, CodeUnsupportedKind, "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantErr(t, decodeErr(t, c.body, MediaJSON, c.hint), c.code, c.path)
		})
	}
}

func TestYAMLLimits(t *testing.T) {
	const envelope = "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\n"
	t.Run("an alias chain past 1 MiB", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(envelope)
		b.WriteString("a: &a \"" + strings.Repeat("x", 1024) + "\"\n")
		prev := "a"
		for _, name := range []string{"b", "c", "d", "e"} {
			b.WriteString(name + ": &" + name + " [")
			for i := range 10 {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString("*" + prev)
			}
			b.WriteString("]\n")
			prev = name
		}
		start := time.Now()
		e := decodeErr(t, b.String(), MediaYAML, Hint{})
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("refusing took %v, more than 100 ms", d)
		}
		wantErr(t, e, CodeInvalidField)
		if !strings.HasPrefix(e.Paths[0], "d[") || !strings.Contains(e.Detail, "expands past") {
			t.Errorf("refused at %q: %s", e.Paths, e.Detail)
		}
	})
	t.Run("nesting past 64 levels", func(t *testing.T) {
		body := envelope + "a: " + strings.Repeat("[", 64) + "1" + strings.Repeat("]", 64) + "\n"
		start := time.Now()
		e := decodeErr(t, body, MediaYAML, Hint{})
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Errorf("refusing took %v, more than 100 ms", d)
		}
		wantErr(t, e, CodeInvalidField, "a"+strings.Repeat("[0]", 63))
		if !strings.Contains(e.Detail, "nests past") {
			t.Errorf("detail %q", e.Detail)
		}
		// Nested mappings and JSON are held to the same depth.
		deepMap := envelope + "a:" + strings.Repeat("\n a:", 0)
		var sb strings.Builder
		sb.WriteString(envelope)
		for i := range 64 {
			sb.WriteString(strings.Repeat(" ", i) + "a:\n")
		}
		sb.WriteString(strings.Repeat(" ", 64) + "b: 1\n")
		_ = deepMap
		wantErr(t, decodeErr(t, sb.String(), MediaYAML, Hint{}), CodeInvalidField)
		wantErr(t, decodeErr(t, "{\"a\": "+strings.Repeat("[", 64)+"1"+strings.Repeat("]", 64)+"}", MediaJSON, Hint{}), CodeInvalidField, "a"+strings.Repeat("[0]", 63))
	})
	t.Run("64 levels are admitted", func(t *testing.T) {
		body := envelope + "a: " + strings.Repeat("[", 63) + "1" + strings.Repeat("]", 63) + "\n"
		e := decodeErr(t, body, MediaYAML, Hint{})
		wantErr(t, e, CodeUnknownField, "a") // the depth passed and the schema refused the key
	})
	t.Run("JSON scalar bytes are counted too", func(t *testing.T) {
		body := "{\"a\": \"" + strings.Repeat("x", MaxScalarBytes+1) + "\"}"
		wantErr(t, decodeErr(t, body, MediaJSON, Hint{}), CodeInvalidField, "a")
	})
}

func TestDecodeTypeMismatchesNameThePath(t *testing.T) {
	const envelope = "apiVersion: lux.latere.ai/v1beta1\nkind: %s\n"
	cases := []struct {
		kind, body, path, detail string
	}{
		{"Model", "spec:\n  targets:\n    - provider: a\n      weight: \"100\"\n", "spec.targets[0].weight", "expected an integer"},
		{"Model", "spec:\n  targets:\n    - provider: a\n      weight: 1.5\n", "spec.targets[0].weight", "expected an integer, not 1.5"},
		{"Model", "spec:\n  targets:\n    - provider: a\n      weight: 99999999999999999999\n", "spec.targets[0].weight", "expected an integer"},
		{"Model", "spec:\n  targets: {}\n", "spec.targets", "expected a list"},
		{"Model", "spec:\n  targets: [1]\n", "spec.targets[0]", "expected an object"},
		{"Model", "spec:\n  pricing: []\n", "spec.pricing", "expected an object"},
		{"Model", "spec:\n  pricing:\n    input: 1.25\n", "spec.pricing.input", "money is a decimal string"},
		{"Model", "spec:\n  pricing:\n    input: \"1.2345678\"\n", "spec.pricing.input", "7 fraction digits"},
		{"Provider", "spec:\n  dialect: [openai]\n", "spec.dialect", "expected a string"},
		{"Provider", "spec:\n  tunnel: yes\n", "spec.tunnel", "expected true or false"},
		{"Provider", "spec:\n  headers: [a]\n", "spec.headers", "expected an object"},
		{"Provider", "spec:\n  headers:\n    X-A: 1\n", "spec.headers[\"X-A\"]", "expected a string"},
		{"Provider", "spec:\n  credential:\n    value: [a]\n", "spec.credential.value", "expected a string"},
		{"Provider", "spec:\n  concurrency: .inf\n", "spec.concurrency", "infinity"},
		{"Key", "spec:\n  expiresAt: tomorrow\n", "spec.expiresAt", "RFC 3339"},
		{"Key", "spec:\n  expiresAt: 5\n", "spec.expiresAt", "RFC 3339"},
		{"Key", "spec:\n  value: 5\n", "spec.value", "expected a string"},
		{"Key", "metadata: 5\n", "metadata", "expected an object"},
		{"Budget", "spec:\n  amount: 5\n", "spec.amount", "money is a decimal string"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			body := strings.Replace(envelope, "%s", c.kind, 1) + c.body
			e := decodeErr(t, body, MediaYAML, Hint{})
			wantErr(t, e, CodeInvalidField, c.path)
			if !strings.Contains(e.Detail, c.detail) {
				t.Errorf("detail %q does not carry %q", e.Detail, c.detail)
			}
		})
	}
}

func TestDecodeAcceptsYAMLForms(t *testing.T) {
	body := `apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: m
  labels: &labels
    team: a
  annotations:
    <<: *labels
    note: |
      two
      lines
    1: numeric key
    !!str 2: tagged key
spec:
  targets:
    - &first {provider: a, weight: 100, priority: 1.0}
    - <<: *first
      provider: b
  pricing:
    input: !!str 1.25
    output: "2"
    per: 1000
  contextWindow: 0x10
  fallback: null
status:
  whatever: [is, ignored]
`
	obj, err := Decode([]byte(body), MediaYAML, Hint{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := obj.(*v1.Model)
	if !ok {
		t.Fatalf("decoded %T", obj)
	}
	if m.Metadata.Annotations["team"] != "a" || m.Metadata.Annotations["note"] != "two\nlines\n" || m.Metadata.Annotations["1"] != "numeric key" || m.Metadata.Annotations["2"] != "tagged key" {
		t.Errorf("annotations = %v", m.Metadata.Annotations)
	}
	if len(m.Spec.Targets) != 2 || *m.Spec.Targets[0].Weight != 100 || m.Spec.Targets[1].Provider != "b" || *m.Spec.Targets[1].Weight != 100 {
		t.Errorf("targets = %+v", m.Spec.Targets)
	}
	if *m.Spec.Pricing.Input != 1_250_000 || m.Spec.Pricing.Per != 1000 || m.Spec.ContextWindow != 16 {
		t.Errorf("pricing = %+v, contextWindow = %d", m.Spec.Pricing, m.Spec.ContextWindow)
	}
	if m.Spec.Fallback != "" {
		t.Errorf("a null field decoded as %q", m.Spec.Fallback)
	}
	if !reflect.DeepEqual(m.Status, v1.ModelStatus{}) {
		t.Errorf("status was not ignored: %+v", m.Status)
	}
	// The JSON-in-YAML rule tolerates leading whitespace, and a media type
	// carries parameters and any case.
	for _, ct := range []string{"application/json; charset=utf-8", "Application/JSON", "text/yaml; charset=utf-8"} {
		if _, err := Decode([]byte("\n  {\"apiVersion\": \"lux.latere.ai/v1beta1\", \"kind\": \"Budget\", \"spec\": {}}"), ct, Hint{}); err != nil {
			t.Errorf("%q: %v", ct, err)
		}
	}
	// A null write-only value is an absent one.
	p, err := Decode([]byte(`{"apiVersion": "lux.latere.ai/v1beta1", "kind": "Provider", "spec": {"credential": {"value": null}}}`), MediaJSON, Hint{})
	if err != nil {
		t.Fatal(err)
	}
	if _, set := p.(*v1.Provider).Spec.Credential.Value(); set {
		t.Error("a null value was recorded")
	}
}

func TestCredentialValueNeverEncodes(t *testing.T) {
	obj, err := Decode(corpusFile(t, "accepted/provider/openai.yaml"), MediaYAML, Hint{})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := obj.(*v1.Provider)
	if !ok {
		t.Fatalf("decoded %T", obj)
	}
	v, set := p.Spec.Credential.Value()
	if !set || v != "sk-live-..." {
		t.Fatalf("Value() = %q, %v", v, set)
	}
	j, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	y, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, enc := range map[string][]byte{"json": j, "yaml": y} {
		if strings.Contains(string(enc), "sk-live") {
			t.Errorf("the %s encoding carries the credential value:\n%s", name, enc)
		}
		if !strings.Contains(string(enc), "Authorization") {
			t.Errorf("the %s encoding lost the credential header:\n%s", name, enc)
		}
	}
	// The same holds for a Key's supplied value.
	k, err := Decode([]byte(`{"apiVersion": "lux.latere.ai/v1beta1", "kind": "Key", "spec": {"models": ["*"], "value": "kv-canary-0002-kv-canary-0002-kv-canary"}}`), MediaJSON, Hint{})
	if err != nil {
		t.Fatal(err)
	}
	if v, set := k.(*v1.Key).Spec.Value(); !set || v != "kv-canary-0002-kv-canary-0002-kv-canary" {
		t.Errorf("Value() = %q, %v", v, set)
	}
	j, _ = json.Marshal(k)
	y, _ = yaml.Marshal(k)
	if strings.Contains(string(j), "canary") || strings.Contains(string(y), "canary") {
		t.Errorf("an encoding of the Key carries its value:\n%s\n%s", j, y)
	}
}

func TestDecodeDoesNotPanicOnKindHelpers(t *testing.T) {
	for _, f := range []func(){
		func() { newObject("Pod") },
		func() { metaOf(nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("no panic for an unknown kind")
				}
			}()
			f()
		}()
	}
}
