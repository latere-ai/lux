// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// outcome is one step of a health sequence: a probe answering with a
// status, or a passive observation.
type outcome struct {
	status  int  // the upstream's answer to a probe; 0 is a refused connection
	passive bool // fed through Observe instead of a probe
	failed  bool // the passive outcome
	want    v1.HealthState
}

// TestHealthTransitions: every transition in the state diagram fires at
// its threshold, in probe mode from probes and in passive mode from
// outcomes, a 4xx is a success while a 5xx is a failure, since moves on
// every change and lastProbeAt on every probe.
func TestHealthTransitions(t *testing.T) {
	probeSeq := []outcome{
		{status: 200, want: v1.HealthHealthy},
		{status: 503, want: v1.HealthDegraded},
		{status: 500, want: v1.HealthDegraded},
		{status: 0, want: v1.HealthUnreachable},
		{status: 502, want: v1.HealthUnreachable},
		{status: 404, want: v1.HealthHealthy},
		{status: 429, want: v1.HealthHealthy},
		{status: 500, want: v1.HealthDegraded},
		{status: 200, want: v1.HealthHealthy},
	}
	passiveSeq := []outcome{
		{passive: true, failed: true, want: v1.HealthDegraded},
		{passive: true, failed: true, want: v1.HealthDegraded},
		{passive: true, failed: true, want: v1.HealthUnreachable},
		{passive: true, failed: true, want: v1.HealthUnreachable},
		{passive: true, failed: false, want: v1.HealthHealthy},
		{passive: true, failed: true, want: v1.HealthDegraded},
		{passive: true, failed: false, want: v1.HealthHealthy},
	}
	for name, tc := range map[string]struct {
		mode v1.HealthMode
		seq  []outcome
	}{"probe": {v1.HealthProbe, probeSeq}, "passive": {v1.HealthPassive, passiveSeq}} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
			live := serveStub(t, up)
			p := h.provider(t, "openai", v1.DialectOpenAI, live, func(p *v1.Provider) { p.Spec.Health.Mode = tc.mode })
			h.declare(t, "gpt-5", "openai", "gpt-5")
			job := h.health("a")
			job.acquire(t.Context())
			job.Tick(t.Context())
			if tc.mode == v1.HealthPassive {
				if got := h.get(t, p.Status.ID).Status.Health; got != nil {
					t.Fatalf("a passive Provider was probed: %+v", got)
				}
			}
			// The first tick already probed a probe-mode Provider once.
			var lastSince time.Time
			prev := v1.HealthUnknown
			if got := h.get(t, p.Status.ID).Status.Health; got != nil {
				prev, lastSince = got.State, got.Since
			}
			for i, step := range tc.seq {
				h.advance(time.Second)
				if step.passive {
					job.Observe(p.Status.ID, step.failed)
				} else {
					if step.status == 0 {
						p.Spec.BaseURL = "http://127.0.0.1:1"
					} else {
						p.Spec.BaseURL = live
						up.set(step.status, `{"error":"x"}`)
					}
					if _, err := h.st.Objects().Put(t.Context(), p, p.Status.Version); err != nil {
						t.Fatal(err)
					}
					p = h.get(t, p.Status.ID)
					job.Tick(t.Context())
				}
				got := h.get(t, p.Status.ID).Status.Health
				if got == nil || got.State != step.want {
					t.Fatalf("step %d: state = %+v, want %s", i, got, step.want)
				}
				if step.want != prev && !got.Since.Equal(h.clock()) {
					t.Fatalf("step %d: since = %s, want the clock on a change", i, got.Since)
				}
				if step.want == prev && !got.Since.Equal(lastSince) {
					t.Fatalf("step %d: since moved without a change", i)
				}
				if !step.passive && !got.LastProbeAt.Equal(h.clock()) {
					t.Fatalf("step %d: lastProbeAt = %s", i, got.LastProbeAt)
				}
				if job.View(p.Status.ID) != step.want {
					t.Fatalf("step %d: the holder's view = %s", i, job.View(p.Status.ID))
				}
				m := h.models(t, "")["gpt-5"]
				if avail := *m.Status.Available; avail != (step.want != v1.HealthUnreachable) || m.Status.Targets[0].Health != step.want {
					t.Fatalf("step %d: Model status = %+v", i, m.Status)
				}
				lastSince, prev = got.Since, step.want
			}
			if strings.Contains(h.logged(), canary) {
				t.Fatal("the log carries the credential")
			}
		})
	}
}

