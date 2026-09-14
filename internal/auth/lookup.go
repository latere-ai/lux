// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"strconv"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Catalog is what a Lookup reads from the store: the objects a manifest
// may reference, by name or by id, and the Models a selector matches
// now. A reference that names nothing is nil and no error; an error is
// the store's own failure.
type Catalog interface {
	Provider(ctx context.Context, nameOrID string) (*v1.Provider, error)
	Budget(ctx context.Context, nameOrID string) (*v1.Budget, error)
	// Models lists every live Model the selector matches, by
	// manifest.Match over the Models' names.
	Models(ctx context.Context, selector string) ([]*v1.Model, error)
}

// Lookup is manifest.Lookup for one request: it reads the catalog and
// asks the authorizer provider.read for a target, budget.draw for a
// budget, and model.use for a selector, as the caller. A deny and a
// reference that names nothing are one answer, not_found with one
// detail, so a manifest cannot probe for another subject's objects; a
// call that produced no decision is authorizer_unavailable. The shared
// client caches provider.read and budget.draw by the object's id and
// never model.use, whose resource has none.
type Lookup struct {
	z       *Authorizer
	caller  Caller
	info    authz.Caller
	catalog Catalog
}

// Lookup builds the Lookup manifest.Resolve asks for one request.
func (z *Authorizer) Lookup(c Caller, info authz.Caller, catalog Catalog) *Lookup {
	return &Lookup{z: z, caller: c, info: info, catalog: catalog}
}

// Provider is the provider.read decision for one target's Provider.
func (l *Lookup) Provider(ctx context.Context, nameOrID string) (*v1.Provider, error) {
	detail := "no Provider named " + strconv.Quote(nameOrID) + " is readable by the caller"
	p, err := l.catalog.Provider(ctx, nameOrID)
	if err != nil {
		return nil, fmt.Errorf("catalog: Provider %q: %w", nameOrID, err)
	}
	if p == nil {
		return nil, notFound(detail)
	}
	if err := l.allow(ctx, ActionProviderRead, ProviderObject(p), detail); err != nil {
		return nil, err
	}
	return p, nil
}

// Budget is the budget.draw decision for a Key's Budget.
func (l *Lookup) Budget(ctx context.Context, nameOrID string) (*v1.Budget, error) {
	detail := "no Budget named " + strconv.Quote(nameOrID) + " is drawable by the caller"
	b, err := l.catalog.Budget(ctx, nameOrID)
	if err != nil {
		return nil, fmt.Errorf("catalog: Budget %q: %w", nameOrID, err)
	}
	if b == nil {
		return nil, notFound(detail)
	}
	if err := l.allow(ctx, ActionBudgetDraw, BudgetObject(b), detail); err != nil {
		return nil, err
	}
	return b, nil
}

// Models is the model.use decision for one selector: the Models it
// matches now when the caller may use it, or the refusal of the selector
// as a whole. The question is asked whether or not the selector matches
// anything, because the decision binds the selector and a Model declared
// later will match it.
func (l *Lookup) Models(ctx context.Context, selector string) ([]v1.ModelRef, error) {
	models, err := l.catalog.Models(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("catalog: selector %q: %w", selector, err)
	}
	refs := make([]v1.ModelRef, 0, len(models))
	for _, m := range models {
		refs = append(refs, v1.ModelRef{ID: m.Status.ID, Name: m.Metadata.Name, Owner: m.Status.Owner})
	}
	detail := "selector " + strconv.Quote(selector) + " names no Model the caller may use"
	if err := l.allow(ctx, ActionModelUse, ModelUse(selector, refs), detail); err != nil {
		return nil, err
	}
	return refs, nil
}

// allow asks one reference's action and returns nil on an allow,
// not_found with the given detail on a deny, and authorizer_unavailable
// when no decision could be had.
func (l *Lookup) allow(ctx context.Context, action string, res authz.Resource, detail string) error {
	d, err := l.z.ask(ctx, l.caller, action, res, l.info)
	if err != nil {
		return &manifest.Error{Code: manifest.CodeAuthorizerUnavailable, Message: manifest.CodeAuthorizerUnavailable.Message(), Detail: err.Error()}
	}
	if !d.Allow {
		return notFound(detail)
	}
	return nil
}

// notFound is the one refusal a reference gets, refused or missing.
func notFound(detail string) *manifest.Error {
	return &manifest.Error{Code: manifest.CodeNotFound, Message: manifest.CodeNotFound.Message(), Detail: detail}
}
