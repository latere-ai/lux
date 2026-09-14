// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

func TestCredentialForms(t *testing.T) {
	cases := []struct {
		name  string
		set   func(r *http.Request)
		want  string
		found bool
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer lux_abc") }, "lux_abc", true},
		{"bearer lower", func(r *http.Request) { r.Header.Set("Authorization", "bearer  spaced") }, " spaced", true},
		{"basic is not a bearer", func(r *http.Request) {
			r.Header.Set("Authorization", "Basic xyz")
			r.Header.Set("x-api-key", "k")
		}, "k", true},
		{"x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "k1") }, "k1", true},
		{"x-goog-api-key", func(r *http.Request) { r.Header.Set("x-goog-api-key", "g1") }, "g1", true},
		{"query", func(r *http.Request) { r.URL.RawQuery = "key=q1" }, "q1", true},
		{"bearer before x-api-key", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer b")
			r.Header.Set("x-api-key", "k")
		}, "b", true},
		{"x-api-key before goog", func(r *http.Request) {
			r.Header.Set("x-api-key", "k")
			r.Header.Set("x-goog-api-key", "g")
		}, "k", true},
		{"goog before query", func(r *http.Request) {
			r.Header.Set("x-goog-api-key", "g")
			r.URL.RawQuery = "key=q"
		}, "g", true},
		{"none", func(*http.Request) {}, "", false},
		{"empty bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer") }, "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)
		c.set(r)
		got, ok := credential(r)
		if got != c.want || ok != c.found {
			t.Errorf("%s: credential = %q, %v; want %q, %v", c.name, got, ok, c.want, c.found)
		}
	}
	// SHA-256("x"), the same function the API stores under.
	if hashValue("x") != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Errorf("hashValue(x) = %s", hashValue("x"))
	}
	if len(hashValue("lux_abc")) != 64 || strings.ToLower(hashValue("lux_abc")) != hashValue("lux_abc") {
		t.Error("hashValue is not 64 lower-case hex characters")
	}
	if hashValue(" x") == hashValue("x") {
		t.Error("hashValue trims")
	}
}

func TestKeyState(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	k := &v1.Key{}
	k.Status.ID = "key_1"
	if f := keyState(k, now); f != nil {
		t.Errorf("an active key is %v", f)
	}
	k.Spec.Disabled = true
	if f := keyState(k, now); f == nil || f.code != CodeKeyDisabled {
		t.Errorf("a disabled key is %v", f)
	}
	k.Spec.Disabled = false
	k.Status.ExpiresAt = now
	if f := keyState(k, now); f == nil || f.code != CodeKeyExpired {
		t.Errorf("a key expiring now is %v", f)
	}
	k.Status.ExpiresAt = now.Add(time.Second)
	if f := keyState(k, now); f != nil {
		t.Errorf("a key expiring later is %v", f)
	}
	k.Spec.Disabled = true
	if f := keyState(k, now.Add(time.Hour)); f.code != CodeKeyDisabled {
		t.Error("disabled does not precede expired")
	}
	k.Spec.Models = []string{"gpt-*", "exact"}
	for name, want := range map[string]bool{"gpt-5": true, "exact": true, "other": false, "gpt": false} {
		if allowed(k, name) != want {
			t.Errorf("allowed(%q) = %v", name, !want)
		}
	}
}

func TestRequestLabels(t *testing.T) {
	if requestLabels("") != nil {
		t.Error("an empty header yields labels")
	}
	got := requestLabels("team=a, run=r-1/2:x,bad key=v,=nov,k=,dup=1,dup=2,ok.k_1=v.v_1-2, e1=1,e2=2,e3=3,e4=4,e5=5,e6=6")
	want := map[string]string{"team": "a", "run": "r-1/2:x", "dup": "1", "ok.k_1": "v.v_1-2", "e1": "1", "e2": "2", "e3": "3", "e4": "4"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	long := requestLabels(strings.Repeat("k", 65) + "=v," + "k=" + strings.Repeat("v", 129) + ",k2=" + strings.Repeat("v", 128))
	if len(long) != 1 || long["k2"] == "" {
		t.Errorf("bounds: %v", long)
	}
	if requestLabels("a b=c,x:y=z") != nil {
		t.Error("a key outside the alphabet is kept")
	}
}
