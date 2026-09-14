// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestTunnelLossIsUnreachableWithinTheTTL is spec 013's liveness rule on
// the health holder, in every mode: a tunnelled Provider with no live
// registry row is Unreachable at once, without three failures, with
// status.tunnel Disconnected, every target out of selection, and
// provider.unreachable raised once; a live row is Connected with the
// row's members and the Provider leaves Unreachable, by a probe under
// probe and by the row alone otherwise, raising provider.healthy; a row
// that lapses past the TTL is the loss again; and under probe a live
// row over a runtime that stopped is still caught by the thresholds.
func TestTunnelLossIsUnreachableWithinTheTTL(t *testing.T) {
	for _, mode := range []v1.HealthMode{v1.HealthProbe, v1.HealthPassive, v1.HealthNone} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t)
			up := &stub{pages: map[string]string{"": openaiList("llama3.1")}}
			url := serveStub(t, up)
			p := h.provider(t, "laptop", v1.DialectOpenAI, "", func(p *v1.Provider) {
				p.Spec.Tunnel, p.Spec.Credential, p.Spec.Health.Mode = true, nil, mode
			})
			h.declare(t, "laptop/llama3.1", "laptop", "llama3.1")
			job := NewHealth(HealthOptions{
				Store: h.st, Clients: tunnelClients{url: url, inner: h.clients}, Credentials: noCredentials{}, Interval: time.Hour,
				TunnelTTL: 30 * time.Second, Holder: "a", Logger: h.logger, Now: h.clock,
			})
			job.acquire(t.Context())
			ctx := t.Context()

			// No row: Unreachable on the first tick, Disconnected, the
			// target out, one event.
			job.Tick(ctx)
			got := h.get(t, p.Status.ID)
			if got.Status.Health == nil || got.Status.Health.State != v1.HealthUnreachable || got.Status.Health.LastError != "no live tunnel session" || !got.Status.Health.Since.Equal(h.clock()) {
				t.Fatalf("no row: health = %+v", got.Status.Health)
			}
			if tun := got.Status.Tunnel; tun == nil || tun.State != v1.TunnelDisconnected || tun.Session != "" || !tun.Since.Equal(h.clock()) {
				t.Fatalf("no row: tunnel = %+v", tun)
			}
			if m := h.models(t, "")["laptop/llama3.1"]; *m.Status.Available || m.Status.Targets[0].Health != v1.HealthUnreachable {
				t.Fatalf("no row: Model = %+v", m.Status)
			}
			if job.View(p.Status.ID) != v1.HealthUnreachable {
				t.Fatalf("no row: view %s", job.View(p.Status.ID))
			}
			unreachable := h.events(t, eventProviderUnreachable)
			if len(unreachable) != 1 {
				t.Fatalf("%d provider.unreachable events after the first tick", len(unreachable))
			}
			var rec eventRecord
			if err := json.Unmarshal(unreachable[0].Payload, &rec); err != nil {
				t.Fatal(err)
			}
			if data, _ := rec.Data.(map[string]any); data["targets"] != float64(1) || data["lastError"] != "no live tunnel session" {
				t.Fatalf("event data %v", rec.Data)
			}
			if up.count() != 0 {
				t.Fatalf("%d probes with no row", up.count())
			}
			// A second tick with no row writes nothing new.
			job.Tick(ctx)
			if len(h.events(t, eventProviderUnreachable)) != 1 {
				t.Fatal("the loss was raised twice")
			}

			// A row: Connected with its members, and the Provider leaves
			// Unreachable.
			h.advance(time.Minute)
			connected := h.clock()
			row := store.Tunnel{ProviderID: p.Status.ID, Session: "tun_01J9ZK2P7Q8R9S0T1U2V3W4X64", Replica: "10.0.0.1:8081", Subject: subject, Agent: "lux/0.1.0"}
			if err := h.st.Tunnels().Register(ctx, row, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			job.Tick(ctx)
			got = h.get(t, p.Status.ID)
			if got.Status.Health.State != v1.HealthHealthy || got.Status.Health.LastError != "" {
				t.Fatalf("a row: health = %+v", got.Status.Health)
			}
			if tun := got.Status.Tunnel; tun == nil || tun.State != v1.TunnelConnected || tun.Session != row.Session || tun.Subject != subject || tun.Agent != "lux/0.1.0" || !tun.Since.Equal(connected) || !tun.LastHeartbeatAt.Equal(connected) {
				t.Fatalf("a row: tunnel = %+v", tun)
			}
			if m := h.models(t, "")["laptop/llama3.1"]; !*m.Status.Available {
				t.Fatalf("a row: Model = %+v", m.Status)
			}
			if want := map[v1.HealthMode]int{v1.HealthProbe: 1, v1.HealthPassive: 0, v1.HealthNone: 0}[mode]; up.count() != want {
				t.Fatalf("%d probes with a row under %s, want %d", up.count(), mode, want)
			}
			healthy := h.events(t, eventProviderHealthy)
			if len(healthy) != 1 {
				t.Fatalf("%d provider.healthy events", len(healthy))
			}
			if err := json.Unmarshal(healthy[0].Payload, &rec); err != nil {
				t.Fatal(err)
			}
			if data, _ := rec.Data.(map[string]any); data["wasUnreachableFor"] != "1m0s" {
				t.Fatalf("recovery data %v", rec.Data)
			}
			// A heartbeat moves lastHeartbeatAt; the state stands.
			h.advance(10 * time.Second)
			if _, err := h.st.Tunnels().Heartbeat(ctx, p.Status.ID, row.Session, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			job.Tick(ctx)
			if tun := h.get(t, p.Status.ID).Status.Tunnel; !tun.LastHeartbeatAt.Equal(h.clock()) || !tun.Since.Equal(connected) {
				t.Fatalf("after a heartbeat: tunnel = %+v", tun)
			}

			// The row lapses: Unreachable within the TTL, Disconnected, one
			// more event, and no probe is spent on it.
			probes := up.count()
			h.advance(31 * time.Second)
			job.Tick(ctx)
			got = h.get(t, p.Status.ID)
			if got.Status.Health.State != v1.HealthUnreachable || got.Status.Tunnel.State != v1.TunnelDisconnected || !got.Status.Tunnel.Since.Equal(h.clock()) {
				t.Fatalf("lapsed: health = %+v tunnel = %+v", got.Status.Health, got.Status.Tunnel)
			}
			if len(h.events(t, eventProviderUnreachable)) != 2 || up.count() != probes {
				t.Fatalf("lapsed: %d unreachable events, %d probes", len(h.events(t, eventProviderUnreachable)), up.count())
			}
			if *h.models(t, "")["laptop/llama3.1"].Status.Available {
				t.Fatal("lapsed: the target is still selectable")
			}

			// Under probe, the row's return alone does not bring the
			// Provider back: the probe does, so a dead runtime behind a
			// live row stays Unreachable, and one that answers and then
			// stops is caught by the thresholds while status.tunnel stays
			// Connected.
			if mode != v1.HealthProbe {
				return
			}
			if err := h.st.Tunnels().Register(ctx, row, 30*time.Second); err != nil {
				t.Fatal(err)
			}
			up.set(http.StatusInternalServerError, `{"error":"dead"}`)
			h.advance(time.Second)
			job.Tick(ctx)
			if got = h.get(t, p.Status.ID); got.Status.Health.State != v1.HealthUnreachable || got.Status.Tunnel.State != v1.TunnelConnected {
				t.Fatalf("a live row over a dead runtime: health = %+v tunnel = %+v", got.Status.Health, got.Status.Tunnel)
			}
			up.setPages(map[string]string{"": openaiList("llama3.1")})
			h.advance(time.Second)
			job.Tick(ctx)
			if got = h.get(t, p.Status.ID); got.Status.Health.State != v1.HealthHealthy {
				t.Fatalf("the runtime back: health = %+v", got.Status.Health)
			}
			up.set(http.StatusInternalServerError, `{"error":"dead"}`)
			for i, want := range []v1.HealthState{v1.HealthDegraded, v1.HealthDegraded, v1.HealthUnreachable} {
				h.advance(time.Second)
				job.Tick(ctx)
				got = h.get(t, p.Status.ID)
				if got.Status.Health.State != want || got.Status.Tunnel.State != v1.TunnelConnected {
					t.Fatalf("dead runtime tick %d: health = %+v tunnel = %+v", i, got.Status.Health, got.Status.Tunnel)
				}
			}
			if !strings.Contains(h.get(t, p.Status.ID).Status.Health.LastError, "upstream status 500") {
				t.Fatalf("lastError = %q", h.get(t, p.Status.ID).Status.Health.LastError)
			}
		})
	}
}

