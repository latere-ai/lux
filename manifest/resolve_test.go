// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

func TestTheExamplesResolve(t *testing.T) {
	o := corpusOptions(t)
	for _, name := range examples {
		t.Run(name, func(t *testing.T) {
			obj, err := Decode(corpusFile(t, name), MediaYAML, Hint{})
			if err != nil {
				t.Fatal(err)
			}
			r, err := Resolve(context.Background(), obj, o)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(r.Warnings) != 0 {
				t.Errorf("warnings: %q", r.Warnings)
			}
			if r.Object.Name() != obj.Name() || r.Object.Kind() != obj.Kind() {
				t.Errorf("resolved %s %q from %s %q", r.Object.Kind(), r.Object.Name(), obj.Kind(), obj.Name())
			}
		})
	}
}

func TestNamesNeverLookLikeIds(t *testing.T) {
	o := corpusOptions(t)
	bodies := map[string]string{v1.KindProvider: minProvider, v1.KindModel: minModel, v1.KindKey: minKey, v1.KindBudget: minBudget}
	for kind, body := range bodies {
		for _, prefix := range v1.KindPrefixes {
			name := prefix + "01j9zk2p7q8r9s0t1u2v3w4x5y"
			t.Run(kind+" "+name, func(t *testing.T) {
				wantErr(t, resolveErr(t, head(kind, name)+body, o), CodeReservedPrefix, "metadata.name")
			})
		}
	}
	t.Run("openai/mdl_x is a Model name", func(t *testing.T) {
		r := mustResolve(t, head(v1.KindModel, "openai/mdl_x")+minModel, o)
		if r.Object.Name() != "openai/mdl_x" {
			t.Errorf("name = %q", r.Object.Name())
		}
	})
}

