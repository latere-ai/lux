// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// subject is the owner of every object the suite writes.
const subject = "https://login.example.com|alice"

// The assertions are helpers rather than inline branches so a case's
// statements all execute on a conforming store and a failure is one
// line naming what was checked.

func noErr(t testing.TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func wantErr(t testing.TB, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func equal[T comparable](t testing.TB, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func truth(t testing.TB, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Fatalf("%s: false, want true", what)
	}
}

// as is the checked type assertion of an object to its kind's type.
func as[T v1.Object](t testing.TB, obj v1.Object) T {
	t.Helper()
	x, ok := obj.(T)
	if !ok {
		var zero T
		t.Fatalf("object is a %T, want %T", obj, zero)
	}
	return x
}

func sameNames(t testing.TB, objs []v1.Object, want ...string) {
	t.Helper()
	got := make([]string, 0, len(objs))
	for _, o := range objs {
		got = append(got, o.Name())
	}
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %q, want %q", got, want)
	}
}

func sameBytes(t testing.TB, got, want []byte, what string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %x, want %x", what, got, want)
	}
}

// The fixtures: one resolved object per kind, as the API would hand it
// to Put, with a fresh id and the suite's subject as owner.

func provider(name string) *v1.Provider {
	return &v1.Provider{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", Discovery: v1.Discovery{Mode: v1.DiscoveryAuto}, Health: v1.Health{Mode: v1.HealthProbe}},
		Status:   v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, time.Now(), nil), Owner: subject, Warnings: []string{}},
	}
}

func model(name string, targets ...string) *v1.Model {
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ModelSpec{Fallback: v1.FallbackOnError},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, time.Now(), nil), Owner: subject, Source: v1.SourceDeclared, Warnings: []string{}},
	}
	for _, p := range targets {
		w := 100
		m.Spec.Targets = append(m.Spec.Targets, v1.Target{Provider: p, Model: name, Weight: &w})
	}
	return m
}

func key(name string) *v1.Key {
	return &v1.Key{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.KeySpec{Models: []string{"gpt-5"}},
		Status:   v1.KeyStatus{ID: v1.NewID(v1.PrefixKey, time.Now(), nil), Owner: subject, Prefix: "lux_ab12cd34", Warnings: []string{}, Selectors: []v1.SelectorStatus{{Selector: "gpt-5", Matched: []string{"gpt-5"}}}},
	}
}

func budget(name string) *v1.Budget {
	amount := v1.Money(10_000_000)
	hard := true
	return &v1.Budget{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.BudgetSpec{Amount: &amount, Currency: "USD", Window: v1.WindowMonth, Hard: &hard},
		Status:   v1.BudgetStatus{ID: v1.NewID(v1.PrefixBudget, time.Now(), nil), Owner: subject, Warnings: []string{}},
	}
}

// hash is a 64-character lower-case hex string built from a seed, the
// shape of a Key hash without a value behind it.
func hash(seed byte) string {
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hex[(int(seed)+i)%16]
	}
	return string(b)
}

// lapse is the TTL the lease and tunnel cases wait past.
const lapse = 60 * time.Millisecond
