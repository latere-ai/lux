// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Catalog is desired state as the doors read it, over the store: spec
// 004's gateway.Catalog under spec 008's resolution rules. A Model is
// found by its exact name and by nothing else: no prefix is stripped, no
// alias table is consulted, a discovered <provider>/<upstream> name is
// looked up whole however many slashes the upstream name carries, and a
// mdl_ id is not a name, so a body naming one is model_not_found at the
// door. A Provider is found by name or by prv_ id, which are the two
// ways a target names one. Every object comes from the store's read
// path, which carries no credential value in any encoding.
type Catalog struct {
	Objects store.Objects
}

// Model returns the Model named exactly name, declared or discovered,
// nil for a name that is no Model's, and the store's own failure.
func (c *Catalog) Model(ctx context.Context, name string) (*v1.Model, error) {
	if name == "" || isID(name) {
		return nil, nil
	}
	obj, _, err := c.Objects.ByName(ctx, v1.KindModel, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Model %q: %w", name, err)
	}
	m, ok := obj.(*v1.Model)
	if !ok {
		return nil, fmt.Errorf("Model %q: the store holds a %T under the name", name, obj)
	}
	return m, nil
}

// Models returns every live Model, by name ascending, for the list a
// door renders to the Key's selectors.
func (c *Catalog) Models(ctx context.Context) ([]*v1.Model, error) {
	objs, _, err := c.Objects.List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	if err != nil {
		return nil, fmt.Errorf("listing the Models: %w", err)
	}
	out := make([]*v1.Model, 0, len(objs))
	for _, o := range objs {
		m, ok := o.(*v1.Model)
		if !ok {
			return nil, fmt.Errorf("listing the Models: a %T among them", o)
		}
		out = append(out, m)
	}
	return out, nil
}

// Provider returns the Provider nameOrID names, by prv_ id when it
// carries the prefix and by name otherwise, nil for one that does not
// exist, and the store's own failure.
func (c *Catalog) Provider(ctx context.Context, nameOrID string) (*v1.Provider, error) {
	if nameOrID == "" {
		return nil, nil
	}
	var obj v1.Object
	var err error
	if strings.HasPrefix(nameOrID, v1.PrefixProvider) {
		obj, _, err = c.Objects.Get(ctx, v1.KindProvider, nameOrID)
	} else {
		obj, _, err = c.Objects.ByName(ctx, v1.KindProvider, nameOrID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Provider %q: %w", nameOrID, err)
	}
	p, ok := obj.(*v1.Provider)
	if !ok {
		return nil, fmt.Errorf("Provider %q: the store holds a %T under the reference", nameOrID, obj)
	}
	return p, nil
}

// isID reports whether s begins with one of the four kinds' id prefixes,
// which no name of any kind may (spec 003), so it names nothing and is
// answered without a read.
func isID(s string) bool {
	for _, prefix := range v1.KindPrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// The seam: the doors take the Catalog through spec 004's interface.
var _ gateway.Catalog = (*Catalog)(nil)
