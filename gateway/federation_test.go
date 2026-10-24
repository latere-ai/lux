// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// tokenCreds is a ProviderCredentialSource and CredentialRefusals that
// answers a federated Provider with a token it hands out and everything
// else from the world's values, recording the refusals it is told of.
type tokenCreds struct {
	*fakeCreds
	mu      sync.Mutex
	token   string
	err     error
	asked   []string
	refused []string
}

func (c *tokenCreds) ProviderCredential(ctx context.Context, p *v1.Provider) ([]byte, error) {
	if p.Spec.Federated() == nil {
		return c.Credential(ctx, p.Status.ID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, p.Metadata.Name)
	if c.err != nil {
		return nil, c.err
	}
	return []byte(c.token), nil
}

func (c *tokenCreds) Refused(_ *v1.Provider, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused = append(c.refused, string(value))
}

// TestFederatedForward is spec 051's fifth criterion at the doors: a
// source that reads the Provider is asked with it, a federated
// Provider's token travels as Authorization: Bearer with no x-api-key, a
// 401 tells the source which value was refused, a source that fails is
// provider_unavailable naming the credential, and a Provider with a value
// is still asked by id.
func TestFederatedForward(t *testing.T) {
	w := newWorld(t)
	creds := &tokenCreds{fakeCreds: w.creds, token: "sk-ant-oat01-exchanged"}
	ant := w.catalog.providers["ant"]
	ant.Spec.Credential = &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer, Federation: &v1.Federation{
		RuleID: "fdrl_01", OrganizationID: "4f6c1d2a-8e3b-4c7d-9a1e-2b5f6c7d8e9f", ServiceAccountID: "svac_01",
	}}
	o := w.options()
	o.Credentials = creds
	w.h = New(o)

	w.anthropic.respondJSON(200, anthropicResponse)
	if rec := w.post("/anthropic/v1/messages", messagesBody("claude", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := w.anthropic.last(t)
	if got.Header.Get("Authorization") != "Bearer sk-ant-oat01-exchanged" || got.Header.Get("x-api-key") != "" {
		t.Fatalf("outbound credential headers: %v", got.Header)
	}
	if len(creds.asked) != 1 || creds.asked[0] != "ant" {
		t.Fatalf("asked for %v", creds.asked)
	}

	w.anthropic.respondJSON(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`)
	w.post("/anthropic/v1/messages", messagesBody("claude", false))
	if len(creds.refused) != 1 || creds.refused[0] != "sk-ant-oat01-exchanged" {
		t.Fatalf("refused %v", creds.refused)
	}

	creds.err = errors.New("the token exchange answered 401: authentication_error: Authentication failed")
	rec := w.post("/anthropic/v1/messages", messagesBody("claude", false))
	if code := errorCode(t, rec); code != CodeProviderUnavailable || !strings.Contains(rec.Header().Get("Lux-Error-Detail"), "Provider ant: credential: the token exchange answered 401") {
		t.Fatalf("a failed exchange: %s %q", code, rec.Header().Get("Lux-Error-Detail"))
	}

	w.openai.respondJSON(200, openaiChatResponse)
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != 200 || w.openai.last(t).Header.Get("Authorization") != "Bearer "+openaiCredential {
		t.Fatalf("a Provider with a value: %d %v", rec.Code, w.openai.last(t).Header)
	}
	if n := w.anthropic.count(); n != 2 {
		t.Errorf("the upstream saw %d requests; the failed exchange sent none", n)
	}
}
