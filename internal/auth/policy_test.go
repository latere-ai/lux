// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

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
		{"provider.create", ActionProviderCreate, ProviderCreate(provider), admins, admins},
		{"provider.create of a tunnel", ActionProviderCreate, ProviderCreate(tunnelled), allow, allow},
		{"provider.read", ActionProviderRead, ProviderObject(provider), allow, allow},
		{"provider.update", ActionProviderUpdate, ProviderObject(provider), admins, admins},
		{"provider.update of a tunnel", ActionProviderUpdate, ProviderObject(tunnelled), allow, notOwner},
		{"provider.delete", ActionProviderDelete, ProviderObject(provider), admins, admins},
		{"provider.delete of a tunnel", ActionProviderDelete, ProviderObject(tunnelled), allow, notOwner},
		{"provider.tunnel", ActionProviderTunnel, ProviderObject(tunnelled), allow, notOwner},
		{"provider.list", ActionProviderList, ProviderList(), allow, allow},
		{"model.create", ActionModelCreate, ModelCreate(model), admins, admins},
		{"model.read", ActionModelRead, ModelObject(model), allow, allow},
		{"model.update", ActionModelUpdate, ModelObject(model), admins, admins},
		{"model.delete", ActionModelDelete, ModelObject(model), admins, admins},
		{"model.list", ActionModelList, ModelList(), allow, allow},
		{"model.use", ActionModelUse, ModelUse("anthropic/*", fixtureMatched), allow, allow},
		{"key.create", ActionKeyCreate, KeyCreate(key), allow, allow},
		{"key.read", ActionKeyRead, KeyObject(key), allow, notOwner},
		{"key.read of an unknown id", ActionKeyRead, KeyObject(unknownKey), notOwner, notOwner},
		{"key.update", ActionKeyUpdate, KeyObject(key), allow, notOwner},
		{"key.delete", ActionKeyDelete, KeyObject(key), allow, notOwner},
		{"key.list", ActionKeyList, KeyList(), filtered, filtered},
		{"budget.create", ActionBudgetCreate, BudgetCreate(budget), allow, allow},
		{"budget.read", ActionBudgetRead, BudgetObject(budget), allow, notOwner},
		{"budget.update", ActionBudgetUpdate, BudgetObject(budget), allow, notOwner},
		{"budget.delete", ActionBudgetDelete, BudgetObject(budget), allow, notOwner},
		{"budget.delete of an unknown id", ActionBudgetDelete, BudgetObject(unknownBudget), notOwner, notOwner},
		{"budget.list", ActionBudgetList, BudgetList(), filtered, filtered},
		{"budget.draw", ActionBudgetDraw, BudgetObject(budget), allow, notOwner},
		{"usage.read", ActionUsageRead, UsageRead(nil, nil), filtered, filtered},
	}
	covered := map[string]bool{}
	for _, r := range rows {
		covered[r.action] = true
	}
	for _, a := range Actions() {
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
		_, err := z.Decide(t.Context(), bob, ActionKeyRead, KeyObject(key), info)
		if e := wantCode(t, err, CodeForbidden); !strings.HasSuffix(e.Detail, authz.ReasonNotOwner) {
			t.Errorf("detail %q", e.Detail)
		}
		d, err := z.Decide(t.Context(), bob, ActionKeyList, KeyList(), info)
		if err != nil || !reflect.DeepEqual(d.Filter, &authz.Filter{Owners: []string{otherSubject}}) || d.Limits != (Limits{}) {
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
		check(t, adminSubject, Request(c, "key.rotate", KeyObject(key), info), verdict{reason: ReasonUnknownAction})
	})

	t.Run("a lookup that fails is no decision", func(t *testing.T) {
		broken := &OwnerPolicy{Objects: &catalog{err: errors.New("store: timeout")}}
		c := Caller{Subject: fixtureSubject, Issuer: fixtureIssuer, Sub: "alice"}
		if _, err := broken.Authorize(t.Context(), Request(c, ActionKeyRead, KeyObject(key), info)); err == nil || !strings.Contains(err.Error(), "store: timeout") {
			t.Errorf("err = %v", err)
		}
		_, err := NewAuthorizer(broken).Decide(t.Context(), c, ActionKeyRead, KeyObject(key), info)
		wantCode(t, err, CodeAuthorizerUnavailable)
		// A row that needs no object still answers.
		if d, err := broken.Authorize(t.Context(), Request(c, ActionKeyCreate, KeyCreate(key), info)); err != nil || !d.Allow {
			t.Errorf("key.create = %+v, %v", d, err)
		}
	})

	t.Run("no lookup at all", func(t *testing.T) {
		none := &OwnerPolicy{}
		c := Caller{Subject: fixtureSubject, Issuer: fixtureIssuer, Sub: "alice"}
		if _, err := none.Authorize(t.Context(), Request(c, ActionKeyRead, KeyObject(key), info)); err == nil || !strings.Contains(err.Error(), "no object lookup") {
			t.Errorf("err = %v", err)
		}
		// An object named by no id is denied without a lookup.
		d, err := none.Authorize(t.Context(), Request(c, ActionKeyRead, authz.NewResource(v1.KindKey, "", nil), info))
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
	if _, err := a.Authorize(context.Background(), authz.Probe(ActionKeyRead, v1.KindKey)); err != nil {
		t.Fatal(err)
	}
}