// TestProbeReportsARefusedCredential: a 401 on the probe leaves the state
// Healthy and writes credential refused: 401 to lastError; the next 200
// clears it.
func TestProbeReportsARefusedCredential(t *testing.T) {
	h := newHarness(t)
	up := &stub{status: http.StatusUnauthorized, body: `{"error":{"message":"bad key"}}`}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	job := h.health("a")
	job.acquire(t.Context())
	job.Tick(t.Context())
	got := h.get(t, p.Status.ID).Status.Health
	if got == nil || got.State != v1.HealthHealthy || got.LastError != "credential refused: 401" {
		t.Fatalf("health = %+v", got)
	}
	up.set(http.StatusForbidden, "")
	job.Tick(t.Context())
	if got := h.get(t, p.Status.ID).Status.Health; got.LastError != "credential refused: 403" || got.State != v1.HealthHealthy {
		t.Fatalf("health = %+v", got)
	}
	up.setPages(map[string]string{"": openaiList("gpt-5")})
	job.Tick(t.Context())
	if got := h.get(t, p.Status.ID).Status.Health; got.LastError != "" || got.State != v1.HealthHealthy {
		t.Fatalf("health = %+v", got)
	}
	req := up.all()[0]
	if req.URL.Path != "/models" || req.Header.Get("Authorization") != "Bearer "+canary {
		t.Fatalf("probe = %s %v", req.URL, req.Header)
	}
	up.set(http.StatusBadGateway, strings.Repeat("x", 4000))
	job.Tick(t.Context())
	if got := h.get(t, p.Status.ID).Status.Health; got.State != v1.HealthDegraded || !strings.Contains(got.LastError, "upstream status 502") || len(got.LastError) > 1200 {
		t.Fatalf("health = %+v", got)
	}
}

// TestLocalHealthOnlyDowngrades: a replica that does not hold the lease
// downgrades its own view below the published state on its failures,
// never raises it above, and publishes nothing.
func TestLocalHealthOnlyDowngrades(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	holder, other := h.health("holder"), h.health("other")
	holder.acquire(t.Context())
	other.acquire(t.Context())
	holder.Tick(t.Context())
	other.Tick(t.Context())
	if !holder.Held() || other.Held() {
		t.Fatal("the lease is not with one replica")
	}
	if other.View(p.Status.ID) != v1.HealthHealthy {
		t.Fatalf("the other replica's view = %s, want the published Healthy", other.View(p.Status.ID))
	}
	other.Observe(p.Status.ID, true)
	if other.View(p.Status.ID) != v1.HealthDegraded || h.get(t, p.Status.ID).Status.Health.State != v1.HealthHealthy {
		t.Fatalf("view = %s, published = %s", other.View(p.Status.ID), h.get(t, p.Status.ID).Status.Health.State)
	}
	other.Observe(p.Status.ID, true)
	other.Observe(p.Status.ID, true)
	if other.View(p.Status.ID) != v1.HealthUnreachable {
		t.Fatalf("view after three failures = %s", other.View(p.Status.ID))
	}
	if n := len(h.events(t, eventProviderUnreachable)); n != 0 {
		t.Fatalf("a non-holder raised %d events", n)
	}
	// The published state falls to Unreachable; the other replica's
	// successes do not raise its view above it.
	up.set(http.StatusInternalServerError, "")
	for range 3 {
		holder.Tick(t.Context())
	}
	other.Tick(t.Context())
	other.Observe(p.Status.ID, false)
	if other.View(p.Status.ID) != v1.HealthUnreachable || other.local[p.Status.ID].state != v1.HealthHealthy {
		t.Fatalf("view = %s with a Healthy local view over a published Unreachable", other.View(p.Status.ID))
	}
	if other.View("prv_unknown") != v1.HealthUnknown {
		t.Fatal("an unknown Provider has a view")
	}
	// Observe on the holder for a Provider it has not read, or with mode
	// none, moves the local view alone.
	quiet := h.provider(t, "quiet", v1.DialectOpenAI, "http://127.0.0.1:1", func(p *v1.Provider) { p.Spec.Health.Mode = v1.HealthNone })
	holder.Observe("prv_new", true)
	holder.Tick(t.Context())
	holder.Observe(quiet.Status.ID, true)
	if got := h.get(t, quiet.Status.ID).Status.Health; got != nil {
		t.Fatalf("a none-mode Provider was published: %+v", got)
	}
	if holder.View(quiet.Status.ID) != v1.HealthDegraded {
		t.Fatal("the holder's own view of a none-mode Provider did not move")
	}
}

