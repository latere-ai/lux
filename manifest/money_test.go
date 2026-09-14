// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	v1 "latere.ai/x/lux/manifest/v1"
)

// moneyFields are the keys of the schema whose values are money strings.
var moneyFields = map[string]bool{"input": true, "output": true, "cachedInput": true, "cacheWrite": true, "amount": true}

func TestMoney(t *testing.T) {
	n := 0
	for _, name := range corpusCases(t, "accepted") {
		j, err := yaml.YAMLToJSON(corpusFile(t, name))
		if err != nil {
			t.Fatal(err)
		}
		var tree map[string]any
		if err := json.Unmarshal(j, &tree); err != nil {
			t.Fatal(err)
		}
		walkStrings(tree, "", func(key, s string) {
			if !moneyFields[key] {
				return
			}
			n++
			m, err := v1.ParseMoney(s)
			if err != nil {
				t.Errorf("%s: ParseMoney(%q): %v", name, s, err)
				return
			}
			// Rendering is lossless in the amount and shortest in the text:
			// trailing zeros of the fraction are dropped.
			if back, err := v1.ParseMoney(m.String()); err != nil || back != m {
				t.Errorf("%s: %q renders as %q, which reads back as %d, %v", name, s, m.String(), back, err)
			}
			if want := shortest(s); m.String() != want {
				t.Errorf("%s: %q renders back as %q, want %q", name, s, m.String(), want)
			}
		})
	}
	if n < 10 {
		t.Errorf("only %d money values in the corpus", n)
	}
	o := corpusOptions(t)
	wantErr(t, resolveErr(t, head(v1.KindBudget, "b")+"spec:\n  amount: \"1.1234567\"\n", o), CodeInvalidField, "spec.amount")
	wantErr(t, resolveErr(t, head(v1.KindBudget, "b")+"spec:\n  amount: \"1234567890123\"\n", o), CodeInvalidField, "spec.amount")
	mustResolve(t, head(v1.KindBudget, "b")+"spec:\n  amount: \"123456789012.123456\"\n", o)
}

// shortest is a money string with the trailing zeros of its fraction
// dropped, the form Money renders.
func shortest(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// walkStrings visits every string leaf of a generic tree with the key it
// sits under; a list's items carry no key.
func walkStrings(v any, key string, f func(key, s string)) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			walkStrings(child, k, f)
		}
	case []any:
		for _, child := range v {
			walkStrings(child, "", f)
		}
	case string:
		f(key, v)
	}
}
