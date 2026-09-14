// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"testing"

	"latere.ai/x/pkg/authz"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The identity group is spec 006's plane boundary: a Key on /v1 and an
// issuer token on a door are each unauthenticated, a control plane
// request without a bearer is 401, with the authorizer down every
// control plane decision is authorizer_unavailable while the doors keep
// serving, and an object's owner is the token's subject.
var identityCases = []testCase{
	{group: "identity", name: "case006UnauthenticatedControlPlane", spec: 6, mode: serverMode, fn: case006UnauthenticatedControlPlane},
	{group: "identity", name: "case006KeyOnControlPlaneIsUnauthenticated", spec: 6, bearer: true, key: true, fn: case006KeyOnControlPlaneIsUnauthenticated},
	{group: "identity", name: "case006TokenOnDoorIsUnauthenticated", spec: 6, bearer: true, key: true, fn: case006TokenOnDoorIsUnauthenticated},
	{group: "identity", name: "case006OwnerIsTheTokensSubject", spec: 6, bearer: true, mode: serverMode, fn: case006OwnerIsTheTokensSubject},
	{group: "identity", name: "case006AuthorizerUnavailable", spec: 6, bearer: true, key: true, stubs: true, fn: case006AuthorizerUnavailable},
}

// case006UnauthenticatedControlPlane: a control plane request without a
// bearer is unauthenticated, and so is one with a bearer no issuer
// signed.
func case006UnauthenticatedControlPlane(t testing.TB, c *client) {
	c.expect(t, c.v1(t, http.MethodGet, "/keys", nil, bearer("")), "unauthenticated")
	c.expect(t, c.v1(t, http.MethodGet, "/self", nil, bearer("not.a.token")), "unauthenticated")
	c.expect(t, c.apply(t, v1.KindBudget, c.name("anon"), map[string]any{"spec": budgetSpec("1")}, bearer("")), "unauthenticated")
}

// case006KeyOnControlPlaneIsUnauthenticated: the suite's Key, a
// credential the doors accept, is unauthenticated on /v1.
func case006KeyOnControlPlaneIsUnauthenticated(t testing.TB, c *client) {
	c.expect(t, c.v1(t, http.MethodGet, "/self", nil, bearer(c.key.value)), "unauthenticated")
	c.expect(t, c.list(t, v1.KindKey, "", bearer(c.key.value)), "unauthenticated")
}

// case006TokenOnDoorIsUnauthenticated: the issuer token /v1 accepts is
// unauthenticated on every door, since no Key was created with it.
func case006TokenOnDoorIsUnauthenticated(t testing.TB, c *client) {
	for _, d := range c.well.Dialects {
		c.expectDoor(t, d, c.door(t, d, http.MethodGet, modelsPath(d), nil, c.token), "unauthenticated")
	}
}

// case006OwnerIsTheTokensSubject: an object applied with a token is
// owned by that token's rendered subject, for the default subject and
// for a second one Token mints.
func case006OwnerIsTheTokensSubject(t testing.TB, c *client) {
	own := c.object(t, v1.KindBudget, "owner", budgetSpec("1"))
	defer c.mustDelete(t, v1.KindBudget, str(own, "status.id"))
	if c.cfg.Subject != "" && str(own, "status.owner") != c.cfg.Subject {
		t.Errorf("owner %q, want the default subject %q", str(own, "status.owner"), c.cfg.Subject)
	}
	iss, _, ok := authz.SplitSubject(c.cfg.Subject)
	if !ok {
		c.skip(t, "Config.Subject "+c.cfg.Subject+" is not a rendered subject, so no second subject of its issuer can be named")
	}
	other := authz.Subject(iss, c.name("other"))
	token, ok := c.cfg.Token(other)
	if !ok {
		c.skip(t, "Token mints nothing for the subject "+other)
	}
	name := c.name("other-owner")
	resp := c.apply(t, v1.KindBudget, name, map[string]any{"metadata": c.meta(name), "spec": budgetSpec("1")}, bearer(token))
	if resp.Status != http.StatusCreated {
		t.Fatalf("as %s: %d %s", other, resp.Status, excerpt(resp.Body))
	}
	theirs := resp.json(t)
	if str(theirs, "status.owner") != other {
		t.Errorf("owner %q, want %q", str(theirs, "status.owner"), other)
	}
	if del := c.del(t, v1.KindBudget, str(theirs, "status.id"), bearer(token)); del.Status != http.StatusNoContent {
		t.Errorf("DELETE as the owner: %d %s", del.Status, excerpt(del.Body))
		c.mustDelete(t, v1.KindBudget, str(theirs, "status.id"))
	}
}

// case006AuthorizerUnavailable: with the stub authorizer answering 503,
// a control plane request that asks a decision is authorizer_unavailable
// and a data plane request with a valid Key is served; the table is
// restored after.
func case006AuthorizerUnavailable(t testing.TB, c *client) {
	self := c.v1(t, http.MethodGet, "/self", nil)
	if str(self.json(t), "policy") != "authorizer" {
		c.skip(t, "the server decides with the "+str(self.json(t), "policy")+" policy, which has no endpoint to take down")
	}
	if c.stubs.Authorizer == "" {
		c.skip(t, "the stubs document names no authorizer")
	}
	c.authorizerFail(t, http.StatusServiceUnavailable)
	defer c.authorizerFail(t, 0)
	// A list is never cached by the shared client, since its resource has
	// no id, so it asks the endpoint every time.
	c.expect(t, c.list(t, v1.KindKey, ""), "authorizer_unavailable")
	if code := c.doorStatus(t, c.key.value); code != "" {
		t.Errorf("a door answered %s while the authorizer was down", code)
	}
	if resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("gpt"), false), c.key.value); resp.Status != http.StatusOK {
		t.Errorf("a served request answered %d while the authorizer was down: %s", resp.Status, excerpt(resp.Body))
	}
}