// TestUnreachableLeavesSelection is the status half of the row: an
// Unreachable Provider's targets make a single-target Model unavailable
// and a two-target Model available through the other, Degraded and
// Unknown take nothing out, and health.mode none is Unknown forever.
func TestUnreachableLeavesSelection(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	live := serveStub(t, up)
	down := h.provider(t, "down", v1.DialectOpenAI, "http://127.0.0.1:1", nil)
	shaky := h.provider(t, "shaky", v1.DialectOpenAI, live, nil)
	quiet := h.provider(t, "quiet", v1.DialectOpenAI, live, func(p *v1.Provider) { p.Spec.Health.Mode = v1.HealthNone })
	h.declare(t, "only-down", "down", "gpt-5")
	both := h.declare(t, "both", "down", "gpt-5")
	w := 100
	both.Spec.Targets = append(both.Spec.Targets, v1.Target{Provider: shaky.Status.ID, Model: "gpt-5", Weight: &w})
	if _, err := h.st.Objects().Put(t.Context(), both, both.Status.Version); err != nil {
		t.Fatal(err)
	}
	h.declare(t, "quiet-only", "quiet", "gpt-5")
	job := h.health("a")
	job.acquire(t.Context())
	for range 3 {
		job.Tick(t.Context())
	}
	if h.get(t, down.Status.ID).Status.Health.State != v1.HealthUnreachable {
		t.Fatalf("down = %+v", h.get(t, down.Status.ID).Status.Health)
	}
	ms := h.models(t, "")
	if *ms["only-down"].Status.Available || ms["only-down"].Status.Targets[0].Health != v1.HealthUnreachable {
		t.Fatalf("only-down = %+v", ms["only-down"].Status)
	}
	if !*ms["both"].Status.Available || ms["both"].Status.Targets[1].Health != v1.HealthHealthy || ms["both"].Status.Targets[1].Provider != shaky.Status.ID {
		t.Fatalf("both = %+v", ms["both"].Status)
	}
	if !*ms["quiet-only"].Status.Available || ms["quiet-only"].Status.Targets[0].Health != v1.HealthUnknown {
		t.Fatalf("quiet-only = %+v", ms["quiet-only"].Status)
	}
	if got := h.get(t, quiet.Status.ID).Status.Health; got != nil {
		t.Fatalf("a none-mode Provider was probed: %+v", got)
	}
	up.set(http.StatusInternalServerError, "")
	job.Tick(t.Context())
	if h.get(t, shaky.Status.ID).Status.Health.State != v1.HealthDegraded || !*h.models(t, "")["both"].Status.Available {
		t.Fatal("Degraded took a target out")
	}
	// The unreachable event names how many targets left selection, and
	// the recovery names how long it lasted.
	events := h.events(t, eventProviderUnreachable)
	if len(events) != 1 {
		t.Fatalf("%d provider.unreachable events, want 1", len(events))
	}
	var rec eventRecord
	if err := json.Unmarshal(events[0].Payload, &rec); err != nil {
		t.Fatal(err)
	}
	data, _ := rec.Data.(map[string]any)
	if rec.Reason != reasonProbe || rec.Object.ID != down.Status.ID || rec.Object.Name != "down" || data["targets"] != float64(2) || !strings.Contains(data["lastError"].(string), "connection refused") {
		t.Fatalf("event = %+v", rec)
	}
	h.advance(time.Minute)
	down.Spec.BaseURL = live
	up.setPages(map[string]string{"": openaiList("gpt-5")})
	if _, err := h.st.Objects().Put(t.Context(), down, down.Status.Version); err != nil {
		t.Fatal(err)
	}
	job.Tick(t.Context())
	var recovered *eventRecord
	for _, e := range h.events(t, eventProviderHealthy) {
		var rec eventRecord
		if err := json.Unmarshal(e.Payload, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Object.ID == down.Status.ID && rec.Data.(map[string]any)["wasUnreachableFor"] != nil {
			recovered = &rec
		}
	}
	if recovered == nil || recovered.Data.(map[string]any)["wasUnreachableFor"] != "1m0s" {
		t.Fatalf("no provider.healthy for the recovery in %d events", len(h.events(t, eventProviderHealthy)))
	}
	if !*h.models(t, "")["only-down"].Status.Available {
		t.Fatal("the recovered Provider's Model is still unavailable")
	}
	// A Provider turned to mode none is reset to Unknown once.
	down.Spec.Health.Mode = v1.HealthNone
	down = h.get(t, down.Status.ID)
	down.Spec.Health.Mode = v1.HealthNone
	if _, err := h.st.Objects().Put(t.Context(), down, down.Status.Version); err != nil {
		t.Fatal(err)
	}
	job.Tick(t.Context())
	if got := h.get(t, down.Status.ID).Status.Health; got.State != v1.HealthUnknown || got.LastProbeAt.IsZero() {
		t.Fatalf("after mode none = %+v", got)
	}
	job.Tick(t.Context())
	if got := h.get(t, down.Status.ID).Status.Health; !got.Since.Equal(h.clock()) {
		t.Fatalf("Unknown was written twice: %+v", got)
	}
}

// TestHealthRunLoop: Run takes the lease, probes at start, fills the
// status of a Model declared later on its tick, and releases the lease
// on stop.
func TestHealthRunLoop(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	job := NewHealth(HealthOptions{Store: h.st, Clients: h.clients, Credentials: h.creds, Interval: 20 * time.Millisecond, Holder: "a", Logger: h.logger, Now: h.clock})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		job.Run(ctx)
	}()
	waitUntil(t, "the first probe", func() bool { return up.count() >= 1 })
	m := h.declare(t, "gpt-5", "openai", "gpt-5")
	waitUntil(t, "the Model's status", func() bool {
		got := h.models(t, "")["gpt-5"]
		return got != nil && got.Status.Available != nil
	})
	if got := h.models(t, "")["gpt-5"]; !*got.Status.Available || got.Status.Targets[0].Health != v1.HealthHealthy || got.Status.ID != m.Status.ID {
		t.Fatalf("Model status = %+v", got.Status)
	}
	if !job.Held() {
		t.Fatal("Run did not take the lease")
	}
	cancel()
	<-done
	if held, err := h.st.Leases().Acquire(t.Context(), store.LeaseHealth, "b", store.LeaseTTL); err != nil || !held {
		t.Fatalf("the lease was not released: %v, %v", held, err)
	}
	_ = p
	if strings.Contains(h.logged(), canary) {
		t.Fatal("the log carries the credential")
	}
}

