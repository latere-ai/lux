// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// catalog is a Catalog over a few objects, matching a selector with the
// contract's own glob.
type catalog struct {
	objects []v1.Object
	err     error
}

func catalogOf(objs ...v1.Object) *catalog { return &catalog{objects: objs} }

func (c *catalog) find(kind, nameOrID string) v1.Object {
	for _, o := range c.objects {
		if o.Kind() == kind && (o.Name() == nameOrID || o.ID() == nameOrID) {
			return o
		}
	}
	return nil
}

func (c *catalog) Provider(_ context.Context, nameOrID string) (*v1.Provider, error) {
	if c.err != nil {
		return nil, c.err
	}
	p, _ := c.find(v1.KindProvider, nameOrID).(*v1.Provider)
	return p, nil
}

func (c *catalog) Budget(_ context.Context, nameOrID string) (*v1.Budget, error) {
	if c.err != nil {
		return nil, c.err
	}
	b, _ := c.find(v1.KindBudget, nameOrID).(*v1.Budget)
	return b, nil
}

func (c *catalog) Models(_ context.Context, selector string) ([]*v1.Model, error) {
	if c.err != nil {
		return nil, c.err
	}
	var out []*v1.Model
	for _, o := range c.objects {
		if m, ok := o.(*v1.Model); ok && manifest.Match(selector, m.Metadata.Name) {
			out = append(out, m)
		}
	}
	return out, nil
}

// objectsOf is an ObjectLookup over the same objects.
func objectsOf(objs ...v1.Object) *catalog { return catalogOf(objs...) }

func (c *catalog) Object(_ context.Context, kind, id string) (authz.Object, error) {
	if c.err != nil {
		return authz.Object{}, c.err
	}
	o := c.find(kind, id)
	if o == nil {
		return authz.Object{}, nil
	}
	return authz.Object{Exists: true, Owner: o.Owner()}, nil
}

// fixtures is the catalog every Lookup test resolves against.
func fixtures(t *testing.T) *catalog {
	t.Helper()
	claude := &v1.Model{Metadata: v1.ObjectMeta{Name: "anthropic/claude"}, Status: v1.ModelStatus{ID: "mdl_01J9TESTCLAUDE0000000000000", Owner: fixtureSubject}}
	return catalogOf(fixtureProvider(), fixtureModel(), claude, fixtureBudget(t))
}

// keyManifest is a Key naming one selector and the Budget; keySelector
// names the selector alone.
func keyManifest() *v1.Key {
	return &v1.Key{Metadata: v1.ObjectMeta{Name: "run-42"}, Spec: v1.KeySpec{Models: []string{"anthropic/*"}, Budget: "team-research"}}
}

func keySelector() *v1.Key {
	return &v1.Key{Metadata: v1.ObjectMeta{Name: "run-42"}, Spec: v1.KeySpec{Models: []string{"anthropic/*"}}}
}

// modelManifest is a Model targeting the Provider.
func modelManifest() *v1.Model {
	return &v1.Model{Metadata: v1.ObjectMeta{Name: "gpt-5"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: "team-openai", Model: "gpt-5"}}}}
}

// wantManifestCode asserts err is a *manifest.Error of the code at the
// path.
func wantManifestCode(t *testing.T, err error, code manifest.Code, path string) *manifest.Error {
	t.Helper()
	var e *manifest.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	if path != "" && !reflect.DeepEqual(e.Paths, []string{path}) {
		t.Errorf("paths %v, want [%s]", e.Paths, path)
	}
	if e.Message != code.Message() {
		t.Errorf("message %q is not the fixed sentence", e.Message)
	}
	return e
}