func TestDefaultsFillOnlyAbsentFields(t *testing.T) {
	o := corpusOptions(t)
	o.Defaults = Defaults{RequestsPerMinute: 30, TokensPerMinute: 4000, Timeout: 90 * time.Second}
	t.Run("provider", func(t *testing.T) {
		r := mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: anthropic\n  baseURL: https://api.example.com/v1\n", o)
		p, ok := r.Object.(*v1.Provider)
		if !ok {
			t.Fatalf("%T", r.Object)
		}
		want := v1.ProviderSpec{
			Dialect:    v1.DialectAnthropic,
			BaseURL:    "https://api.example.com/v1",
			Credential: &v1.Credential{Header: "x-api-key", Scheme: v1.SchemeRaw},
			Discovery:  v1.Discovery{Mode: v1.DiscoveryAuto},
			Health:     v1.Health{Mode: v1.HealthProbe},
			Timeout:    "1m30s",
		}
		if !reflect.DeepEqual(p.Spec, want) {
			t.Errorf("spec = %+v, want %+v", p.Spec, want)
		}
		if p.Status.Warnings == nil || len(p.Status.Warnings) != 0 {
			t.Errorf("status.warnings = %#v, want an empty list", p.Status.Warnings)
		}
		// Nothing the caller set is overwritten.
		r = mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: anthropic\n  baseURL: https://api.example.com/v1\n  credential: {header: api-key, scheme: bearer}\n  discovery: {mode: none}\n  health: {mode: none}\n  timeout: 5s\n  concurrency: 3\n", o)
		p = r.Object.(*v1.Provider)
		if p.Spec.Credential.Header != "api-key" || p.Spec.Credential.Scheme != v1.SchemeBearer || p.Spec.Discovery.Mode != v1.DiscoveryNone || p.Spec.Health.Mode != v1.HealthNone || p.Spec.Timeout != "5s" || p.Spec.Concurrency != 3 {
			t.Errorf("a set field was overwritten: %+v", p.Spec)
		}
		// No configured timeout leaves the field absent; a tunneled
		// Provider gets no credential block.
		o2 := o
		o2.Defaults.Timeout = 0
		r = mustResolve(t, head(v1.KindProvider, "laptop")+"spec:\n  dialect: openai\n  tunnel: true\n", o2)
		p = r.Object.(*v1.Provider)
		if p.Spec.Timeout != "" || p.Spec.Credential != nil {
			t.Errorf("tunnel provider spec = %+v", p.Spec)
		}
	})
	t.Run("model", func(t *testing.T) {
		r := mustResolve(t, head(v1.KindModel, "m")+"spec:\n  targets:\n    - provider: openai\n  pricing:\n    input: \"1\"\n    output: \"2\"\n", o)
		m := r.Object.(*v1.Model)
		one, two := v1.Money(1_000_000), v1.Money(2_000_000)
		want := v1.ModelSpec{
			Targets:    []v1.Target{{Provider: "openai", Model: "m", Weight: ptr(100), Priority: 0}},
			Fallback:   v1.FallbackOnError,
			Pricing:    &v1.Pricing{Currency: "USD", Per: 1_000_000, Input: &one, Output: &two, CachedInput: &one, CacheWrite: &one},
			Modalities: v1.Modalities{Input: []v1.Modality{v1.ModalityText}, Output: []v1.Modality{v1.ModalityText}},
		}
		if !reflect.DeepEqual(m.Spec, want) {
			got, _ := json.Marshal(m.Spec)
			t.Errorf("spec = %s", got)
		}
		r = mustResolve(t, head(v1.KindModel, "m")+"spec:\n  targets:\n    - {provider: openai, model: up, weight: 0, priority: 2}\n    - {provider: azure, weight: 7}\n  fallback: never\n  pricing:\n    currency: EUR\n    per: 1000\n    input: \"1\"\n    output: \"2\"\n    cachedInput: \"0\"\n    cacheWrite: \"0.5\"\n  modalities:\n    input: [image]\n    output: [embedding]\n", o)
		m = r.Object.(*v1.Model)
		if m.Spec.Targets[0].Model != "up" || *m.Spec.Targets[0].Weight != 0 || m.Spec.Targets[0].Priority != 2 || *m.Spec.Targets[1].Weight != 7 || m.Spec.Fallback != v1.FallbackNever || m.Spec.Pricing.Currency != "EUR" || m.Spec.Pricing.Per != 1000 || *m.Spec.Pricing.CachedInput != 0 || *m.Spec.Pricing.CacheWrite != 500_000 || m.Spec.Modalities.Input[0] != v1.ModalityImage || m.Spec.Modalities.Output[0] != v1.ModalityEmbedding {
			got, _ := json.Marshal(m.Spec)
			t.Errorf("a set field was overwritten: %s", got)
		}
	})
	t.Run("key", func(t *testing.T) {
		r := mustResolve(t, head(v1.KindKey, "k")+"spec:\n  models: [gpt-5]\n  limits:\n    spend: {amount: \"5\", window: 24h}\n  ttl: 1h\n", o)
		k := r.Object.(*v1.Key)
		if *k.Spec.Limits.RequestsPerMinute != 30 || *k.Spec.Limits.TokensPerMinute != 4000 || k.Spec.Limits.Spend.Currency != "USD" {
			t.Errorf("limits = %+v", k.Spec.Limits)
		}
		if k.Spec.AllowUnpriced || k.Spec.Passthrough || k.Spec.Disabled {
			t.Errorf("bools = %v %v %v", k.Spec.AllowUnpriced, k.Spec.Passthrough, k.Spec.Disabled)
		}
		want := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
		if !k.Status.ExpiresAt.Equal(want) {
			t.Errorf("status.expiresAt = %v, want Now plus ttl %v", k.Status.ExpiresAt, want)
		}
		if !k.Spec.ExpiresAt.IsZero() {
			t.Error("spec.expiresAt was filled from ttl; ttl and expiresAt are exclusive")
		}
		r = mustResolve(t, head(v1.KindKey, "k")+"spec:\n  models: [gpt-5]\n  limits:\n    requestsPerMinute: 0\n    tokensPerMinute: 1\n    spend: {amount: \"5\", currency: EUR, window: month}\n  expiresAt: 2027-01-01T00:00:00Z\n  allowUnpriced: true\n  passthrough: true\n  disabled: true\n", o)
		k = r.Object.(*v1.Key)
		if *k.Spec.Limits.RequestsPerMinute != 0 || *k.Spec.Limits.TokensPerMinute != 1 || k.Spec.Limits.Spend.Currency != "EUR" || !k.Spec.AllowUnpriced || !k.Spec.Passthrough || !k.Spec.Disabled {
			t.Errorf("a set field was overwritten: %+v", k.Spec)
		}
		if !k.Status.ExpiresAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("status.expiresAt = %v, want spec.expiresAt", k.Status.ExpiresAt)
		}
		r = mustResolve(t, head(v1.KindKey, "k")+minKey, o)
		if !r.Object.(*v1.Key).Status.ExpiresAt.IsZero() {
			t.Error("a Key without ttl or expiresAt got an expiry")
		}
	})
	t.Run("budget", func(t *testing.T) {
		r := mustResolve(t, head(v1.KindBudget, "b")+minBudget, o)
		b := r.Object.(*v1.Budget)
		if b.Spec.Currency != "USD" || b.Spec.Window != v1.WindowMonth || b.Spec.Hard == nil || !*b.Spec.Hard {
			t.Errorf("spec = %+v", b.Spec)
		}
		r = mustResolve(t, head(v1.KindBudget, "b")+"spec:\n  amount: \"10\"\n  currency: EUR\n  window: 24h\n  hard: false\n", o)
		b = r.Object.(*v1.Budget)
		if b.Spec.Currency != "EUR" || b.Spec.Window != "24h" || *b.Spec.Hard {
			t.Errorf("a set field was overwritten: %+v", b.Spec)
		}
	})
}

