// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// references is the auth.Catalog of one apply over the store: the
// objects a manifest may name, by name or by id, and the Models a
// selector matches now. It remembers the Budget it answered so the
// Key's status.budget carries the id beside the name.
type references struct {
	objects store.Objects
	budget  *v1.Budget
}

// Provider implements auth.Catalog.
func (r *references) Provider(ctx context.Context, nameOrID string) (*v1.Provider, error) {
	obj, err := r.find(ctx, v1.KindProvider, v1.PrefixProvider, nameOrID)
	if err != nil || obj == nil {
		return nil, err
	}
	p, ok := obj.(*v1.Provider)
	if !ok {
		return nil, fmt.Errorf("Provider %q: the store holds a %T under the reference", nameOrID, obj)
	}
	return p, nil
}

// Budget implements auth.Catalog.
func (r *references) Budget(ctx context.Context, nameOrID string) (*v1.Budget, error) {
	obj, err := r.find(ctx, v1.KindBudget, v1.PrefixBudget, nameOrID)
	if err != nil || obj == nil {
		return nil, err
	}
	b, ok := obj.(*v1.Budget)
	if !ok {
		return nil, fmt.Errorf("Budget %q: the store holds a %T under the reference", nameOrID, obj)
	}
	r.budget = b
	return b, nil
}

// Models implements auth.Catalog: every live Model the selector matches
// under spec 003's glob.
func (r *references) Models(ctx context.Context, selector string) ([]*v1.Model, error) {
	objs, _, err := r.objects.List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	if err != nil {
		return nil, fmt.Errorf("listing the Models for %q: %w", selector, err)
	}
	var out []*v1.Model
	for _, o := range objs {
		if m, ok := o.(*v1.Model); ok && manifest.Match(selector, m.Metadata.Name) {
			out = append(out, m)
		}
	}
	return out, nil
}

// find reads one object by id when nameOrID carries the kind's prefix
// and by name otherwise; nil and no error for one that does not exist.
func (r *references) find(ctx context.Context, kind, prefix, nameOrID string) (v1.Object, error) {
	if nameOrID == "" {
		return nil, nil
	}
	var obj v1.Object
	var err error
	if strings.HasPrefix(nameOrID, prefix) {
		obj, _, err = r.objects.Get(ctx, kind, nameOrID)
	} else {
		obj, _, err = r.objects.ByName(ctx, kind, nameOrID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", kind, nameOrID, err)
	}
	return obj, nil
}
