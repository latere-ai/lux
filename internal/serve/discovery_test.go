// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestDiscoveryAddsAndRemoves: a successful list adds new discovered
// Models, removes those the upstream dropped, applies include before
// exclude, leaves an unchanged Model at its version, and writes
// status.discovered.
func TestDiscoveryAddsAndRemoves(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5", "gpt-5-mini", "o3", "dall-e-3", "gpt-4o-audio", "o4")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), func(p *v1.Provider) {
		p.Spec.Discovery.Include = []string{"gpt-*", "o3"}
		p.Spec.Discovery.Exclude = []string{"*-audio"}
	})
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	got := h.models(t, string(v1.SourceDiscovered))
	if names(got) != "openai/gpt-5 openai/gpt-5-mini openai/o3" {
		t.Fatalf("discovered = %q", names(got))
	}
	version := got["openai/gpt-5"].Status.Version
	if version != 1 {
		t.Fatalf("version = %d", version)
	}
	if s := h.get(t, p.Status.ID).Status.Discovered; s == nil || s.Count != 3 || !s.At.Equal(h.clock()) || len(s.Warnings) != 0 {
		t.Fatalf("status.discovered = %+v", s)
	}
	if n := len(h.events(t, eventModelDiscovered)); n != 3 {
		t.Fatalf("%d model.discovered events, want 3", n)
	}

	up.setPages(map[string]string{"": openaiList("gpt-5", "gpt-6", "o3", "o4")})
	h.advance(time.Minute)
	d.Tick(t.Context())
	got = h.models(t, string(v1.SourceDiscovered))
	if names(got) != "openai/gpt-5 openai/gpt-6 openai/o3" {
		t.Fatalf("after the second list = %q", names(got))
	}
	if got["openai/gpt-5"].Status.Version != version {
		t.Fatalf("an unchanged Model moved from version %d to %d", version, got["openai/gpt-5"].Status.Version)
	}
	if n := len(h.events(t, eventModelRemoved)); n != 1 {
		t.Fatalf("%d model.removed events, want 1", n)
	}
	var rec eventRecord
	if err := json.Unmarshal(h.events(t, eventModelRemoved)[0].Payload, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Reason != reasonDiscovery || rec.Object.Kind != v1.KindModel || rec.Subject != "" || !strings.HasPrefix(rec.ID, "evt_") {
		t.Fatalf("event = %+v", rec)
	}
	if !strings.Contains(h.logged(), "discovery: listed") || strings.Contains(h.logged(), canary) {
		t.Fatalf("log:\n%s", h.logged())
	}
}

// TestDiscoveredModelShape: a discovered Model has one target on the
// Provider, weight 100, priority 0, fallback never, no pricing, the
// default modalities, no contextWindow and no maxOutputTokens, the
// Provider's owner, source discovered, and the models route was reached
// with the Provider's credential.
func TestDiscoveredModelShape(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up)+"/v1", nil)
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	m := h.models(t, "")["openai/gpt-5"]
	if m == nil {
		t.Fatal("no Model openai/gpt-5")
	}
	if len(m.Spec.Targets) != 1 || m.Spec.Targets[0].Provider != "openai" || m.Spec.Targets[0].Model != "gpt-5" || *m.Spec.Targets[0].Weight != 100 || m.Spec.Targets[0].Priority != 0 {
		t.Fatalf("targets = %+v", m.Spec.Targets)
	}
	if m.Spec.Fallback != v1.FallbackNever || m.Spec.Pricing != nil || m.Spec.ContextWindow != 0 || m.Spec.MaxOutputTokens != 0 {
		t.Fatalf("spec = %+v", m.Spec)
	}
	if fmt.Sprint(m.Spec.Modalities) != "{[text] [text]}" {
		t.Fatalf("modalities = %v", m.Spec.Modalities)
	}
	if m.Status.Owner != subject || m.Status.Source != v1.SourceDiscovered || !strings.HasPrefix(m.Status.ID, "mdl_") {
		t.Fatalf("status = %+v", m.Status)
	}
	if m.Status.Available == nil || !*m.Status.Available || len(m.Status.Targets) != 1 || m.Status.Targets[0].Health != v1.HealthUnknown {
		t.Fatalf("observed status = %+v", m.Status)
	}
	req := up.all()[0]
	if req.URL.Path != "/v1/models" || req.Header.Get("Authorization") != "Bearer "+canary || req.Header.Get("User-Agent") != "luxd/test" {
		t.Fatalf("request = %s %s", req.URL, req.Header)
	}
	_ = p
}