func TestDialectDefaults(t *testing.T) {
	o := corpusOptions(t)
	cases := map[v1.Dialect]v1.Credential{
		v1.DialectOpenAI:    {Header: "Authorization", Scheme: v1.SchemeBearer},
		v1.DialectAnthropic: {Header: "x-api-key", Scheme: v1.SchemeRaw},
		v1.DialectGemini:    {Header: "x-goog-api-key", Scheme: v1.SchemeRaw},
		v1.DialectLux:       {Header: "Authorization", Scheme: v1.SchemeBearer},
	}
	for d, want := range cases {
		t.Run(string(d), func(t *testing.T) {
			r := mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: "+string(d)+"\n  baseURL: https://api.example.com/v1\n  credential:\n    value: secret\n", o)
			c := r.Object.(*v1.Provider).Spec.Credential
			if c.Header != want.Header || c.Scheme != want.Scheme {
				t.Errorf("credential = %s %s, want %s %s", c.Header, c.Scheme, want.Header, want.Scheme)
			}
			if v, ok := c.Value(); !ok || v != "secret" {
				t.Errorf("the value was lost through Resolve: %q %v", v, ok)
			}
			// A header the caller set keeps the scheme it implies only if
			// the caller set that too: the default scheme is the dialect's.
			r = mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: "+string(d)+"\n  baseURL: https://api.example.com/v1\n  credential:\n    header: api-key\n", o)
			c = r.Object.(*v1.Provider).Spec.Credential
			if c.Header != "api-key" || c.Scheme != want.Scheme {
				t.Errorf("credential = %s %s", c.Header, c.Scheme)
			}
		})
	}
}

func TestNameGeneration(t *testing.T) {
	o := corpusOptions(t)
	body := "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\n" + minBudget
	r := mustResolve(t, body, o)
	if r.Object.Name() != "fixed-name-0000" {
		t.Errorf("name = %q, want NewName's", r.Object.Name())
	}
	r = mustResolve(t, head(v1.KindBudget, "given")+minBudget, o)
	if r.Object.Name() != "given" {
		t.Errorf("a given name was replaced by %q", r.Object.Name())
	}
	o.NewName = nil
	wantErr(t, resolveErr(t, body, o), CodeMissingField, "metadata.name")
	for _, kind := range []string{v1.KindProvider, v1.KindModel, v1.KindKey} {
		bodies := map[string]string{v1.KindProvider: minProvider, v1.KindModel: minModel, v1.KindKey: minKey}
		wantErr(t, resolveErr(t, "apiVersion: lux.latere.ai/v1beta1\nkind: "+kind+"\n"+bodies[kind], o), CodeMissingField, "metadata.name")
	}
}

