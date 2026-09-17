// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"

	"latere.ai/x/lux/authorizer"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The subjects of the owner policy's table.
const (
	adminSubject = fixtureIssuer + "|root"
	otherSubject = fixtureIssuer + "|bob"
)

// verdict is what one row expects: allow, or a deny with the reason, and
// for a list the filter narrowed to the subject.
type verdict struct {
	allow    bool
	reason   string
	filtered bool
}

var (
	allow    = verdict{allow: true}
	filtered = verdict{allow: true, filtered: true}
	notOwner = verdict{reason: authz.ReasonNotOwner}
	admins   = verdict{reason: ReasonAdminOnly}
)

// TestOwnerPolicy is spec 006's owner policy, table-driven over every
// action and the roles: an admin, the owner of the object, another
// subject, and the anonymous subject. The tunnel exception, a Provider
// with tunnel true, which any subject may create and its owner may
// update, delete, and tunnel, has its own rows; a deny on an object that
// does not exist reads not_owner like one on another subject's; a list
// is allowed with the filter narrowed to the subject; and no limits are
// granted on any allow.
func TestOwnerPolicy(t *testing.T) {
	tunnelled := fixtureProvider()
	tunnelled.Spec.Tunnel = true
	tunnelled.Status.ID = "prv_01J9TESTTUNNEL000000000000"
	tunnelled.Metadata.Name = "laptop"
	objects := objectsOf(fixtureProvider(), tunnelled, fixtureModel(), fixtureKey(), fixtureBudget(t))
	policy := &OwnerPolicy{Admins: []string{adminSubject}, Objects: objects}

	// object builds the resource of an action on the fixture of its kind,
	// or on the tunnelled Provider, or with an id no object has.
	type row struct {
		name   string
		action string
		res    authz.Resource
		owner  verdict // the fixtures' owner, alice
		other  verdict // bob
	}
	provider, model, key, budget := fixtureProvider(), fixtureModel(), fixtureKey(), fixtureBudget(t)
	unknownKey := fixtureKey()
	unknownKey.Status.ID = "key_01J9NOSUCHKEY000000000000000"
	unknownBudget := fixtureBudget(t)
	unknownBudget.Status.ID = "bud_01J9NOSUCHBUDGET00000000000"
	rows := []row{
		{"provider.create", authorizer.ActionProviderCreate, authorizer.ProviderCreate(provider), admins, admins},
		{"provider.create of a tunnel", authorizer.ActionProviderCreate, authorizer.ProviderCreate(tunnelled), allow, allow},
		{"provider.read", authorizer.ActionProviderRead, authorizer.ProviderObject(provider), allow, allow},
		{"provider.update", authorizer.ActionProviderUpdate, authorizer.ProviderObject(provider), admins, admins},
		{"provider.update of a tunnel", authorizer.ActionProviderUpdate, authorizer.ProviderObject(tunnelled), allow, notOwner},
		{"provider.delete", authorizer.ActionProviderDelete, authorizer.ProviderObject(provider), admins, admins},
		{"provider.delete of a tunnel", authorizer.ActionProviderDelete, authorizer.ProviderObject(tunnelled), allow, notOwner},
		{"provider.tunnel", authorizer.ActionProviderTunnel, authorizer.ProviderObject(tunnelled), allow, notOwner},
		{"provider.list", authorizer.ActionProviderList, authorizer.ProviderList(), allow, allow},
		{"model.create", authorizer.ActionModelCreate, authorizer.ModelCreate(model), admins, admins},
		{"model.read", authorizer.ActionModelRead, authorizer.ModelObject(model), allow, allow},
		{"model.update", authorizer.ActionModelUpdate, authorizer.ModelObject(model), admins, admins},
		{"model.delete", authorizer.ActionModelDelete, authorizer.ModelObject(model), admins, admins},
		{"model.list", authorizer.ActionModelList, authorizer.ModelList(), allow, allow},
		{"model.use", authorizer.ActionModelUse, authorizer.ModelUse("anthropic/*", fixtureMatched), allow, allow},
		{"key.create", authorizer.ActionKeyCreate, authorizer.KeyCreate(key), allow, allow},
		{"key.read", authorizer.ActionKeyRead, authorizer.KeyObject(key), allow, notOwner},
		{"key.read of an unknown id", authorizer.ActionKeyRead, authorizer.KeyObject(unknownKey), notOwner, notOwner},
		{"key.update", authorizer.ActionKeyUpdate, authorizer.KeyObject(key), allow, notOwner},
		{"key.delete", authorizer.ActionKeyDelete, authorizer.KeyObject(key), allow, notOwner},
		{"key.list", authorizer.ActionKeyList, authorizer.KeyList(), filtered, filtered},
		{"budget.create", authorizer.ActionBudgetCreate, authorizer.BudgetCreate(budget), allow, allow},
		{"budget.read", authorizer.ActionBudgetRead, authorizer.BudgetObject(budget), allow, notOwner},
		{"budget.update", authorizer.ActionBudgetUpdate, authorizer.BudgetObject(budget), allow, notOwner},
		{"budget.delete", authorizer.ActionBudgetDelete, authorizer.BudgetObject(budget), allow, notOwner},
		{"budget.delete of an unknown id", authorizer.ActionBudgetDelete, authorizer.BudgetObject(unknownBudget), notOwner, notOwner},
		{"budget.list", authorizer.ActionBudgetList, authorizer.BudgetList(), filtered, filtered},
		{"budget.draw", authorizer.ActionBudgetDraw, authorizer.BudgetObject(budget), allow, notOwner},
		{"usage.read", authorizer.ActionUsageRead, authorizer.UsageRead(nil, nil), filtered, filtered},
	}
	covered := map[string]bool{}
	for _, r := range rows {
		covered[r.action] = true
	}
	for _, a := range authorizer.Actions() {
		if !covered[a] {
			t.Errorf("the table has no row for %s", a)
		}
	}

	check := func(t *testing.T, subject string, req authz.Request, want verdict) {
		t.Helper()
		d, err := policy.Authorize(t.Context(), req)
		if err != nil {
			t.Fatalf("Authorize = %v", err)
		}
		if d.Allow != want.allow || (!want.allow && d.Reason != want.reason) {
			t.Fatalf("as %q: allow %v reason %q; want allow %v reason %q", subject, d.Allow, d.Reason, want.allow, want.reason)
		}
		if d.Limits != nil {
			t.Errorf("as %q: limits %s were granted; the owner policy grants none", subject, d.Limits)
		}
		var filter *authz.Filter
		if want.filtered {
			filter = &authz.Filter{Owners: []string{subject}}
		}
		if !reflect.DeepEqual(d.Filter, filter) {
			t.Errorf("as %q: filter %+v, want %+v", subject, d.Filter, filter)
		}
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			for _, tc := range []struct {
				subject string
				want    verdict
			}{
				{adminSubject, allow},
				{fixtureSubject, r.owner},
				{otherSubject, r.other},
				{"", verdict{reason: authz.ReasonAnonymous}},
			} {
				c := Caller{Subject: tc.subject}
				c.Issuer, c.Sub, _ = authz.SplitSubject(tc.subject)
				check(t, tc.subject, Request(c, r.action, r.res, info), tc.want)
			}
		})
	}

	t.Run("through the Authorizer", func(t *testing.T) {
		z := NewAuthorizer(policy)
		bob := Caller{Subject: otherSubject, Issuer: fixtureIssuer, Sub: "bob"}
		_, err := z.Decide(t.Context(), bob, authorizer.ActionKeyRead, authorizer.KeyObject(key), info)
		if e := wantCode(t, err, CodeForbidden); !strings.HasSuffix(e.Detail, authz.ReasonNotOwner) {
			t.Errorf("detail %q", e.Detail)
		}
		d, err := z.Decide(t.Context(), bob, authorizer.ActionKeyList, authorizer.KeyList(), info)
		if err != nil || !reflect.DeepEqual(d.Filter, &authz.Filter{Owners: []string{otherSubject}}) || d.Limits != (authorizer.Limits{}) {
			t.Errorf("key.list = %+v, %v", d, err)
		}
		// The Lookup over the owner policy: bob may use every Model and
		// draw no Budget of alice's, which reads as not_found.
		l := z.Lookup(bob, info, fixtures(t))
		if refs, err := l.Models(t.Context(), "*"); err != nil || len(refs) != 2 {
			t.Errorf("Models = %v, %v", refs, err)
		}
		if _, err := l.Budget(t.Context(), "team-research"); err == nil || !strings.HasPrefix(err.Error(), "not_found") {
			t.Errorf("Budget = %v", err)
		}
	})

	t.Run("an action outside the vocabulary", func(t *testing.T) {
		c := Caller{Subject: adminSubject, Issuer: fixtureIssuer, Sub: "root"}
		check(t, adminSubject, Request(c, "key.rotate", authorizer.KeyObject(key), info), verdict{reason: ReasonUnknownAction})
	})

	t.Run("a lookup that fails is no decision", func(t *testing.T) {
		broken := &OwnerPolicy{Objects: &catalog{err: errors.New("store: timeout")}}
		c := Caller{Subject: fixtureSubject, Issuer: fixtureIssuer, Sub: "alice"}
		if _, err := broken.Authorize(t.Context(), Request(c, authorizer.ActionKeyRead, authorizer.KeyObject(key), info)); err == nil || !strings.Contains(err.Error(), "store: timeout") {
			t.Errorf("err = %v", err)
		}
		_, err := NewAuthorizer(broken).Decide(t.Context(), c, authorizer.ActionKeyRead, authorizer.KeyObject(key), info)
		wantCode(t, err, CodeAuthorizerUnavailable)
		// A row that needs no object still answers.
		if d, err := broken.Authorize(t.Context(), Request(c, authorizer.ActionKeyCreate, authorizer.KeyCreate(key), info)); err != nil || !d.Allow {
			t.Errorf("key.create = %+v, %v", d, err)
		}
	})

	t.Run("no lookup at all", func(t *testing.T) {
		none := &OwnerPolicy{}
		c := Caller{Subject: fixtureSubject, Issuer: fixtureIssuer, Sub: "alice"}
		if _, err := none.Authorize(t.Context(), Request(c, authorizer.ActionKeyRead, authorizer.KeyObject(key), info)); err == nil || !strings.Contains(err.Error(), "no object lookup") {
			t.Errorf("err = %v", err)
		}
		// An object named by no id is denied without a lookup.
		d, err := none.Authorize(t.Context(), Request(c, authorizer.ActionKeyRead, authz.NewResource(v1.KindKey, "", nil), info))
		if err != nil || d.Allow || d.Reason != authz.ReasonNotOwner {
			t.Errorf("key.read with no id = %+v, %v", d, err)
		}
	})
}