// TestHealthStoreFailures: an ended context is logged at every read and
// write and changes nothing.
func TestHealthStoreFailures(t *testing.T) {
	h := newHarness(t)
	p := h.provider(t, "openai", v1.DialectOpenAI, "http://127.0.0.1:1", nil)
	job := h.health("a")
	job.acquire(t.Context())
	job.Tick(t.Context())
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	job.acquire(ended)
	if job.Held() {
		t.Fatal("a failed acquire kept the lease")
	}
	job.acquire(t.Context())
	job.Tick(ended)
	job.mu.Lock()
	job.record(ended, p, true, "x", true)
	job.resetToUnknown(ended, h.provider(t, "quiet", v1.DialectOpenAI, "http://127.0.0.1:1", func(p *v1.Provider) {
		p.Spec.Health.Mode = v1.HealthNone
		p.Status.Health = &v1.HealthStatus{State: v1.HealthHealthy}
	}))
	job.mu.Unlock()
	for _, want := range []string{"health: acquiring the lease", "health: reading the Providers", "health: writing status.health"} {
		if !strings.Contains(h.logged(), want) {
			t.Errorf("log lacks %q:\n%s", want, h.logged())
		}
	}
	// A Provider whose baseURL cannot be a client is a failed probe.
	bad := h.provider(t, "bad", v1.DialectOpenAI, "://nope", nil)
	failed, lastError := job.probe(t.Context(), bad)
	if !failed || lastError == "" {
		t.Fatalf("probe of a bad baseURL = %v, %q", failed, lastError)
	}
	job.release(t.Context())
	job.release(t.Context())
	if held, err := h.st.Leases().Acquire(t.Context(), store.LeaseHealth, "b", store.LeaseTTL); err != nil || !held {
		t.Fatalf("the lease was not released: %v, %v", held, err)
	}
}