// TestDiscoveryPaginates: an anthropic list of three pages by has_more
// and a gemini list of three pages by nextPageToken are read whole, the
// gemini generation-methods filter applies, and a list past 20 pages
// is a failed list.
func TestDiscoveryPaginates(t *testing.T) {
	h := newHarness(t)
	anthropic := &stub{pages: map[string]string{
		"":   `{"data":[{"id":"claude-a"}],"has_more":true,"first_id":"claude-a","last_id":"claude-a"}`,
		"p1": `{"data":[{"id":"claude-b"}],"has_more":true,"last_id":"claude-b"}`,
		"p2": `{"data":[{"id":"claude-c"}],"has_more":false}`,
	}}
	anthropic.pages["claude-a"], anthropic.pages["claude-b"] = anthropic.pages["p1"], anthropic.pages["p2"]
	gemini := &stub{pages: map[string]string{
		"":   `{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent","countTokens"]},{"name":"models/aqa","supportedGenerationMethods":["generateAnswer"]}],"nextPageToken":"t1"}`,
		"t1": `{"models":[{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"t2"}`,
		"t2": `{"models":[{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent","embedContent"]}]}`,
	}}
	a := h.provider(t, "anthropic", v1.DialectAnthropic, serveStub(t, anthropic), nil)
	g := h.provider(t, "gemini", v1.DialectGemini, serveStub(t, gemini)+"/v1beta", nil)
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	got := h.models(t, string(v1.SourceDiscovered))
	if names(got) != "anthropic/claude-a anthropic/claude-b anthropic/claude-c gemini/gemini-2.5-flash gemini/gemini-2.5-pro gemini/text-embedding-004" {
		t.Fatalf("discovered = %q", names(got))
	}
	if out := got["gemini/text-embedding-004"].Spec.Modalities.Output; len(out) != 1 || out[0] != v1.ModalityEmbedding {
		t.Fatalf("embedding model modalities = %v", out)
	}
	if out := got["gemini/gemini-2.5-flash"].Spec.Modalities.Output; len(out) != 1 || out[0] != v1.ModalityText {
		t.Fatalf("a model that generates and embeds = %v", out)
	}
	var paths []string
	for _, r := range anthropic.all() {
		paths = append(paths, r.URL.RequestURI())
	}
	if strings.Join(paths, " ") != "/models?limit=1000 /models?after_id=claude-a&limit=1000 /models?after_id=claude-b&limit=1000" {
		t.Fatalf("anthropic pages = %q", paths)
	}
	paths = nil
	for _, r := range gemini.all() {
		paths = append(paths, r.URL.RequestURI())
	}
	if strings.Join(paths, " ") != "/v1beta/models?pageSize=1000 /v1beta/models?pageSize=1000&pageToken=t1 /v1beta/models?pageSize=1000&pageToken=t2" {
		t.Fatalf("gemini pages = %q", paths)
	}
	if h.get(t, a.Status.ID).Status.Discovered.Count != 3 || h.get(t, g.Status.ID).Status.Discovered.Count != 3 {
		t.Fatal("status.discovered.count is not the pages' total")
	}

	// A list that never ends is a failed list, and the catalog stands.
	endless := &stub{pages: map[string]string{}}
	endless.pages[""] = `{"data":[{"id":"m0"}],"has_more":true,"last_id":"m0"}`
	for i := range 30 {
		endless.pages[fmt.Sprintf("m%d", i)] = fmt.Sprintf(`{"data":[{"id":"m%d"}],"has_more":true,"last_id":"m%d"}`, i+1, i+1)
	}
	e := h.provider(t, "endless", v1.DialectAnthropic, serveStub(t, endless), nil)
	d.Tick(t.Context())
	if endless.count() != 20 {
		t.Fatalf("an endless list was followed for %d pages", endless.count())
	}
	if got := h.get(t, e.Status.ID); got.Status.Discovered != nil || got.Status.Health == nil || !strings.Contains(got.Status.Health.LastError, "did not end within 20 pages") {
		t.Fatalf("status = %+v", got.Status)
	}
	if n := len(h.models(t, string(v1.SourceDiscovered))); n != 6 {
		t.Fatalf("%d discovered Models after the failed list, want 6", n)
	}
}

