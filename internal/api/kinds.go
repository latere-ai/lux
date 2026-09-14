// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// kind is one row of the route table's four: the path segment, the
// kind, its id prefix, and the five actions its routes ask.
type kind struct {
	plural                          string
	name                            string
	prefix                          string
	create, read, update, del, list string
}

// kinds are the four, in the route table's order.
var kinds = []kind{
	{"providers", v1.KindProvider, v1.PrefixProvider, authorizer.ActionProviderCreate, authorizer.ActionProviderRead, authorizer.ActionProviderUpdate, authorizer.ActionProviderDelete, authorizer.ActionProviderList},
	{"models", v1.KindModel, v1.PrefixModel, authorizer.ActionModelCreate, authorizer.ActionModelRead, authorizer.ActionModelUpdate, authorizer.ActionModelDelete, authorizer.ActionModelList},
	{"keys", v1.KindKey, v1.PrefixKey, authorizer.ActionKeyCreate, authorizer.ActionKeyRead, authorizer.ActionKeyUpdate, authorizer.ActionKeyDelete, authorizer.ActionKeyList},
	{"budgets", v1.KindBudget, v1.PrefixBudget, authorizer.ActionBudgetCreate, authorizer.ActionBudgetRead, authorizer.ActionBudgetUpdate, authorizer.ActionBudgetDelete, authorizer.ActionBudgetList},
}

// kindOf is the row of a kind name.
func kindOf(name string) kind {
	for _, k := range kinds {
		if k.name == name {
			return k
		}
	}
	panic("api: no kind " + name)
}

// hasKindPrefix reports whether s begins with any kind's id prefix,
// which no name may (spec 003), so s is an id or names nothing.
func hasKindPrefix(s string) bool {
	for _, p := range v1.KindPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// modelName is spec 003's name rule for a Model, checked on the path a
// model route rejoined: segments of [a-z0-9._:-] beginning and ending
// alphanumeric, joined by /, at most 128 characters. A rejoined name
// that fails it names nothing.
var modelSegment = regexp.MustCompile(`^[a-z0-9]([a-z0-9._:-]*[a-z0-9])?$`)

func validModelName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for seg := range strings.SplitSeq(s, "/") {
		if !modelSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// load resolves {id-or-name} for the kind: an id when the segment
// carries the kind's prefix, not_found when it carries another kind's,
// a name otherwise. A missing object is not_found; nil and no error is
// never returned.
func (c *call) load(ctx context.Context, k kind, ref string) (v1.Object, int64, *Error) {
	if ref == "" || (k.name == v1.KindModel && !hasKindPrefix(ref) && !validModelName(ref)) {
		return nil, 0, refuse(CodeNotFound, "the path names no "+k.name)
	}
	var (
		obj     v1.Object
		version int64
		err     error
	)
	switch {
	case strings.HasPrefix(ref, k.prefix):
		obj, version, err = c.h.o.Store.Objects().Get(ctx, k.name, ref)
	case hasKindPrefix(ref):
		return nil, 0, refuse(CodeNotFound, strconv.Quote(ref)+" is an id of another kind, and no "+k.name+" has it")
	default:
		obj, version, err = c.h.o.Store.Objects().ByName(ctx, k.name, ref)
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, 0, refuse(CodeNotFound, "no "+k.name+" "+strconv.Quote(ref))
	}
	if err != nil {
		return nil, 0, mapError(err)
	}
	return obj, version, nil
}

// loadByName is the apply's read of the existing object: the object of
// the kind and name, or nil and version 0 when none is live. A
// discovered Model does not count as existing, because a declared one
// replaces it in place as a create (spec 010).
func (c *call) loadByName(ctx context.Context, k kind, name string) (v1.Object, int64, *Error) {
	obj, version, err := c.h.o.Store.Objects().ByName(ctx, k.name, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, mapError(err)
	}
	if m, ok := obj.(*v1.Model); ok && m.Status.Source == v1.SourceDiscovered {
		return nil, 0, nil
	}
	return obj, version, nil
}
