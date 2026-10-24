// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package federation gets the credential of a federated Provider (spec
// 051): the gateway's own identity token, exchanged at the upstream's
// token endpoint for a short-lived access token kept in memory and
// exchanged again before it expires. Nothing it holds reaches the store.
package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The schedule of spec 051, modelled on the Anthropic SDKs': a token is
// reused until RefreshBefore its expiry; from then until MandatoryBefore
// one request exchanges again while the others keep it, and a failed
// exchange keeps it; from MandatoryBefore a request waits for an
// exchange. A failure is answered again for RetryAfter without another
// exchange, so a token endpoint that is down is not asked once per
// request.
const (
	RefreshBefore   = 120 * time.Second
	MandatoryBefore = 30 * time.Second
	RetryAfter      = 5 * time.Second
)

// exchangeTimeout bounds one exchange, which runs past a caller that
// stops waiting so that the token it gets is kept.
const exchangeTimeout = 30 * time.Second

// maxResponseBytes bounds the token endpoint's answer read.
const maxResponseBytes = 64 << 10

// grantType is RFC 7523's JWT bearer grant.
const grantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// IdentitySource answers the gateway's identity token for one exchange.
type IdentitySource interface {
	IdentityToken(ctx context.Context) (string, error)
}

// ClientSource is the upstream client per Provider, spec 005's client
// pinned to the Provider's base URL.
type ClientSource interface {
	Client(ctx context.Context, p *v1.Provider) (*http.Client, error)
}

// Options is what New builds an Exchanger from. Identity and Clients are
// required; Now is time.Now and Logger slog.Default when nil.
type Options struct {
	Identity IdentitySource
	Clients  ClientSource
	Now      func() time.Time
	Logger   *slog.Logger
}

// Exchanger holds each federated Provider's access token by the
// Provider's id.
type Exchanger struct {
	o      Options
	mu     sync.Mutex
	tokens map[string]*held
}

// held is one Provider's token. exchange lets one exchange run at a
// time; mu guards the fields, which a request reads without waiting for
// an exchange.
type held struct {
	exchange sync.Mutex
	mu       sync.Mutex
	// key is the base URL and the identifiers the token was exchanged
	// for; a Provider that names others exchanges again. failKey is what
	// the last failed exchange was for, apart, so a failure for other
	// identifiers never makes the token held for these serve them.
	key      string
	token    []byte
	expires  time.Time
	failKey  string
	failedAt time.Time
	failure  error
}

// New returns an Exchanger over o.
func New(o Options) *Exchanger {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Exchanger{o: o, tokens: map[string]*held{}}
}

// keyOf is what a token was exchanged for.
func keyOf(p *v1.Provider, f *v1.Federation) string {
	return strings.Join([]string{p.Spec.BaseURL, f.RuleID, f.OrganizationID, f.ServiceAccountID, f.WorkspaceID}, "\x00")
}

func (e *Exchanger) entry(id string) *held {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.tokens[id]
	if h == nil {
		h = &held{}
		e.tokens[id] = h
	}
	return h
}

// current is the token held for key, usable when it is before its
// mandatory refresh and fresh when it is before its advisory one, and the
// failure of an exchange for key within RetryAfter.
func (h *held) current(key string, now time.Time) (token []byte, usable, fresh bool, recent error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.key == key && h.token != nil {
		usable = now.Before(h.expires.Add(-MandatoryBefore))
		fresh = now.Before(h.expires.Add(-RefreshBefore))
		token = h.token
	}
	if h.failKey == key && h.failure != nil && now.Sub(h.failedAt) < RetryAfter {
		recent = h.failure
	}
	return token, usable, fresh, recent
}

