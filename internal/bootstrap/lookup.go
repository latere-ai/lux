// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// catalog is the manifest.Lookup of one run: the objects this run has
// already resolved, by name and by id, before the store. A dry run
// writes nothing, so a Model resolves against a Provider of the same
// directory only through the objects the run holds, and an apply reads
// the same way so the two cannot disagree. Every reference is allowed,
// because no authorizer is asked: there is no caller whose access could
// be decided. A reference that names nothing is not_found at the field,
// the answer /v1 gives for it.
type catalog struct {
	objects   store.Objects
	providers map[string]*v1.Provider // by name and by id
	budgets   map[string]*v1.Budget   // by name and by id
	models    map[string]*v1.Model    // by name
	hashes    map[string]string       // a created Key's hash to its name
}

func newCatalog(objects store.Objects) *catalog {
	return &catalog{
		objects:   objects,
		providers: map[string]*v1.Provider{},
		budgets:   map[string]*v1.Budget{},
		models:    map[string]*v1.Model{},
		hashes:    map[string]string{},
	}
}

// add files a resolved object where the later kinds look it up.
func (c *catalog) add(obj v1.Object) {
	switch x := obj.(type) {
	case *v1.Provider:
		c.providers[x.Metadata.Name], c.providers[x.Status.ID] = x, x
	case *v1.Budget:
		c.budgets[x.Metadata.Name], c.budgets[x.Status.ID] = x, x
	case *v1.Model:
		c.models[x.Metadata.Name] = x
	}
}

// Provider implements manifest.Lookup.
func (c *catalog) Provider(ctx context.Context, nameOrID string) (*v1.Provider, error) {
	if p, ok := c.providers[nameOrID]; ok {
		return p, nil
	}
	obj, err := c.find(ctx, v1.KindProvider, v1.PrefixProvider, nameOrID)
	if err != nil {
		return nil, err
	}
	p, ok := obj.(*v1.Provider)
	if !ok {
		return nil, fmt.Errorf("Provider %q: the store holds a %T under the reference", nameOrID, obj)
	}
	return p, nil
}

// Budget implements manifest.Lookup.
func (c *catalog) Budget(ctx context.Context, nameOrID string) (*v1.Budget, error) {
	if b, ok := c.budgets[nameOrID]; ok {
		return b, nil
	}
	obj, err := c.find(ctx, v1.KindBudget, v1.PrefixBudget, nameOrID)
	if err != nil {
		return nil, err
	}
	b, ok := obj.(*v1.Budget)
	if !ok {
		return nil, fmt.Errorf("Budget %q: the store holds a %T under the reference", nameOrID, obj)
	}
	return b, nil
}

// Models implements manifest.Lookup: every live Model of the store and
// of this run the selector matches, the run's in place of the store's of
// the same name, as the API's catalog answers from the store alone.
func (c *catalog) Models(ctx context.Context, selector string) ([]v1.ModelRef, error) {
	objs, _, err := c.objects.List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	if err != nil {
		return nil, fmt.Errorf("listing the Models for %q: %w", selector, err)
	}
	matched := map[string]*v1.Model{}
	for _, o := range objs {
		if m, ok := o.(*v1.Model); ok && manifest.Match(selector, m.Metadata.Name) {
			matched[m.Metadata.Name] = m
		}
	}
	for name, m := range c.models {
		if manifest.Match(selector, name) {
			matched[name] = m
		}
	}
	refs := make([]v1.ModelRef, 0, len(matched))
	for _, name := range slices.Sorted(maps.Keys(matched)) {
		m := matched[name]
		refs = append(refs, v1.ModelRef{ID: m.Status.ID, Name: name, Owner: m.Status.Owner, Labels: maps.Clone(m.Metadata.Labels)})
	}
	return refs, nil
}

// find reads one object of the store by id when nameOrID carries the
// kind's prefix and by name otherwise; one that does not exist is
// not_found, and any other error is the store's own failure.
func (c *catalog) find(ctx context.Context, kind, prefix, nameOrID string) (v1.Object, error) {
	var (
		obj v1.Object
		err error
	)
	if strings.HasPrefix(nameOrID, prefix) {
		obj, _, err = c.objects.Get(ctx, kind, nameOrID)
	} else {
		obj, _, err = c.objects.ByName(ctx, kind, nameOrID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, &manifest.Error{Code: manifest.CodeNotFound, Message: manifest.CodeNotFound.Message(), Detail: "no " + kind + " named " + strconv.Quote(nameOrID) + " is in the store or the bootstrap directory"}
	}
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", kind, nameOrID, err)
	}
	return obj, nil
}