// TestTunnelObserveFoldsIntoHealth: a data plane outcome toward a
// tunnelled Provider under passive mode counts on the holder as for any
// Provider, so a runtime that fails through the tunnel degrades while
// its session lives.
func TestTunnelObserveFoldsIntoHealth(t *testing.T) {
	h := newHarness(t)
	p := h.provider(t, "laptop", v1.DialectOpenAI, "", func(p *v1.Provider) {
		p.Spec.Tunnel, p.Spec.Credential, p.Spec.Health.Mode = true, nil, v1.HealthPassive
	})
	if err := h.st.Tunnels().Register(t.Context(), store.Tunnel{ProviderID: p.Status.ID, Session: "tun_1", Subject: subject}, time.Minute); err != nil {
		t.Fatal(err)
	}
	job := NewHealth(HealthOptions{Store: h.st, Clients: h.clients, Credentials: noCredentials{}, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock})
	job.acquire(t.Context())
	job.Tick(t.Context())
	if got := h.get(t, p.Status.ID).Status.Health; got.State != v1.HealthHealthy {
		t.Fatalf("with a row: %+v", got)
	}
	job.Observe(p.Status.ID, true)
	if got := h.get(t, p.Status.ID).Status.Health; got.State != v1.HealthDegraded {
		t.Fatalf("after a failed outcome: %+v", got)
	}
	job.Observe(p.Status.ID, false)
	if got := h.get(t, p.Status.ID).Status.Health; got.State != v1.HealthHealthy {
		t.Fatalf("after a success: %+v", got)
	}
	// Without the default the TTL is thirty seconds.
	if job.o.TunnelTTL != defaultTunnelTTL {
		t.Errorf("TunnelTTL %s", job.o.TunnelTTL)
	}
}