// TestLookupDenyIsNotFound: through manifest.Resolve, a deny on
// model.use, on budget.draw, or on a target's provider.read is not_found
// naming the field, with the same detail a missing object gets, so a
// refused object and a missing one are one answer; an allow resolves.
func TestLookupDenyIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
		in     v1.Object
		path   string
	}{
		{"model.use on a Key's selector", ActionModelUse, keySelector(), "spec.models[0]"},
		{"budget.draw on a Key's budget", ActionBudgetDraw, keyManifest(), "spec.budget"},
		{"provider.read on a Model's target", ActionProviderRead, modelManifest(), "spec.targets[0].provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each phase runs over a fresh client, because the shared client
			// holds a deny for five seconds and an allow for sixty.
			s, z := newStubAuthorizer(t)
			s.Deny(stub.Rule{Action: tc.action}, "not in your plan")
			opts := manifest.Options{Actor: manifest.Actor{Subject: fixtureSubject}, Lookup: z.Lookup(alice, info, fixtures(t))}
			_, err := manifest.Resolve(t.Context(), tc.in, opts)
			denied := wantManifestCode(t, err, manifest.CodeNotFound, tc.path)
			if denied.Detail == "" || contains(denied.Detail, "plan") {
				t.Errorf("detail %q carries the authorizer's reason or nothing", denied.Detail)
			}

			// The same reference against an empty catalog, allowed: the
			// same code and the same detail.
			_, z = newStubAuthorizer(t)
			opts.Lookup = z.Lookup(alice, info, catalogOf())
			_, err = manifest.Resolve(t.Context(), tc.in, opts)
			if tc.action == ActionModelUse {
				// A selector matching nothing is a warning, not a refusal.
				if err != nil {
					t.Fatalf("an empty catalog refused the selector: %v", err)
				}
			} else if missing := wantManifestCode(t, err, manifest.CodeNotFound, tc.path); missing.Detail != denied.Detail {
				t.Errorf("a missing object reads %q and a refused one %q; they disclose which is which", missing.Detail, denied.Detail)
			}

			// Allowed, the manifest resolves and the Key records its matches.
			_, z = newStubAuthorizer(t)
			opts.Lookup = z.Lookup(alice, info, fixtures(t))
			out, err := manifest.Resolve(t.Context(), tc.in, opts)
			if err != nil {
				t.Fatalf("allowed: %v", err)
			}
			if k, ok := out.Object.(*v1.Key); ok && !reflect.DeepEqual(k.Status.Selectors, []v1.SelectorStatus{{Selector: "anthropic/*", Matched: []string{"anthropic/claude"}}}) {
				t.Errorf("selectors %+v", k.Status.Selectors)
			}
		})
	}
	t.Run("the questions carry the table's resources", func(t *testing.T) {
		s, z := newStubAuthorizer(t)
		l := z.Lookup(alice, info, fixtures(t))
		if _, err := l.Provider(t.Context(), "prv_01J9TESTPROVIDER0000000000"); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Budget(t.Context(), "team-research"); err != nil {
			t.Fatal(err)
		}
		refs, err := l.Models(t.Context(), "anthropic/*")
		if err != nil || !reflect.DeepEqual(refs, []v1.ModelRef{{ID: "mdl_01J9TESTCLAUDE0000000000000", Name: "anthropic/claude", Owner: fixtureSubject}}) {
			t.Fatalf("Models() = %+v, %v", refs, err)
		}
		want := shapes(t)
		reqs := s.Requests()
		if len(reqs) != 3 {
			t.Fatalf("%d requests", len(reqs))
		}
		for i, tc := range []struct{ action string }{{ActionProviderRead}, {ActionBudgetDraw}, {ActionModelUse}} {
			if reqs[i].Action != tc.action || reqs[i].Subject != fixtureSubject || reqs[i].Request != info {
				t.Errorf("request %d: %+v", i, reqs[i])
			}
		}
		if got := flat(t, reqs[0].Resource); !reflect.DeepEqual(got, want[ActionProviderRead]) {
			t.Errorf("provider.read resource %v", got)
		}
		if got := flat(t, reqs[1].Resource); !reflect.DeepEqual(got, want[ActionBudgetDraw]) {
			t.Errorf("budget.draw resource %v", got)
		}
		if got := flat(t, reqs[2].Resource); got["selector"] != "anthropic/*" || len(got["matched"].([]any)) != 1 {
			t.Errorf("model.use resource %v", got)
		}
	})
	t.Run("a catalog failure is no decision", func(t *testing.T) {
		_, z := newStubAuthorizer(t)
		broken := &catalog{err: errors.New("store: connection reset")}
		l := z.Lookup(alice, info, broken)
		for _, ask := range []func() error{
			func() error { _, err := l.Provider(t.Context(), "team-openai"); return err },
			func() error { _, err := l.Budget(t.Context(), "team-research"); return err },
			func() error { _, err := l.Models(t.Context(), "*"); return err },
		} {
			if err := ask(); err == nil || !contains(err.Error(), "store: connection reset") {
				t.Errorf("err = %v", err)
			}
		}
		_, err := manifest.Resolve(t.Context(), keyManifest(), manifest.Options{Actor: manifest.Actor{Subject: fixtureSubject}, Lookup: l})
		wantManifestCode(t, err, manifest.CodeAuthorizerUnavailable, "spec.budget")
	})
}

// TestNoIdIsNeverCached: model.use has no resource id, so ten applies of
// a Key naming one selector send ten model.use calls, while budget.draw
// on the same Key is asked once and answered from the cache after.
func TestNoIdIsNeverCached(t *testing.T) {
	s, z := newStubAuthorizer(t)
	opts := manifest.Options{Actor: manifest.Actor{Subject: fixtureSubject}, Lookup: z.Lookup(alice, info, fixtures(t))}
	for range 10 {
		if _, err := manifest.Resolve(t.Context(), keyManifest(), opts); err != nil {
			t.Fatal(err)
		}
	}
	uses, draws := 0, 0
	for _, r := range s.Requests() {
		switch r.Action {
		case ActionModelUse:
			uses++
		case ActionBudgetDraw:
			draws++
		}
	}
	if uses != 10 || draws != 1 {
		t.Errorf("ten applies sent %d model.use and %d budget.draw calls; want 10 and 1", uses, draws)
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