// TestLookupFailurePassesThrough: a Lookup error that is neither a
// refusal nor one of the two sentinels is the Lookup's own failure, a
// catalog or a store that could not answer, and Resolve returns it as a
// plain error and not as a refusal, so the API answers for the store and
// not for the authorizer.
func TestLookupFailurePassesThrough(t *testing.T) {
	o := corpusOptions(t)
	lookup := corpusLookup(t)
	lookup.refused = map[string]error{"broken/*": errors.New("catalog: the store is not reachable")}
	o.Lookup = lookup
	_, err := Resolve(context.Background(), mustDecode(t, head(v1.KindKey, "k")+"spec:\n  models: [\"broken/*\"]\n"), o)
	if err == nil {
		t.Fatal("Resolve accepted a Key whose Lookup failed")
	}
	var e *Error
	if errors.As(err, &e) {
		t.Fatalf("a Lookup failure was turned into the refusal %s", e.Code)
	}
	if !strings.Contains(err.Error(), "spec.models[0]") || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("err = %v, wants the path and the cause", err)
	}
}

func TestLookupErrors(t *testing.T) {
	o := corpusOptions(t)
	lookup := corpusLookup(t)
	lookup.refused = map[string]error{
		"denied-provider": ErrNotFound,
		"refused-budget":  refuse(CodeNotFound, "budget.draw denied", "x"),
		"down-provider":   ErrAuthorizerUnavailable,
		"down/*":          fmt.Errorf("%w: dial tcp: connection refused", ErrAuthorizerUnavailable),
		"broken/*":        errors.New("catalog: the store is not reachable"),
		"gone/*":          refuse(CodeAuthorizerUnavailable, "timeout", "y"),
	}
	o.Lookup = lookup
	cases := []struct {
		name, body string
		code       Code
		path       string
		detail     string
	}{
		{"missing provider", head(v1.KindModel, "m") + "spec:\n  targets:\n    - provider: openai\n    - provider: nobody\n", CodeNotFound, "spec.targets[1].provider", "not found"},
		{"denied provider", head(v1.KindModel, "m") + "spec:\n  targets:\n    - provider: denied-provider\n", CodeNotFound, "spec.targets[0].provider", ""},
		{"unavailable provider", head(v1.KindModel, "m") + "spec:\n  targets:\n    - provider: down-provider\n", CodeAuthorizerUnavailable, "spec.targets[0].provider", ""},
		{"missing budget", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  budget: nobody\n", CodeNotFound, "spec.budget", ""},
		{"refused budget", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  budget: refused-budget\n", CodeNotFound, "spec.budget", "budget.draw denied"},
		{"unavailable selector", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5, \"down/*\"]\n", CodeAuthorizerUnavailable, "spec.models[1]", "connection refused"},
		{"unavailable selector as an Error", head(v1.KindKey, "k") + "spec:\n  models: [\"gone/*\"]\n", CodeAuthorizerUnavailable, "spec.models[0]", "timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := resolveErr(t, c.body, o)
			wantErr(t, e, c.code, c.path)
			if !strings.Contains(e.Detail, c.detail) {
				t.Errorf("detail %q lacks %q", e.Detail, c.detail)
			}
		})
	}
	t.Run("a nil Lookup cannot decide", func(t *testing.T) {
		o2 := o
		o2.Lookup = nil
		wantErr(t, resolveErr(t, head(v1.KindModel, "m")+minModel, o2), CodeAuthorizerUnavailable, "spec.targets[0].provider")
		wantErr(t, resolveErr(t, head(v1.KindKey, "k")+minKey, o2), CodeAuthorizerUnavailable, "spec.models")
		// A Provider and a Budget name nothing and need no Lookup.
		mustResolve(t, head(v1.KindProvider, "p")+minProvider, o2)
		mustResolve(t, head(v1.KindBudget, "b")+minBudget, o2)
	})
	t.Run("a nil object with no error is not found", func(t *testing.T) {
		o2 := o
		o2.Lookup = nilLookup{}
		wantErr(t, resolveErr(t, head(v1.KindModel, "m")+minModel, o2), CodeNotFound, "spec.targets[0].provider")
		wantErr(t, resolveErr(t, head(v1.KindKey, "k")+"spec:\n  models: [gpt-5]\n  budget: b\n", o2), CodeNotFound, "spec.budget")
	})
}

// nilLookup answers nil, nil for every reference.
type nilLookup struct{}

func (nilLookup) Provider(context.Context, string) (*v1.Provider, error) { return nil, nil }
func (nilLookup) Budget(context.Context, string) (*v1.Budget, error)     { return nil, nil }
func (nilLookup) Models(context.Context, string) ([]v1.ModelRef, error) {
	return nil, nil
}

func TestSelectorsRecordTheirMatches(t *testing.T) {
	o := corpusOptions(t)
	r := mustResolve(t, head(v1.KindKey, "k")+"spec:\n  models: [\"gpt-5\", \"anthropic/*\", \"nothing-*\", \"*\"]\n", o)
	k := r.Object.(*v1.Key)
	all := corpusLookup(t).models
	want := []v1.SelectorStatus{
		{Selector: "gpt-5", Matched: []string{"gpt-5"}},
		{Selector: "anthropic/*", Matched: []string{"anthropic/claude-opus-4", "anthropic/claude-sonnet-4"}},
		{Selector: "nothing-*", Matched: []string{}},
		{Selector: "*", Matched: sorted(all)},
	}
	if !reflect.DeepEqual(k.Status.Selectors, want) {
		got, _ := json.Marshal(k.Status.Selectors)
		t.Errorf("selectors = %s", got)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], `"nothing-*"`) {
		t.Errorf("warnings = %q, want one about nothing-*", r.Warnings)
	}
	if !reflect.DeepEqual(k.Status.Warnings, r.Warnings) {
		t.Errorf("status.warnings %q differ from Resolved.Warnings %q", k.Status.Warnings, r.Warnings)
	}
}

