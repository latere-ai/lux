// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// discovered stores a discovered Model with one target on the Provider,
// under the name discovery gives it.
func (h *harness) discovered(t *testing.T, name, provider, upstream string) *v1.Model {
	t.Helper()
	m := h.declare(t, name, provider, upstream)
	m.Status.Source = v1.SourceDiscovered
	if _, err := h.st.Objects().Put(t.Context(), m, m.Status.Version); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestModelResolution: an exact name resolves, declared or discovered; a
// discovered <provider>/<upstream> name is looked up whole and never
// split, however many slashes the upstream name carries; an unknown
// name, a name differing in case or in whitespace, a mdl_ id, and an
// empty name resolve to nothing.
func TestModelResolution(t *testing.T) {
	h := newHarness(t)
	h.provider(t, "openai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	declared := h.declare(t, "gpt-5", "openai", "gpt-5")
	h.discovered(t, "openai/meta/llama-3", "openai", "meta/llama-3")
	c := &Catalog{Objects: h.st.Objects()}
	cases := []struct {
		name   string
		want   string // the resolved Model's name, "" for nothing
		source v1.Source
	}{
		{"gpt-5", "gpt-5", v1.SourceDeclared},
		{"openai/meta/llama-3", "openai/meta/llama-3", v1.SourceDiscovered},
		{"openai/meta", "", ""},
		{"meta/llama-3", "", ""},
		{"llama-3", "", ""},
		{"GPT-5", "", ""},
		{" gpt-5", "", ""},
		{"gpt-5 ", "", ""},
		{"nope", "", ""},
		{declared.Status.ID, "", ""},
		{"mdl_", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := c.Model(t.Context(), tc.name)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == "" && m != nil:
				t.Fatalf("resolved to %s", m.Metadata.Name)
			case tc.want != "" && (m == nil || m.Metadata.Name != tc.want || m.Status.Source != tc.source):
				t.Fatalf("resolved to %+v, want %s (%s)", m, tc.want, tc.source)
			}
		})
	}
}

// TestCatalogProviders: a Provider is found by name and by prv_ id, an
// unknown name, an unknown id, and an empty reference are nothing, and
// what comes back carries no credential value in any encoding.
func TestCatalogProviders(t *testing.T) {
	h := newHarness(t)
	p := h.provider(t, "openai", v1.DialectOpenAI, "https://api.example.com/v1", func(p *v1.Provider) {
		p.Spec.Credential.SetValue(canary)
	})
	c := &Catalog{Objects: h.st.Objects()}
	for _, ref := range []string{"openai", p.Status.ID} {
		got, err := c.Provider(t.Context(), ref)
		if err != nil || got == nil || got.Status.ID != p.Status.ID || got.Metadata.Name != "openai" {
			t.Fatalf("Provider(%q) = %+v, %v", ref, got, err)
		}
		if v, ok := got.Spec.Credential.Value(); ok || v != "" {
			t.Fatalf("Provider(%q) carries a credential value", ref)
		}
		enc, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(enc), canary) {
			t.Fatalf("Provider(%q) encodes the credential value", ref)
		}
		if got.Status.Credential == nil || !got.Status.Credential.Set {
			t.Fatalf("Provider(%q) status.credential = %+v", ref, got.Status.Credential)
		}
	}
	for _, ref := range []string{"anthropic", "prv_" + strings.Repeat("0", 26), "", "mdl_" + strings.Repeat("0", 26)} {
		if got, err := c.Provider(t.Context(), ref); err != nil || got != nil {
			t.Fatalf("Provider(%q) = %+v, %v", ref, got, err)
		}
	}
}

