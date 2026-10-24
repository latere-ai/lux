// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lux/internal/federation"
	v1 "latere.ai/x/lux/manifest/v1"
)

type staticIdentity string

func (s staticIdentity) IdentityToken(context.Context) (string, error) { return string(s), nil }

// TestDiscoveryWithFederation is spec 051's sixth criterion: discovery
// lists a federated Provider's models with the exchanged token as
// Authorization: Bearer and no x-api-key; the health probe of a 401 drops
// the token so the next list exchanges again; and without a workload
// identity the list fails naming the variables.
func TestDiscoveryWithFederation(t *testing.T) {
	h := newHarness(t)
	models := &stub{pages: map[string]string{"": `{"data":[{"id":"claude-a"}],"has_more":false}`}}
	var exchanges atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		n := exchanges.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat01-` + string(rune('0'+n)) + `","token_type":"Bearer","expires_in":3600}`))
	})
	mux.Handle("/", models)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := h.provider(t, "anthropic", v1.DialectAnthropic, srv.URL+"/v1", func(p *v1.Provider) {
		p.Spec.Credential = &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer, Federation: &v1.Federation{
			RuleID: "fdrl_01", OrganizationID: "4f6c1d2a-8e3b-4c7d-9a1e-2b5f6c7d8e9f", ServiceAccountID: "svac_01",
		}}
	})
	tokens := federation.New(federation.Options{Identity: staticIdentity("jwt"), Clients: h.clients, Now: h.clock, Logger: h.logger})
	creds := FederatedCredentials{Base: h.creds, Tokens: tokens}
	d := NewDiscovery(DiscoveryOptions{Store: h.st, Clients: h.clients, Credentials: creds, Interval: time.Hour, Tail: 10 * time.Millisecond,
		Holder: "a", Logger: h.logger, Now: h.clock, Jitter: func() float64 { return 0 }})
	d.acquire(t.Context())
	d.Tick(t.Context())
	if got := h.models(t, string(v1.SourceDiscovered)); names(got) != "anthropic/claude-a" {
		t.Fatalf("discovered %q", names(got))
	}
	if auth, key := models.lastHeader("Authorization"), models.lastHeader("x-api-key"); auth != "Bearer sk-ant-oat01-1" || key != "" {
		t.Fatalf("the list sent Authorization %q and x-api-key %q", auth, key)
	}

	// A 401 to the health probe drops the token; the next request exchanges.
	health := NewHealth(HealthOptions{Store: h.st, Clients: h.clients, Credentials: creds, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock})
	models.set(http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	if failed, lastError := health.Probe(t.Context(), p); failed || lastError != "credential refused: 401" {
		t.Fatalf("the probe of a 401: %v %q", failed, lastError)
	}
	models.setPages(map[string]string{"": `{"data":[{"id":"claude-a"}],"has_more":false}`})
	if failed, _ := health.Probe(t.Context(), p); failed || exchanges.Load() != 2 || models.lastHeader("Authorization") != "Bearer sk-ant-oat01-2" {
		t.Fatalf("after the 401: %d exchanges, Authorization %q", exchanges.Load(), models.lastHeader("Authorization"))
	}

	// A Provider with a value is still the stored value's.
	keyed := h.provider(t, "keyed", v1.DialectAnthropic, srv.URL+"/v1", nil)
	if value, err := creds.ProviderCredential(t.Context(), keyed); err != nil || string(value) != canary {
		t.Fatalf("a keyed Provider: %q %v", value, err)
	}
	none := FederatedCredentials{Base: h.creds}
	if failed, lastError := NewHealth(HealthOptions{Store: h.st, Clients: h.clients, Credentials: none, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock}).Probe(t.Context(), p); !failed || !strings.Contains(lastError, "LUX_WORKLOAD_IDENTITY_KUBERNETES") {
		t.Fatalf("without a workload identity: %v %q", failed, lastError)
	}
	if _, err := none.ProviderCredential(t.Context(), p); !errors.Is(err, ErrNoWorkloadIdentity) {
		t.Fatalf("without a workload identity: %v", err)
	}
}