func TestImmutableFields(t *testing.T) {
	o := corpusOptions(t)
	o.FileMode = true // so valueFrom is admitted on both sides
	cases := []struct {
		kind, before, after string
		paths               []string
	}{
		{
			v1.KindProvider,
			head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n  baseURL: https://a.example.com/v1\n  credential: {valueFrom: {env: A}}\n",
			head(v1.KindProvider, "q") + "spec:\n  dialect: anthropic\n  tunnel: true\n",
			[]string{"metadata.name", "spec.dialect", "spec.tunnel", "spec.credential.valueFrom.env"},
		},
		{
			v1.KindKey,
			head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  ttl: 1h\n  valueFrom: {env: A}\n",
			head(v1.KindKey, "j") + "spec:\n  models: [gpt-5]\n  ttl: 2h\n  valueFrom: {env: B}\n",
			[]string{"metadata.name", "spec.ttl", "spec.valueFrom.env"},
		},
		{
			v1.KindBudget,
			head(v1.KindBudget, "b") + "spec:\n  amount: \"10\"\n",
			head(v1.KindBudget, "c") + "spec:\n  amount: \"20\"\n  currency: EUR\n  window: 24h\n",
			[]string{"metadata.name", "spec.currency", "spec.window"},
		},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			before := mustResolve(t, c.before, o)
			o2 := o
			o2.Existing = before.Object
			e := resolveErr(t, c.after, o2)
			wantErr(t, e, CodeImmutableField, c.paths...)
			// The same object again changes nothing, defaults included.
			mustResolve(t, c.before, o2)
		})
	}
	t.Run("mutable fields change freely", func(t *testing.T) {
		before := mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: openai\n  baseURL: https://a.example.com/v1\n", o)
		o2 := o
		o2.Existing = before.Object
		mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: openai\n  baseURL: https://b.example.com/v2\n  timeout: 30s\n  headers: {X-A: b}\n", o2)
		bud := mustResolve(t, head(v1.KindBudget, "b")+"spec:\n  amount: \"10\"\n  window: 1h\n", o)
		o2.Existing = bud.Object
		mustResolve(t, head(v1.KindBudget, "b")+"spec:\n  amount: \"20\"\n  window: 60m\n  hard: false\n", o2)
		key := mustResolve(t, head(v1.KindKey, "k")+"spec:\n  models: [gpt-5]\n  ttl: 1h\n", o)
		o2.Existing = key.Object
		mustResolve(t, head(v1.KindKey, "k")+"spec:\n  models: [\"*\"]\n  ttl: 60m\n  disabled: true\n", o2)
		model := mustResolve(t, head(v1.KindModel, "m")+minModel, o)
		o2.Existing = model.Object
		mustResolve(t, head(v1.KindModel, "m")+"spec:\n  targets:\n    - provider: azure\n", o2)
		wantErr(t, resolveErr(t, head(v1.KindModel, "n")+minModel, o2), CodeImmutableField, "metadata.name")
	})
	t.Run("an Existing of another kind is a caller's mistake", func(t *testing.T) {
		o2 := o
		o2.Existing = mustResolve(t, head(v1.KindBudget, "b")+minBudget, o).Object
		for _, body := range []string{head(v1.KindProvider, "p") + minProvider, head(v1.KindModel, "m") + minModel, head(v1.KindKey, "k") + minKey} {
			_, err := Resolve(context.Background(), mustDecode(t, body), o2)
			var e *Error
			if err == nil || errors.As(err, &e) {
				t.Errorf("Resolve = %v, want a plain error", err)
			}
		}
		o2.Existing = mustResolve(t, head(v1.KindKey, "k")+minKey, o).Object
		if _, err := Resolve(context.Background(), mustDecode(t, head(v1.KindBudget, "b")+minBudget), o2); err == nil {
			t.Error("a Key as the existing Budget was accepted")
		}
	})
}