// TestCatalogModels: the list is every live Model by name ascending,
// declared and discovered alike, and empty when there is none.
func TestCatalogModels(t *testing.T) {
	h := newHarness(t)
	c := &Catalog{Objects: h.st.Objects()}
	if got, err := c.Models(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("empty catalog: %v, %v", got, err)
	}
	h.provider(t, "openai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	h.declare(t, "zeta", "openai", "zeta")
	h.discovered(t, "openai/alpha", "openai", "alpha")
	h.declare(t, "beta", "openai", "beta")
	got, err := c.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got3 []string
	for _, m := range got {
		got3 = append(got3, m.Metadata.Name)
	}
	if strings.Join(got3, " ") != "beta openai/alpha zeta" {
		t.Fatalf("list %v", got3)
	}
}

// TestCatalogStoreFailures: a store that cannot answer is the catalog's
// error, never nil, so the door answers store_unavailable rather than
// model_not_found.
func TestCatalogStoreFailures(t *testing.T) {
	h := newHarness(t)
	p := h.provider(t, "openai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	h.declare(t, "gpt-5", "openai", "gpt-5")
	b := &broken{Store: h.st, fail: map[string]bool{"Objects.ByName": true, "Objects.Get": true, "Objects.List": true}}
	c := &Catalog{Objects: b.Objects()}
	if _, err := c.Model(t.Context(), "gpt-5"); !errors.Is(err, errBroken) {
		t.Errorf("Model: %v", err)
	}
	if _, err := c.Models(t.Context()); !errors.Is(err, errBroken) {
		t.Errorf("Models: %v", err)
	}
	if _, err := c.Provider(t.Context(), "openai"); !errors.Is(err, errBroken) {
		t.Errorf("Provider by name: %v", err)
	}
	if _, err := c.Provider(t.Context(), p.Status.ID); !errors.Is(err, errBroken) {
		t.Errorf("Provider by id: %v", err)
	}
}

// wrongKind is a store whose rows come back as another kind, which no
// implementation of spec 010 does; the catalog refuses the row rather
// than serving it.
type wrongKind struct{ store.Objects }

func (wrongKind) ByName(context.Context, string, string) (v1.Object, int64, error) {
	return &v1.Key{}, 1, nil
}

func (wrongKind) Get(context.Context, string, string) (v1.Object, int64, error) {
	return &v1.Key{}, 1, nil
}

func (wrongKind) List(context.Context, string, store.Filter, store.Page) ([]v1.Object, string, error) {
	return []v1.Object{&v1.Key{}}, "", nil
}

func TestCatalogRefusesAnotherKind(t *testing.T) {
	c := &Catalog{Objects: wrongKind{}}
	if _, err := c.Model(t.Context(), "gpt-5"); err == nil || !strings.Contains(err.Error(), "*v1.Key") {
		t.Errorf("Model: %v", err)
	}
	if _, err := c.Models(t.Context()); err == nil || !strings.Contains(err.Error(), "*v1.Key") {
		t.Errorf("Models: %v", err)
	}
	if _, err := c.Provider(t.Context(), "openai"); err == nil || !strings.Contains(err.Error(), "*v1.Key") {
		t.Errorf("Provider by name: %v", err)
	}
	if _, err := c.Provider(t.Context(), "prv_"+strings.Repeat("0", 26)); err == nil || !strings.Contains(err.Error(), "*v1.Key") {
		t.Errorf("Provider by id: %v", err)
	}
}

// TestModelStatusFollowsHealth: status.available and
// status.targets[].health follow the Providers' published health as
// the health lease holder writes them, and the catalog reads them back:
// one target Unreachable leaves the Model available with that target
// marked, every target Unreachable makes it unavailable, and a recovery
// makes it available again.
func TestModelStatusFollowsHealth(t *testing.T) {
	h := newHarness(t)
	upA, upB := &stub{pages: map[string]string{"": openaiList("gpt-5")}}, &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	liveA, liveB := serveStub(t, upA), serveStub(t, upB)
	a := h.provider(t, "a", v1.DialectOpenAI, liveA, nil)
	b := h.provider(t, "b", v1.DialectOpenAI, liveB, nil)
	m := h.declare(t, "m", "a", "gpt-5")
	w := 100
	m.Spec.Targets = append(m.Spec.Targets, v1.Target{Provider: b.Status.ID, Model: "gpt-5", Weight: &w})
	if _, err := h.st.Objects().Put(t.Context(), m, m.Status.Version); err != nil {
		t.Fatal(err)
	}
	c := &Catalog{Objects: h.st.Objects()}
	job := h.health("holder")
	job.acquire(t.Context())
	readdress := func(p *v1.Provider, baseURL string) *v1.Provider {
		p.Spec.BaseURL = baseURL
		if _, err := h.st.Objects().Put(t.Context(), p, p.Status.Version); err != nil {
			t.Fatal(err)
		}
		return h.get(t, p.Status.ID)
	}
	status := func() (bool, []v1.HealthState) {
		got, err := c.Model(t.Context(), "m")
		if err != nil || got == nil || got.Status.Available == nil || len(got.Status.Targets) != 2 {
			t.Fatalf("Model m: %+v, %v", got, err)
		}
		return *got.Status.Available, []v1.HealthState{got.Status.Targets[0].Health, got.Status.Targets[1].Health}
	}
	steps := []struct {
		name          string
		act           func()
		wantAvailable bool
		wantHealth    []v1.HealthState
	}{
		{"both healthy", func() { job.Tick(t.Context()) }, true, []v1.HealthState{v1.HealthHealthy, v1.HealthHealthy}},
		{"a unreachable", func() {
			a = readdress(a, "http://127.0.0.1:1")
			for range unreachableAfter {
				h.advance(time.Second)
				job.Tick(t.Context())
			}
		}, true, []v1.HealthState{v1.HealthUnreachable, v1.HealthHealthy}},
		{"both unreachable", func() {
			b = readdress(b, "http://127.0.0.1:1")
			for range unreachableAfter {
				h.advance(time.Second)
				job.Tick(t.Context())
			}
		}, false, []v1.HealthState{v1.HealthUnreachable, v1.HealthUnreachable}},
		{"a recovers", func() {
			a = readdress(a, liveA)
			h.advance(time.Second)
			job.Tick(t.Context())
		}, true, []v1.HealthState{v1.HealthHealthy, v1.HealthUnreachable}},
	}
	for _, s := range steps {
		s.act()
		available, health := status()
		if available != s.wantAvailable || health[0] != s.wantHealth[0] || health[1] != s.wantHealth[1] {
			t.Fatalf("%s: available %v, targets %v; want %v, %v", s.name, available, health, s.wantAvailable, s.wantHealth)
		}
	}
	// The status names the targets as the manifest wrote them, by name
	// and by id alike.
	got, _ := c.Model(t.Context(), "m")
	if got.Status.Targets[0].Provider != "a" || got.Status.Targets[1].Provider != b.Status.ID || got.Status.Targets[0].Model != "gpt-5" {
		t.Fatalf("targets %+v", got.Status.Targets)
	}
}
