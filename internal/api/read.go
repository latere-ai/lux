// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// read is GET /v1/{kind}s/{id-or-name}: load, authorize the kind's read
// on what was loaded, render, answer with the ETag.
func (c *call) read(ctx context.Context, k kind, ref string) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	obj, version, err := c.load(ctx, k, ref)
	if err != nil {
		return err
	}
	if !c.h.fileMode() {
		res, _ := auth.ResourceFor(k.read, obj)
		if _, err := c.authorize(ctx, k.read, res); err != nil {
			return err
		}
	}
	if err := c.render(ctx, obj); err != nil {
		return err
	}
	return c.writeJSON(http.StatusOK, obj, version)
}

// The list parameters of spec 011 and their defaults.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// listResponse is {"items": [...], "next_cursor": "..."}, the member
// absent on the last page.
type listResponse struct {
	Items      []v1.Object `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// list is GET /v1/{kind}s: the query parsed strictly, the kind's list
// action asked for its filter, the selectors intersected with it, and
// the page rendered.
func (c *call) list(ctx context.Context, k kind) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	q, err := parseListQuery(k, c.r.URL.Query())
	if err != nil {
		return err
	}
	var filter *authz.Filter
	if !c.h.fileMode() {
		res, _ := auth.ResourceFor(k.list, zeroObject(k.name))
		d, err := c.authorize(ctx, k.list, res)
		if err != nil {
			return err
		}
		filter = d.Filter
	}
	f, owners, empty := q.intersect(filter)
	if empty {
		return c.writeJSON(http.StatusOK, listResponse{Items: []v1.Object{}}, 0)
	}
	if q.provider != "" {
		id, err := c.providerID(ctx, q.provider)
		if err != nil {
			return err
		}
		if id == "" {
			return c.writeJSON(http.StatusOK, listResponse{Items: []v1.Object{}}, 0)
		}
		f.Provider = id
	}
	items := make([]v1.Object, 0, q.limit)
	cursor := q.cursor
	var next string
	for {
		page, n, lerr := c.h.o.Store.Objects().List(ctx, k.name, f, store.Page{Limit: q.limit, Cursor: cursor})
		if lerr != nil {
			return mapError(lerr)
		}
		for _, obj := range page {
			if len(owners) > 0 && !slices.Contains(owners, obj.Owner()) {
				continue
			}
			if len(items) < q.limit {
				items = append(items, obj)
			}
		}
		next = n
		if next == "" || len(items) >= q.limit || len(owners) == 0 {
			break
		}
		cursor = next
	}
	for _, obj := range items {
		if err := c.render(ctx, obj); err != nil {
			return err
		}
	}
	return c.writeJSON(http.StatusOK, listResponse{Items: items, NextCursor: next}, 0)
}

// providerID resolves the ?provider= selector to a prv_ id: the id as
// given, or the live Provider of the name; "" when none, which is an
// empty list and never not_found.
func (c *call) providerID(ctx context.Context, ref string) (string, *Error) {
	if strings.HasPrefix(ref, v1.PrefixProvider) {
		return ref, nil
	}
	obj, _, err := c.h.o.Store.Objects().ByName(ctx, v1.KindProvider, ref)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", mapError(err)
	}
	return obj.ID(), nil
}

// listQuery is a list's parameters, parsed.
type listQuery struct {
	labels   map[string]string
	owner    string
	source   string
	provider string
	limit    int
	cursor   string
}

// parseListQuery reads the parameters the kind's route defines and
// refuses any other, or a value outside its rule, with invalid_field at
// the parameter's name.
func parseListQuery(k kind, values url.Values) (listQuery, *Error) {
	q := listQuery{labels: map[string]string{}, limit: defaultLimit}
	for name, vals := range values {
		switch name {
		case "label":
			for _, v := range vals {
				key, val, ok := strings.Cut(v, "=")
				if !ok || key == "" {
					return q, refuse(CodeInvalidField, "label "+strconv.Quote(v)+" is not k=v", "label")
				}
				if prev, dup := q.labels[key]; dup && prev != val {
					// Two values for one key match nothing.
					q.labels = nil
					continue
				}
				if q.labels != nil {
					q.labels[key] = val
				}
			}
		case "owner":
			q.owner = one(vals)
		case "limit":
			n, err := strconv.Atoi(one(vals))
			if err != nil || n < 1 || n > maxLimit {
				return q, refuse(CodeInvalidField, "limit "+strconv.Quote(one(vals))+" is not an integer from 1 to "+strconv.Itoa(maxLimit), "limit")
			}
			q.limit = n
		case "cursor":
			q.cursor = one(vals)
		case "source":
			if k.name != v1.KindModel {
				return q, refuse(CodeInvalidField, "source is a parameter of /v1/models alone", "source")
			}
			if s := v1.Source(one(vals)); !s.Valid() {
				return q, refuse(CodeInvalidField, "source "+strconv.Quote(one(vals))+" is not declared or discovered", "source")
			}
			q.source = one(vals)
		case "provider":
			if k.name != v1.KindModel {
				return q, refuse(CodeInvalidField, "provider is a parameter of /v1/models alone", "provider")
			}
			q.provider = one(vals)
		default:
			return q, refuse(CodeInvalidField, "no list route defines the parameter "+strconv.Quote(name), name)
		}
	}
	return q, nil
}

// one is the last value of a repeated parameter.
func one(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

// intersect narrows the query by the authorizer's filter: an owner
// outside the filter's owners, a label the filter contradicts, or two
// values for one label is an empty list; one owner goes to the store,
// several are the post-filter the page loop applies.
func (q listQuery) intersect(filter *authz.Filter) (f store.Filter, owners []string, empty bool) {
	if q.labels == nil {
		return f, nil, true
	}
	f = store.Filter{Owner: q.owner, Source: q.source}
	labels := map[string]string{}
	maps.Copy(labels, q.labels)
	if filter != nil {
		for k, v := range filter.Labels {
			if prev, ok := labels[k]; ok && prev != v {
				return f, nil, true
			}
			labels[k] = v
		}
		if len(filter.Owners) > 0 {
			switch {
			case q.owner != "" && !slices.Contains(filter.Owners, q.owner):
				return f, nil, true
			case q.owner == "" && len(filter.Owners) == 1:
				f.Owner = filter.Owners[0]
			case q.owner == "":
				owners = slices.Clone(filter.Owners)
			}
		}
	}
	if len(labels) > 0 {
		f.Labels = labels
	}
	return f, owners, false
}

// zeroObject is the empty object of a kind, for a list's resource.
func zeroObject(kind string) v1.Object {
	switch kind {
	case v1.KindProvider:
		return &v1.Provider{}
	case v1.KindModel:
		return &v1.Model{}
	case v1.KindKey:
		return &v1.Key{}
	default:
		return &v1.Budget{}
	}
}
