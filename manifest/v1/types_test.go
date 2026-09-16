// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnums(t *testing.T) {
	type enum interface{ Valid() bool }
	cases := []struct {
		good []enum
		bad  enum
	}{
		{[]enum{DialectOpenAI, DialectAnthropic, DialectGemini, DialectLux}, Dialect("azure")},
		{[]enum{SchemeBearer, SchemeRaw}, Scheme("basic")},
		{[]enum{DiscoveryAuto, DiscoveryNone}, DiscoveryMode("manual")},
		{[]enum{HealthProbe, HealthPassive, HealthNone}, HealthMode("active")},
		{[]enum{HealthUnknown, HealthHealthy, HealthDegraded, HealthUnreachable}, HealthState("Down")},
		{[]enum{TunnelConnected, TunnelDisconnected}, TunnelState("Open")},
		{[]enum{FallbackOnError, FallbackNever}, Fallback("always")},
		{[]enum{ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ModalityFile, ModalityEmbedding}, Modality("smell")},
		{[]enum{SourceDeclared, SourceDiscovered}, Source("guessed")},
		{[]enum{KeyActive, KeyDisabled, KeyExpired, KeyExhausted}, KeyState("Lost")},
		{[]enum{BudgetOpen, BudgetExhausted}, BudgetState("Closed")},
	}
	for _, c := range cases {
		for _, g := range c.good {
			if !g.Valid() {
				t.Errorf("%T %v refused", g, g)
			}
		}
		if c.bad.Valid() {
			t.Errorf("%T %v accepted", c.bad, c.bad)
		}
	}
}

func TestDialectCredentialDefaults(t *testing.T) {
	cases := map[Dialect]struct {
		header string
		scheme Scheme
	}{
		DialectOpenAI:    {"Authorization", SchemeBearer},
		DialectLux:       {"Authorization", SchemeBearer},
		DialectAnthropic: {"x-api-key", SchemeRaw},
		DialectGemini:    {"x-goog-api-key", SchemeRaw},
		Dialect("other"): {"", ""},
	}
	for d, want := range cases {
		if got := d.CredentialHeader(); got != want.header {
			t.Errorf("%s header = %q, want %q", d, got, want.header)
		}
		if got := d.CredentialScheme(); got != want.scheme {
			t.Errorf("%s scheme = %q, want %q", d, got, want.scheme)
		}
	}
}

func TestKinds(t *testing.T) {
	for _, k := range []string{KindProvider, KindModel, KindKey, KindBudget} {
		if !KnownKind(k) {
			t.Errorf("%s unknown", k)
		}
	}
	if KnownKind("Pod") {
		t.Error("Pod is a kind")
	}
	objects := []Object{
		&Provider{Metadata: ObjectMeta{Name: "p"}, Status: ProviderStatus{ID: "prv_1", Owner: "o"}},
		&Model{Metadata: ObjectMeta{Name: "m"}, Status: ModelStatus{ID: "mdl_1", Owner: "o"}},
		&Key{Metadata: ObjectMeta{Name: "k"}, Status: KeyStatus{ID: "key_1", Owner: "o"}},
		&Budget{Metadata: ObjectMeta{Name: "b"}, Status: BudgetStatus{ID: "bud_1", Owner: "o"}},
	}
	for i, o := range objects {
		kind := []string{KindProvider, KindModel, KindKey, KindBudget}[i]
		if o.Kind() != kind || o.Owner() != "o" || o.ID() == "" || o.Name() == "" {
			t.Errorf("%T: Kind %q ID %q Owner %q Name %q", o, o.Kind(), o.ID(), o.Owner(), o.Name())
		}
		out, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"apiVersion":"lux.latere.ai/v1beta1","kind":"` + kind + `","metadata":`
		if !strings.HasPrefix(string(out), want) {
			t.Errorf("%T encodes as %s, want the envelope first", o, out)
		}
	}
}

func TestWriteOnlyValuesNeverEncode(t *testing.T) {
	p := &Provider{Spec: ProviderSpec{Credential: &Credential{Header: "Authorization"}}}
	if _, ok := p.Spec.Credential.Value(); ok {
		t.Error("a fresh credential reports a value")
	}
	p.Spec.Credential.SetValue("sk-canary-0001")
	k := &Key{}
	k.Spec.SetValue("kv-canary-0002-kv-canary-0002-kv-canary")
	h := &Key{}
	h.Spec.SetValueSHA256("canary0003" + strings.Repeat("0", 54))
	for _, o := range []any{p, *p, k, *k, h, *h} {
		out, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "canary") {
			t.Errorf("%T encodes its write-only value: %s", o, out)
		}
	}
	if v, ok := p.Spec.Credential.Value(); !ok || v != "sk-canary-0001" {
		t.Errorf("Value() = %q, %v", v, ok)
	}
	if v, ok := k.Spec.Value(); !ok || v != "kv-canary-0002-kv-canary-0002-kv-canary" {
		t.Errorf("Value() = %q, %v", v, ok)
	}
	if v, ok := h.Spec.ValueSHA256(); !ok || v != "canary0003"+strings.Repeat("0", 54) {
		t.Errorf("ValueSHA256() = %q, %v", v, ok)
	}
	if _, ok := h.Spec.Value(); ok {
		t.Error("a hash-supplied Key reports a value")
	}
	if _, ok := k.Spec.ValueSHA256(); ok {
		t.Error("a value-supplied Key reports a hash")
	}
	p.Spec.Credential.ClearValue()
	k.Spec.ClearValue()
	h.Spec.ClearValueSHA256()
	if _, ok := h.Spec.ValueSHA256(); ok {
		t.Error("ClearValueSHA256 left the hash")
	}
	if _, ok := p.Spec.Credential.Value(); ok {
		t.Error("ClearValue left the credential value")
	}
	if _, ok := k.Spec.Value(); ok {
		t.Error("ClearValue left the key value")
	}
	// An empty value that was written is told from an absent one.
	p.Spec.Credential.SetValue("")
	if v, ok := p.Spec.Credential.Value(); !ok || v != "" {
		t.Errorf("an empty written value reads %q, %v", v, ok)
	}
}