func TestCredentialUpdate(t *testing.T) {
	o := corpusOptions(t)
	stored := mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: openai\n  baseURL: https://a.example.com/v1\n  credential: {value: first}\n", o)
	// The store took the value out; the existing object carries none.
	stored.Object.(*v1.Provider).Spec.Credential.ClearValue()
	o.Existing = stored.Object
	without := mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: openai\n  baseURL: https://a.example.com/v1\n", o)
	if _, ok := without.Object.(*v1.Provider).Spec.Credential.Value(); ok {
		t.Error("an update without a value resolved with one; the stored value is kept by its absence")
	}
	with := mustResolve(t, head(v1.KindProvider, "p")+"spec:\n  dialect: openai\n  baseURL: https://a.example.com/v1\n  credential: {value: second}\n", o)
	if v, ok := with.Object.(*v1.Provider).Spec.Credential.Value(); !ok || v != "second" {
		t.Errorf("an update with a value resolved to %q %v; the API bumps the version from its presence", v, ok)
	}
}

func TestLimits(t *testing.T) {
	o := corpusOptions(t)
	o.Defaults = Defaults{RequestsPerMinute: 60, TokensPerMinute: 1000}
	key := func(spec string) string { return head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n" + spec }
	cases := []struct {
		name   string
		limits Limits
		body   string
		code   Code
		path   string
		detail string
	}{
		{"zero limits are no limit", Limits{}, key("  limits: {requestsPerMinute: 0, tokensPerMinute: 0}\n"), "", "", ""},
		{"within", Limits{MaxRequestsPerMinute: 100, MaxTokensPerMinute: 2000, MaxSpend: 50_000_000, MaxTTL: 720 * time.Hour}, key("  limits:\n    requestsPerMinute: 100\n    spend: {amount: \"50\", window: 24h}\n  ttl: 720h\n"), "", "", ""},
		{"requests above", Limits{MaxRequestsPerMinute: 50}, key(""), CodeCeilingExceeded, "spec.limits.requestsPerMinute", "requestsPerMinute 60 is above the ceiling 50"},
		{"requests unlimited", Limits{MaxRequestsPerMinute: 50}, key("  limits: {requestsPerMinute: 0}\n"), CodeCeilingExceeded, "spec.limits.requestsPerMinute", "0 is no limit"},
		{"tokens above", Limits{MaxTokensPerMinute: 500}, key(""), CodeCeilingExceeded, "spec.limits.tokensPerMinute", "1000 is above the ceiling 500"},
		{"spend above", Limits{MaxSpend: 5_000_000}, key("  limits:\n    spend: {amount: \"5.000001\", window: 24h}\n"), CodeCeilingExceeded, "spec.limits.spend.amount", "5.000001 is above the ceiling 5"},
		{"spend absent", Limits{MaxSpend: 5_000_000}, key(""), CodeCeilingExceeded, "spec.limits.spend.amount", "no spend limit is set"},
		{"ttl above", Limits{MaxTTL: time.Hour}, key("  ttl: 61m\n"), CodeCeilingExceeded, "spec.ttl", "1h1m"},
		{"expiresAt above", Limits{MaxTTL: time.Hour}, key("  expiresAt: 2026-09-13T11:00:01Z\n"), CodeCeilingExceeded, "spec.expiresAt", "above the ceiling 1h"},
		{"expiresAt within", Limits{MaxTTL: time.Hour}, key("  expiresAt: 2026-09-13T11:00:00Z\n"), "", "", ""},
		{"never expires", Limits{MaxTTL: time.Hour}, key(""), CodeCeilingExceeded, "spec.ttl", "never expires"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o2 := o
			o2.Limits = c.limits
			if c.code == "" {
				mustResolve(t, c.body, o2)
				return
			}
			e := resolveErr(t, c.body, o2)
			wantErr(t, e, c.code, c.path)
			if !strings.Contains(e.Detail, c.detail) {
				t.Errorf("detail %q lacks %q", e.Detail, c.detail)
			}
		})
	}
	t.Run("expiresAt on an update counts from createdAt", func(t *testing.T) {
		o2 := o
		o2.Limits = Limits{MaxTTL: 2 * time.Hour}
		existing := mustResolve(t, key("  expiresAt: 2026-09-13T11:30:00Z\n"), o2)
		existing.Object.(*v1.Key).Status.CreatedAt = time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
		o2.Existing = existing.Object
		// 09:00 plus 2h is 11:00, so 11:30 is above the ceiling now.
		wantErr(t, resolveErr(t, key("  expiresAt: 2026-09-13T11:30:00Z\n"), o2), CodeCeilingExceeded, "spec.expiresAt")
		// A ttl update preserves its recorded deadline even if an older writer
		// persisted a creation timestamp inconsistent with the resolution clock.
		o2.Limits = Limits{}
		o2.Existing = nil
		existing = mustResolve(t, key("  ttl: 1h\n"), o2)
		existing.Object.(*v1.Key).Status.CreatedAt = time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
		o2.Existing = existing.Object
		r := mustResolve(t, key("  ttl: 1h\n"), o2)
		if got := r.Object.(*v1.Key).Status.ExpiresAt; !got.Equal(existing.Object.(*v1.Key).Status.ExpiresAt) {
			t.Errorf("status.expiresAt = %v, want the persisted deadline", got)
		}
	})
}

