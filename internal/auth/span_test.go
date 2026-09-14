// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
)

// TestAuthorizerSpanCarriesTheDecision is spec 019's lux.authorizer: one
// span per question, a child of the caller's span, carrying the action
// and the decision in the metric's vocabulary, allow, deny, or
// unavailable, and never the subject, the issuer, or the claims.
func TestAuthorizerSpanCarriesTheDecision(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	s, z := newStubAuthorizer(t)
	s.Deny(stub.Rule{Action: authorizer.ActionKeyDelete}, "not yours")
	caller := Caller{Subject: "https://login.example.com|alice", Issuer: "https://login.example.com", Sub: "alice", Claims: map[string]any{"email": "alice@example.com"}}
	ctx, parent := tp.Tracer("test").Start(t.Context(), "lux.api")
	res := authz.Resource{Kind: "Key", ID: "key_01J9ZK2P7Q8R9S0T1U2V3W4X5Y"}
	if _, err := z.Decide(ctx, caller, authorizer.ActionKeyRead, res, authz.Caller{ID: "req_1", IP: "203.0.113.9"}); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if _, err := z.Decide(ctx, caller, authorizer.ActionKeyDelete, res, authz.Caller{ID: "req_2"}); err == nil {
		t.Fatal("deny was allowed")
	}
	s.Fail(http.StatusInternalServerError)
	if _, err := z.Decide(ctx, caller, authorizer.ActionKeyUpdate, res, authz.Caller{ID: "req_3"}); err == nil {
		t.Fatal("an outage was allowed")
	}
	parent.End()
	var got []string
	for _, sp := range sr.Ended() {
		if sp.Name() != SpanAuthorizer {
			continue
		}
		if sp.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Error("the authorizer span is not the caller's child")
		}
		attrs := map[string]string{}
		for _, kv := range sp.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
			for _, secret := range []string{"alice", "login.example.com", "203.0.113.9"} {
				if strings.Contains(kv.Value.String(), secret) {
					t.Errorf("%s carries %q", kv.Key, secret)
				}
			}
		}
		if len(attrs) != 2 {
			t.Errorf("attributes %v", attrs)
		}
		got = append(got, attrs[AttrAction]+"="+attrs[AttrDecision])
	}
	want := []string{authorizer.ActionKeyRead + "=" + DecisionAllow, authorizer.ActionKeyDelete + "=" + DecisionDeny, authorizer.ActionKeyUpdate + "=" + DecisionUnavailable}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("spans %v, want %v", got, want)
	}
}