// TestOwnerPolicyIsAnAuthorizer: the type satisfies the shared interface,
// so the Authorizer over it and over the client are one to the API.
func TestOwnerPolicyIsAnAuthorizer(t *testing.T) {
	var _ authz.Authorizer = (*OwnerPolicy)(nil)
	var a authz.Authorizer = &OwnerPolicy{Objects: objectsOf()}
	if _, err := a.Authorize(context.Background(), authz.Probe(authorizer.ActionKeyRead, v1.KindKey)); err != nil {
		t.Fatal(err)
	}
}

// scopedRequest is one envelope from a personal access token: the claims
// a verified token carries, with the grants its holder chose.
func scopedRequest(subject, action, kind, id string, grants ...authz.Grant) authz.Request {
	claims := map[string]any{"iss": fixtureIssuer, "sub": "root", "token_use": authz.TokenUsePAT}
	if grants != nil {
		claims["authorization_details"] = grants
	}
	req := authz.Request{
		Subject:  subject,
		Action:   action,
		Resource: authz.NewResource(kind, id, map[string]any{"owner": subject}),
		Claims:   claims,
	}
	req.Issuer, req.Sub, _ = authz.SplitSubject(subject)
	return req
}

// grantOn is one RFC 9396 entry: the actions, qualified by the core, and
// the one resource they name.
func grantOn(id string, actions ...string) authz.Grant {
	qualified := make([]string, 0, len(actions))
	for _, a := range actions {
		qualified = append(qualified, "lux:"+a)
	}
	return authz.Grant{
		Type:       authz.GrantType,
		Actions:    qualified,
		Datatypes:  []string{authorizer.Kind(actions[0])},
		Locations:  []string{"https://api.example.com"},
		Identifier: id,
	}
}

