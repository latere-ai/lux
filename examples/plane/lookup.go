// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// lookup is what manifest.Resolve asks about the objects a manifest
// names, scoped to the caller: the Provider a target names, the Budget
// a Key draws from, and the Models a selector matches. A reference the
// authorizer refuses comes back as not found, so a manifest cannot
// probe for objects another tenant owns, and a decision that could not
// be made comes back as an error, which Resolve passes through.
type lookup struct {
	c *call
	// budget is the Budget a Key resolved through, kept so the Key's
	// status.budget names it by id beside its name.
	budget *v1.Budget
}

func (c *call) lookup() *lookup { return &lookup{c: c} }

// Provider implements manifest.Lookup.
func (l *lookup) Provider(ctx context.Context, nameOrID string) (*v1.Provider, error) {
	p, err := l.c.p.store.Provider(ctx, nameOrID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, manifest.ErrNotFound
	}
	allow, err := l.allowed(ctx, authorizer.ActionProviderRead, resourceFor(v1.KindProvider, p))
	if err != nil || !allow {
		return nil, err
	}
	return p, nil
}

// Budget implements manifest.Lookup.
func (l *lookup) Budget(ctx context.Context, nameOrID string) (*v1.Budget, error) {
	b, err := l.c.p.store.Budget(ctx, nameOrID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, manifest.ErrNotFound
	}
	allow, err := l.allowed(ctx, authorizer.ActionBudgetDraw, resourceFor(v1.KindBudget, b))
	if err != nil || !allow {
		return nil, err
	}
	l.budget = b
	return b, nil
}

// Models implements manifest.Lookup: the decision binds the selector,
// so it is asked once per selector, one that matches nothing included.
func (l *lookup) Models(ctx context.Context, selector string) ([]v1.ModelRef, error) {
	var matched []v1.ModelRef
	for _, obj := range l.c.p.store.list(v1.KindModel) {
		if m, ok := obj.(*v1.Model); ok && manifest.Match(selector, m.Metadata.Name) {
			matched = append(matched, v1.ModelRef{ID: m.Status.ID, Name: m.Metadata.Name, Owner: m.Status.Owner})
		}
	}
	res := authz.NewResource(v1.KindModel, "", map[string]any{"selector": selector, "matched": matched, "labels": map[string]string{}})
	allow, err := l.allowed(ctx, authorizer.ActionModelUse, res)
	if err != nil || !allow {
		return nil, err
	}
	return matched, nil
}

// allowed asks the platform's endpoint and turns a deny into not found,
// which is the one answer a refused and a missing reference share.
func (l *lookup) allowed(ctx context.Context, action string, res authz.Resource) (bool, error) {
	d, err := l.c.p.decide(ctx, l.c.caller, action, res, authz.Caller{ID: l.c.id, IP: l.c.r.RemoteAddr, UserAgent: l.c.r.UserAgent()})
	if err != nil {
		return false, err
	}
	if !d.Allow {
		return false, manifest.ErrNotFound
	}
	return true, nil
}
