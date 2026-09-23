// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestClientDelegatesADialedProvider: a Provider that is not tunneled
// gets spec 005's client, a nil Provider and a tunneled one without an
// id are refused, and Revoke reaches spec 005's clients too.
func TestClientDelegatesADialedProvider(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	dialed := &v1.Provider{Metadata: v1.ObjectMeta{Name: "openai"}, Spec: v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", Concurrency: 4}, Status: v1.ProviderStatus{ID: "prv_dialed"}}
	client, err := r.g.Client(t.Context(), dialed)
	if err != nil || client == nil {
		t.Fatalf("a dialed Provider: %v", err)
	}
	if _, ok := client.Transport.(*transport); ok {
		t.Fatal("a dialed Provider got the carrier transport")
	}
	if _, err := r.g.Client(t.Context(), nil); err == nil {
		t.Error("a nil Provider was answered")
	}
	if _, err := r.g.Client(t.Context(), &v1.Provider{Spec: v1.ProviderSpec{Tunnel: true}}); err == nil {
		t.Error("a tunneled Provider without an id was answered")
	}
	// Revoke of a Provider with no session changes nothing and panics on
	// nothing.
	r.g.Revoke("prv_dialed")
	r.g.Revoke("prv_none")
}

// TestSessionsGauge: an open and a closed session move
// lux_tunnel_sessions by one on this replica's registry.
func TestSessionsGauge(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	gauge := func() string {
		var buf bytes.Buffer
		r.reg.WritePrometheus(&buf)
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if strings.HasPrefix(line, MetricSessions+" ") || strings.HasPrefix(line, MetricSessions+"{") {
				return line
			}
		}
		return buf.String()
	}
	if got := gauge(); !strings.HasSuffix(got, " 0") {
		t.Fatalf("before: %q", got)
	}
	alice := r.mint("alice", time.Hour)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	if got := gauge(); !strings.HasSuffix(got, " 1") {
		t.Fatalf("with a session: %q", got)
	}
	stop()
	waitFor(t, func() bool { return r.g.Sessions() == 0 }, "the session to close")
	if got := gauge(); !strings.HasSuffix(got, " 0") {
		t.Fatalf("after: %q", got)
	}
	if r.g.TTL() != DefaultTTL {
		t.Errorf("TTL %s", r.g.TTL())
	}
}

// TestRegisterIdleReadsZero: a process with the tunnel off still
// exposes lux_tunnel_sessions, at zero, so spec 019's metric table
// holds whether or not the tunnel is on.
func TestRegisterIdleReadsZero(t *testing.T) {
	reg := metrics.NewRegistry()
	RegisterIdle(reg)
	var out strings.Builder
	reg.WritePrometheus(&out)
	if !strings.Contains(out.String(), MetricSessions+" 0") {
		t.Fatalf("registry lacks %s at zero:\n%s", MetricSessions, out.String())
	}
}

// TestTunnelDiscovery: discovery over the tunnel declares one Model per
// upstream name under the Provider's owner, once at connect through
// OnConnect and on the job's tick; a failed list, the agent gone, keeps
// the catalog and writes the failure to status.health.lastError.
func TestTunnelDiscovery(t *testing.T) {
	st := memory.New()
	var discovery *serve.Discovery
	listed := make(chan string, 8)
	r := newReplica(t, st, replicaOptions{connect: func(ctx context.Context, p *v1.Provider) {
		discovery.List(ctx, p)
		listed <- p.Metadata.Name
	}})
	discovery = serve.NewDiscovery(serve.DiscoveryOptions{
		Store: st, Clients: r.g, Credentials: noCredentials{}, Interval: time.Hour,
		Logger: slog.New(slog.DiscardHandler),
	})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	result, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	select {
	case name := <-listed:
		if name != "laptop" {
			t.Fatalf("listed %q", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no list at connect")
	}
	models, _, err := st.Objects().List(t.Context(), v1.KindModel, store.Filter{Provider: p.Status.ID}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, o := range models {
		m := o.(*v1.Model)
		names[m.Metadata.Name] = true
		if m.Status.Owner != p.Status.Owner || m.Status.Source != v1.SourceDiscovered || m.Spec.Targets[0].Provider != "laptop" {
			t.Errorf("Model %s: owner %q source %q targets %v", m.Metadata.Name, m.Status.Owner, m.Status.Source, m.Spec.Targets)
		}
	}
	if !names["laptop/llama3.1"] || !names["laptop/qwen2.5"] || len(names) != 2 {
		t.Fatalf("discovered %v", names)
	}
	// The agent goes; a list now fails at once and keeps the catalog.
	stop()
	<-result
	waitFor(t, func() bool { return r.g.Sessions() == 0 }, "the session to close")
	started := time.Now()
	discovery.List(t.Context(), p)
	if time.Since(started) > 2*time.Second {
		t.Errorf("a list with no session took %s", time.Since(started))
	}
	models, _, err = st.Objects().List(t.Context(), v1.KindModel, store.Filter{Provider: p.Status.ID}, store.Page{})
	if err != nil || len(models) != 2 {
		t.Fatalf("the catalog after a failed list: %d %v", len(models), err)
	}
	obj, _, err := st.Objects().Get(t.Context(), v1.KindProvider, p.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h := obj.(*v1.Provider).Status.Health; h == nil || !strings.Contains(h.LastError, "model list") || !strings.Contains(h.LastError, "no live session") {
		t.Errorf("status.health after a failed list: %+v", h)
	}
}

// noCredentials is the credential source of a tunneled Provider: none.
type noCredentials struct{}

func (noCredentials) Credential(context.Context, string) ([]byte, error) { return nil, nil }

// TestRegistryFailuresAreReported: a store that cannot be read makes the
// exchange fail with the store's error rather than with no session.
func TestRegistryFailuresAreReported(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	g := New(Options{Store: failingStore{st}, Verifier: r.verifier, Clients: r.g.o.Clients})
	client, err := g.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/models", nil)
	_, err = client.Do(req)
	if err == nil || errors.Is(err, ErrNoSession) || !strings.Contains(err.Error(), "reading the registry") || !errors.Is(err, errStore) {
		t.Fatalf("a failing store: %v", err)
	}
}