// TestOwnerPolicyRestrictsToTheTokensGrants is spec 006's grant case at
// the site that decides: a personal access token granted one action on
// one resource is refused every other action on that resource and that
// action on every other resource, with reason grant, and is not refused
// the one its grant names.
//
// The subject is an admin, so the owner policy allows every row of the
// table and the grants alone move the answer. The rows after it are the
// three properties the intersection has to keep: a grant is never
// authority, a token of another class is decided by the policy alone,
// and a personal token that carries no grant reaches nothing.
func TestOwnerPolicyRestrictsToTheTokensGrants(t *testing.T) {
	const here = "mdl_01J9TESTMODELHERE00000000"
	const elsewhere = "mdl_01J9TESTMODELAWAY00000000"
	p := &OwnerPolicy{Admins: []string{adminSubject}, Objects: objectsOf()}
	read := grantOn(here, authorizer.ActionModelRead)

	for _, tc := range []struct {
		name string
		req  authz.Request
		want verdict
	}{
		{
			"the action and the resource its own grant names",
			scopedRequest(adminSubject, authorizer.ActionModelRead, v1.KindModel, here, read),
			allow,
		},
		{
			"another action on the granted resource",
			scopedRequest(adminSubject, authorizer.ActionModelUpdate, v1.KindModel, here, read),
			verdict{reason: authz.ReasonGrant},
		},
		{
			"the granted action on a resource no grant names",
			scopedRequest(adminSubject, authorizer.ActionModelRead, v1.KindModel, elsewhere, read),
			verdict{reason: authz.ReasonGrant},
		},
		{
			// A grant is a restriction and never authority: the policy
			// answers first, and it denies another subject's object
			// whatever the token says about it.
			"a grant on an object the person may not touch",
			scopedRequest(otherSubject, authorizer.ActionModelUpdate, v1.KindModel, here, grantOn(here, authorizer.ActionModelUpdate)),
			admins,
		},
		{
			// The claim is read on a personal access token alone.
			"another credential class carrying the same claim",
			func() authz.Request {
				req := scopedRequest(adminSubject, authorizer.ActionModelUpdate, v1.KindModel, here, read)
				req.Claims["token_use"] = "access"
				return req
			}(),
			allow,
		},
		{
			// An absent claim is not full access: a personal token
			// nobody wrote a grant for reaches nothing.
			"a personal token carrying no grant at all",
			scopedRequest(adminSubject, authorizer.ActionModelRead, v1.KindModel, here),
			verdict{reason: authz.ReasonGrant},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := p.Authorize(t.Context(), tc.req)
			if err != nil {
				t.Fatalf("%s on %s: %v", tc.req.Action, tc.req.Resource.ID, err)
			}
			if d.Allow != tc.want.allow || d.Reason != tc.want.reason {
				t.Errorf("%s on %s = {allow:%v reason:%q}, want {allow:%v reason:%q}",
					tc.req.Action, tc.req.Resource.ID, d.Allow, d.Reason, tc.want.allow, tc.want.reason)
			}
		})
	}
}