// Token is p's access token: the one held while it is fresh, a new one
// exchanged otherwise, under spec 051's schedule.
func (e *Exchanger) Token(ctx context.Context, p *v1.Provider) ([]byte, error) {
	f := p.Spec.Federated()
	if f == nil {
		return nil, errors.New("the Provider's credential is not federated")
	}
	key, h := keyOf(p, f), e.entry(p.Status.ID)
	token, usable, fresh, recent := h.current(key, e.o.Now())
	if fresh {
		return token, nil
	}
	if usable {
		// The advisory window: one request exchanges, the others keep
		// the token, and a failed exchange keeps it too, without another
		// for RetryAfter.
		if recent != nil || !h.exchange.TryLock() {
			return token, nil
		}
		defer h.exchange.Unlock()
		if next, err := e.exchange(ctx, p, f, h, key); err == nil {
			return next, nil
		}
		return token, nil
	}
	h.exchange.Lock()
	defer h.exchange.Unlock()
	// Another request may have exchanged, or failed to, while this one
	// waited.
	token, _, fresh, recent = h.current(key, e.o.Now())
	if fresh {
		return token, nil
	}
	if recent != nil {
		return nil, recent
	}
	return e.exchange(ctx, p, f, h, key)
}

// Refused drops value when it is the token held for p, after the
// upstream answered 401 to a request that carried it, so the next request
// exchanges again.
func (e *Exchanger) Refused(p *v1.Provider, value []byte) {
	if len(value) == 0 {
		return
	}
	h := e.entry(p.Status.ID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if bytes.Equal(h.token, value) {
		h.token, h.expires = nil, time.Time{}
	}
}

// exchange gets a new token for p and holds it, or records the failure.
// The caller holds h.exchange.
func (e *Exchanger) exchange(ctx context.Context, p *v1.Provider, f *v1.Federation, h *held, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exchangeTimeout)
	defer cancel()
	token, lifetime, err := e.post(ctx, p, f)
	now := e.o.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.failKey, h.failedAt, h.failure = key, now, err
		e.o.Logger.WarnContext(ctx, "federation: the token exchange failed", "provider", p.Metadata.Name, "error", err)
		return nil, err
	}
	h.key, h.token, h.expires, h.failKey, h.failure = key, token, now.Add(lifetime), "", nil
	e.o.Logger.InfoContext(ctx, "federation: exchanged a token", "provider", p.Metadata.Name, "valid_for", lifetime.String())
	return token, nil
}

// post sends the exchange of spec 051 and reads its answer: the access
// token and how long it lasts. An error names the status and the
// upstream's message, never the assertion.
func (e *Exchanger) post(ctx context.Context, p *v1.Provider, f *v1.Federation) ([]byte, time.Duration, error) {
	assertion, err := e.o.Identity.IdentityToken(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("the workload identity token: %w", err)
	}
	body := map[string]string{
		"grant_type": grantType, "assertion": assertion,
		"federation_rule_id": f.RuleID, "organization_id": f.OrganizationID, "service_account_id": f.ServiceAccountID,
	}
	if f.WorkspaceID != "" {
		body["workspace_id"] = f.WorkspaceID
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	client, err := e.o.Clients.Client(ctx, p)
	if err != nil {
		return nil, 0, fmt.Errorf("client: %w", err)
	}
	endpoint := TokenURL(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("POST %s: reading the answer: %w", endpoint, err)
	}
	answer = bytes.ReplaceAll(answer, []byte(assertion), []byte("[redacted]"))
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("the token exchange answered %d: %s", resp.StatusCode, upstreamMessage(answer))
	}
	var got struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(answer, &got); err != nil {
		return nil, 0, fmt.Errorf("the token exchange answered 200 with a body that is not a token response: %w", err)
	}
	if got.AccessToken == "" || got.ExpiresIn <= 0 {
		return nil, 0, errors.New("the token exchange answered 200 without an access_token and a positive expires_in")
	}
	return []byte(got.AccessToken), time.Duration(got.ExpiresIn) * time.Second, nil
}

// TokenURL is the Provider's token endpoint: its base URL, which names
// the API's version, with oauth/token after it.
func TokenURL(p *v1.Provider) string {
	return strings.TrimSuffix(p.Spec.BaseURL, "/") + "/oauth/token"
}

// upstreamMessage is the error's type and message from Anthropic's error
// shape, or the start of the body otherwise.
func upstreamMessage(body []byte) string {
	var shape struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &shape) == nil && shape.Error.Message != "" {
		if shape.Error.Type != "" {
			return shape.Error.Type + ": " + shape.Error.Message
		}
		return shape.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "an empty body"
	}
	return strconv.Quote(s)
}