// TestDiscoveryFollowsProviderChanges: the lease holder lists a Provider
// within one second of reading its provider.created, or a
// provider.updated naming spec.baseURL or spec.credential, from the
// journal, and not on an update elsewhere.
func TestDiscoveryFollowsProviderChanges(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	url := serveStub(t, up)
	d := h.discovery("a")
	run(t, d.Run)
	waitUntil(t, "the lease", d.Held)

	p := h.provider(t, "openai", v1.DialectOpenAI, url, nil)
	if up.count() != 0 {
		t.Fatal("a Provider was listed before its event")
	}
	journal := func(typ string, data any) {
		payload, _ := json.Marshal(map[string]any{"type": typ, "data": data})
		if _, err := h.st.Journal().Append(t.Context(), store.Event{ID: v1.NewID(v1.PrefixEvent, time.Now(), nil), ObjectID: p.Status.ID, Type: typ, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	journal(eventProviderCreated, map[string]string{"dialect": "openai", "baseURL": url})
	waitUntil(t, "the list after provider.created", func() bool { return up.count() == 1 })
	waitUntil(t, "the Provider's Models", func() bool { return len(h.models(t, string(v1.SourceDiscovered))) == 1 })

	journal(eventProviderUpdated, []string{"spec.headers"})
	journal("model.created", nil)
	time.Sleep(50 * time.Millisecond)
	if up.count() != 1 {
		t.Fatalf("an unrelated update listed the Provider: %d requests", up.count())
	}
	journal(eventProviderUpdated, []string{"spec.timeout", "spec.baseURL"})
	waitUntil(t, "the list after a baseURL change", func() bool { return up.count() == 2 })
	journal(eventProviderUpdated, map[string]any{"paths": []string{"spec.credential.value"}})
	waitUntil(t, "the list after a credential change", func() bool { return up.count() == 3 })

	// A Provider the journal names and the store no longer holds is
	// skipped; one with discovery off is not listed.
	if _, err := h.st.Journal().Append(t.Context(), store.Event{ID: "evt_gone", ObjectID: "prv_gone", Type: eventProviderCreated}); err != nil {
		t.Fatal(err)
	}
	off := h.provider(t, "quiet", v1.DialectOpenAI, url, func(p *v1.Provider) { p.Spec.Discovery.Mode = v1.DiscoveryNone })
	if _, err := h.st.Journal().Append(t.Context(), store.Event{ID: "evt_off", ObjectID: off.Status.ID, Type: eventProviderCreated}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if up.count() != 3 {
		t.Fatalf("a deleted or discovery-off Provider was listed: %d requests", up.count())
	}
}

// TestRelist holds the journal filter to its rules, payload shape by
// payload shape.
func TestRelist(t *testing.T) {
	for name, tc := range map[string]struct {
		e    store.Event
		want bool
	}{
		"created":               {store.Event{Type: eventProviderCreated}, true},
		"deleted":               {store.Event{Type: "provider.deleted"}, false},
		"updated, no payload":   {store.Event{Type: eventProviderUpdated}, false},
		"updated, bad payload":  {store.Event{Type: eventProviderUpdated, Payload: []byte("{")}, false},
		"updated, no data":      {store.Event{Type: eventProviderUpdated, Payload: []byte(`{"type":"provider.updated"}`)}, false},
		"updated, odd data":     {store.Event{Type: eventProviderUpdated, Payload: []byte(`{"data":42}`)}, false},
		"updated, headers":      {store.Event{Type: eventProviderUpdated, Payload: []byte(`{"data":["spec.headers"]}`)}, false},
		"updated, baseURL":      {store.Event{Type: eventProviderUpdated, Payload: []byte(`{"data":["spec.baseURL"]}`)}, true},
		"updated, credential":   {store.Event{Type: eventProviderUpdated, Payload: []byte(`{"data":{"paths":["spec.credential.value"]}}`)}, true},
		"updated, object paths": {store.Event{Type: eventProviderUpdated, Payload: []byte(`{"data":{"paths":["spec.timeout"]}}`)}, false},
	} {
		if got := relist(tc.e); got != tc.want {
			t.Errorf("%s: relist = %v, want %v", name, got, tc.want)
		}
	}
}

// TestDiscoveryFailureKeepsTheCatalogue: a failed, empty, or unparseable
// list changes no object, leaves status.discovered.at as it was, and
// records the failure in status.health.lastError.
func TestDiscoveryFailureKeepsTheCatalogue(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5", "o3")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	before := h.models(t, "")
	at := h.get(t, p.Status.ID).Status.Discovered.At
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"a 500":              {http.StatusInternalServerError, `{"error":"down"}`, "upstream status 500"},
		"a 401":              {http.StatusUnauthorized, `{"error":"no"}`, "upstream status 401"},
		"an empty list":      {http.StatusOK, openaiList(), "the model list is empty"},
		"an unparseable one": {http.StatusOK, `<html>`, "not a models list"},
	} {
		t.Run(name, func(t *testing.T) {
			up.set(tc.status, tc.body)
			h.advance(time.Minute)
			d.Tick(t.Context())
			if got := names(h.models(t, "")); got != names(before) {
				t.Fatalf("the catalog changed: %q", got)
			}
			got := h.get(t, p.Status.ID)
			if !got.Status.Discovered.At.Equal(at) {
				t.Fatalf("status.discovered.at moved to %s", got.Status.Discovered.At)
			}
			if got.Status.Health == nil || !strings.Contains(got.Status.Health.LastError, tc.want) || !strings.HasPrefix(got.Status.Health.LastError, "model list: ") {
				t.Fatalf("lastError = %+v, want %q", got.Status.Health, tc.want)
			}
			if !strings.Contains(h.logged(), "the list failed, the catalog stands") {
				t.Fatalf("log:\n%s", h.logged())
			}
		})
	}
	// A transport failure the same way, and the health block's other
	// members are kept.
	if err := h.st.Objects().PutStatus(t.Context(), v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy, Since: h.clock()}}); err != nil {
		t.Fatal(err)
	}
	gone := h.provider(t, "gone", v1.DialectOpenAI, "http://127.0.0.1:1", nil)
	d.Tick(t.Context())
	if got := h.get(t, gone.Status.ID); got.Status.Health == nil || !strings.Contains(got.Status.Health.LastError, "connection refused") {
		t.Fatalf("a refused connection: %+v", got.Status.Health)
	}
	if got := h.get(t, p.Status.ID); got.Status.Health.State != v1.HealthHealthy {
		t.Fatalf("the health state was overwritten: %+v", got.Status.Health)
	}
}

// TestDeclaredModelSurvivesDiscovery: discovery never writes or deletes a
// Model whose source is declared; a declared Model shadows the
// discovered one, and deleting it lets the next run restore it.
func TestDeclaredModelSurvivesDiscovery(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5", "o3")}}
	h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	declaredBefore := h.declare(t, "openai/gpt-5", "openai", "gpt-5-2026")
	other := h.declare(t, "team/fast", "openai", "o3")
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	all := h.models(t, "")
	if names(all) != "openai/gpt-5 openai/o3 team/fast" {
		t.Fatalf("Models = %q", names(all))
	}
	if m := all["openai/gpt-5"]; m.Status.Source != v1.SourceDeclared || m.Status.Version != 1 || m.Spec.Targets[0].Model != "gpt-5-2026" {
		t.Fatalf("the declared Model was touched: %+v", m)
	}

	// The upstream drops o3: the declared team/fast on it is not deleted.
	up.setPages(map[string]string{"": openaiList("gpt-5")})
	d.Tick(t.Context())
	all = h.models(t, "")
	if names(all) != "openai/gpt-5 team/fast" || all["team/fast"].Status.ID != other.Status.ID {
		t.Fatalf("after the drop = %q", names(all))
	}

	// Deleting the declared Model lets the next run restore the
	// discovered one under a fresh id.
	if err := h.st.Objects().Delete(t.Context(), v1.KindModel, declaredBefore.Status.ID); err != nil {
		t.Fatal(err)
	}
	d.Tick(t.Context())
	restored := h.models(t, "")["openai/gpt-5"]
	if restored == nil || restored.Status.Source != v1.SourceDiscovered || restored.Status.ID == declaredBefore.Status.ID {
		t.Fatalf("restored = %+v", restored)
	}
}

// TestDiscoveryRunsOnOneReplica: two replicas with the lease contended
// run one list per interval between them, and the other takes over once
// the lease lapses.
func TestDiscoveryRunsOnOneReplica(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	a, b := h.discovery("a"), h.discovery("b")
	for i := range 5 {
		a.acquire(t.Context())
		b.acquire(t.Context())
		a.Tick(t.Context())
		b.Tick(t.Context())
		if up.count() != i+1 {
			t.Fatalf("interval %d: %d lists, want %d", i, up.count(), i+1)
		}
	}
	if !a.Held() || b.Held() {
		t.Fatal("the lease moved while its holder renewed")
	}
	h.advance(store.LeaseTTL + time.Second)
	b.acquire(t.Context())
	a.acquire(t.Context())
	if !b.Held() || a.Held() {
		t.Fatal("the lapsed lease did not move")
	}
	b.Tick(t.Context())
	a.Tick(t.Context())
	if up.count() != 6 {
		t.Fatalf("%d lists after the handover, want 6", up.count())
	}
	a.release(t.Context())
	b.release(t.Context())
	if _, err := h.st.Leases().Acquire(t.Context(), store.LeaseDiscovery, "c", store.LeaseTTL); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoveryWarnsOnRefusedNames: a name the schema refuses is not
// stored and is one warning in status.discovered naming the name and the
// rule; a name that appears twice is one Model.
func TestDiscoveryWarnsOnRefusedNames(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5", "GPT-5-Turbo", "gpt-5", "bad name")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	if got := names(h.models(t, "")); got != "openai/gpt-5" {
		t.Fatalf("Models = %q", got)
	}
	s := h.get(t, p.Status.ID).Status.Discovered
	if s == nil || s.Count != 1 || len(s.Warnings) != 2 {
		t.Fatalf("status.discovered = %+v", s)
	}
	for _, w := range s.Warnings {
		if !strings.HasPrefix(w, "upstream model ") || !strings.Contains(w, "invalid_field at metadata.name") {
			t.Errorf("warning = %q", w)
		}
	}
}

// TestDiscoverySkipsOffProvidersAndListsTunnels: a Provider with
// discovery.mode none is not listed on a tick; a tunneled one is listed
// through whatever client its source answers, which with spec 005's
// clients alone is a failed list that keeps the catalog and names the
// tunnel in status.health.lastError (spec 013's wiring composes the
// carrier client in front).
func TestDiscoverySkipsOffProvidersAndListsTunnels(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	url := serveStub(t, up)
	h.provider(t, "off", v1.DialectOpenAI, url, func(p *v1.Provider) { p.Spec.Discovery.Mode = v1.DiscoveryNone })
	tunnelled := h.provider(t, "tunnelled", v1.DialectOpenAI, "", func(p *v1.Provider) { p.Spec.Tunnel = true; p.Spec.Credential = nil })
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	if up.count() != 0 || len(h.models(t, "")) != 0 {
		t.Fatalf("%d lists, %d Models", up.count(), len(h.models(t, "")))
	}
	if got := h.get(t, tunnelled.Status.ID).Status.Health; got == nil || !strings.Contains(got.LastError, "model list") || !strings.Contains(got.LastError, "tunnelled Provider has no client") {
		t.Fatalf("the tunnelled Provider's failed list: %+v", got)
	}
	// A client source that answers the tunneled Provider lists it like
	// any other, with no credential injected.
	d = NewDiscovery(DiscoveryOptions{
		Store: h.st, Clients: tunnelClients{url: url, inner: h.clients}, Credentials: noCredentials{}, Interval: time.Hour,
		Holder: "a", Logger: h.logger, Now: h.clock, Jitter: func() float64 { return 0 },
	})
	d.acquire(t.Context())
	d.Tick(t.Context())
	if up.count() != 1 || names(h.models(t, "")) != "tunnelled/gpt-5" || up.lastHeader("Authorization") != "" {
		t.Fatalf("%d lists, Models %q, Authorization %q", up.count(), names(h.models(t, "")), up.lastHeader("Authorization"))
	}
	// Without the lease nothing happens either.
	b := h.discovery("b")
	b.acquire(t.Context())
	b.Tick(t.Context())
	b.Tail(t.Context())
	if b.Held() {
		t.Fatal("two holders")
	}
}

// TestDiscoveryJitterAndWait: the wait is the interval plus up to ten
// percent.
func TestDiscoveryJitterAndWait(t *testing.T) {
	d := NewDiscovery(DiscoveryOptions{Interval: time.Hour, Jitter: func() float64 { return 0.5 }})
	if got := d.wait(); got != time.Hour+3*time.Minute {
		t.Fatalf("wait = %s", got)
	}
	d = NewDiscovery(DiscoveryOptions{Interval: time.Hour})
	if got := d.wait(); got < time.Hour || got > time.Hour+6*time.Minute {
		t.Fatalf("wait with the default jitter = %s", got)
	}
	if d.o.Holder == "" || d.o.Tail != defaultTail || !strings.HasPrefix(d.o.NewID(v1.PrefixEvent), "evt_") {
		t.Fatalf("defaults = %+v", d.o)
	}
}

// TestDiscoveryStoreFailures: a store that cannot be read or written is
// logged and changes nothing.
func TestDiscoveryStoreFailures(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	d := h.discovery("a")
	d.acquire(t.Context())
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	d.Tick(ended)
	d.Tail(ended)
	d.List(ended, p)
	d.recordFailure(ended, p, fmt.Errorf("x"))
	if len(h.models(t, "")) != 0 {
		t.Fatal("an ended context wrote a Model")
	}
	for _, want := range []string{"discovery: reading the Providers", "discovery: reading the journal", "discovery: the list failed", "discovery: reading the Provider"} {
		if !strings.Contains(h.logged(), want) {
			t.Errorf("log lacks %q:\n%s", want, h.logged())
		}
	}
	_ = h.st.Close()
}

func TestDiscoveredModelInheritsAndRefreshesLabels(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), func(p *v1.Provider) { p.Metadata.Labels = map[string]string{"tenant": "a"} })
	d := h.discovery("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	first := h.models(t, "")["openai/gpt-5"]
	if first.Metadata.Labels["tenant"] != "a" {
		t.Fatal("model lost tenancy", first.Metadata)
	}
	p = h.get(t, p.Status.ID)
	p.Metadata.Labels["tenant"] = "b"
	if _, err := h.st.Objects().Put(t.Context(), p, p.Status.Version); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"data":{"paths":["metadata.labels.tenant"]}}`)
	if !relist(store.Event{Type: eventProviderUpdated, Payload: payload}) {
		t.Fatal("labels do not trigger rediscovery")
	}
	d.Tick(t.Context())
	second := h.models(t, "")["openai/gpt-5"]
	if second.Metadata.Labels["tenant"] != "b" || second.Status.Version <= first.Status.Version {
		t.Fatal("model labels not refreshed", second.Metadata, second.Status.Version)
	}
	if first.Metadata.Labels["tenant"] != "a" {
		t.Fatal("old model shares provider labels")
	}
}