// TestMachineAndWorse hold the state machine and the order to the
// diagram directly.
func TestMachineAndWorse(t *testing.T) {
	var m machine
	steps := []struct {
		failed  bool
		want    v1.HealthState
		changed bool
	}{
		{true, v1.HealthDegraded, true}, {true, v1.HealthDegraded, false}, {true, v1.HealthUnreachable, true},
		{true, v1.HealthUnreachable, false}, {false, v1.HealthHealthy, true}, {false, v1.HealthHealthy, false},
		{true, v1.HealthDegraded, true}, {false, v1.HealthHealthy, true},
	}
	for i, s := range steps {
		if changed := m.step(s.failed); m.state != s.want || changed != s.changed {
			t.Fatalf("step %d: %s, %v; want %s, %v", i, m.state, changed, s.want, s.changed)
		}
	}
	for _, tc := range []struct{ a, b, want v1.HealthState }{
		{v1.HealthUnknown, v1.HealthUnknown, v1.HealthUnknown},
		{"", "", v1.HealthUnknown},
		{v1.HealthHealthy, v1.HealthUnknown, v1.HealthHealthy},
		{v1.HealthUnknown, v1.HealthDegraded, v1.HealthDegraded},
		{v1.HealthHealthy, v1.HealthDegraded, v1.HealthDegraded},
		{v1.HealthUnreachable, v1.HealthHealthy, v1.HealthUnreachable},
		{v1.HealthDegraded, v1.HealthUnreachable, v1.HealthUnreachable},
	} {
		if got := worse(tc.a, tc.b); got != tc.want {
			t.Errorf("worse(%s, %s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
	}
	empty := observedModel(&v1.Model{}, func(string) v1.HealthState { return "" })
	if *empty.Available || len(empty.Targets) != 0 {
		t.Fatalf("a Model with no targets = %+v", empty)
	}
	if defaultHolder() == "" {
		t.Fatal("no default holder")
	}
}