// decider adapts an authz.Authorizer to the scaffold's Decider, which
// names the same call: the owner policy answers a request from the
// gateway's own state, and a deny is a decision and never an error.
type decider struct{ a authz.Authorizer }

func (d decider) Decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	return d.a.Authorize(ctx, req)
}

// TestOwnerPolicyConforms holds the owner policy to the contract every
// authorizer of the family passes, driven from the declared table rather
// than from a list written out here: the probe is denied for every
// subject and action, a wrong bearer and no bearer are refused, an
// action outside the table is a 400, every well-formed request is
// answered with a decision of the contract's shape, and a personal
// access token is answered inside the grants it carries.
//
// The subject the grant case uses is an admin here, so the policy allows
// every row and the intersection is the only thing that can move the
// answer: a grant qualified by another core would deny the action its
// own grant names, and the suite reads that as the endpoint refusing
// what the credential was written for.
//
// The scaffold applies the intersection too, so this run proves the
// endpoint and not the policy alone; TestOwnerPolicyRestrictsToTheTokensGrants
// is the same case against the policy in process, where nothing else
// could be answering.
func TestOwnerPolicyConforms(t *testing.T) {
	const bearer = "the-owner-policy-token"
	srv := httptest.NewServer(server.New(server.Options{
		Bearer:     bearer,
		Vocabulary: authorizer.Vocabulary(),
		Decider:    decider{&OwnerPolicy{Admins: []string{fixtureSubject}, Objects: objectsOf()}},
	}))
	t.Cleanup(srv.Close)
	conformance.Run(t, srv.URL, bearer,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(fixtureSubject, otherSubject),
		conformance.WithHTTPClient(&http.Client{}))
}