func TestResolveIsDeterministicAndLeavesTheInput(t *testing.T) {
	o := corpusOptions(t)
	in := mustDecode(t, corpusFileString(t, "accepted/key/run-42.yaml"))
	before, _ := json.Marshal(in)
	a, err := Resolve(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resolve(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a.Object)
	jb, _ := json.Marshal(b.Object)
	if string(ja) != string(jb) {
		t.Errorf("two resolves differ:\n%s\n%s", ja, jb)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Errorf("Resolve changed its input:\n%s\n%s", before, after)
	}
	if _, err := Resolve(context.Background(), nil, o); err == nil {
		t.Error("a nil object resolved")
	}
	if _, err := Resolve(context.Background(), otherKind{}, o); err == nil {
		t.Error("an object of another kind resolved")
	}
	// Status in the input is dropped and rebuilt.
	in.(*v1.Key).Status.ID = "key_x"
	in.(*v1.Key).Status.Warnings = []string{"stale"}
	r, err := Resolve(context.Background(), in, o)
	if err != nil {
		t.Fatal(err)
	}
	if k := r.Object.(*v1.Key); k.Status.ID != "" || slices.Contains(k.Status.Warnings, "stale") {
		t.Errorf("status carried over: %+v", k.Status)
	}
}

// otherKind is an Object of no kind this package resolves.
type otherKind struct{}

func (otherKind) Kind() string  { return "Other" }
func (otherKind) ID() string    { return "" }
func (otherKind) Owner() string { return "" }
func (otherKind) Name() string  { return "" }

func corpusFileString(t *testing.T, name string) string {
	t.Helper()
	return string(corpusFile(t, name))
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sortStrings(out)
	return out
}
