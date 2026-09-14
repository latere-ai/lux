// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The two usage routes of spec 011 over spec 009's parameters, rows,
// and records: GET /v1/usage answers aggregates and GET /v1/requests
// answers records, newest first. Both parse the query strictly,
// resolve a Key, Model, or Provider name to its id before the
// authorizer sees the resource, ask usage.read once with the resource
// of spec 006, and intersect the decision's filter with the query, so
// a caller outside the filter reads an empty items and never a 403.

// maxRecordLimit is the page cap of GET /v1/requests, spec 009's: a
// record is a ledger line a caller pulls in bulk rather than an object
// to render, so the cap is above a list's.
const maxRecordLimit = 1000

// sourceMemory is the record set this build reads: the replica's own
// ring of the last records per Key. The other source, the durable and
// installation-wide archive, arrives with the request log exporter of
// spec 012, which is what makes the answer span the replicas.
const sourceMemory = "memory"

// sourceArchive is the other value the source member takes, named in
// the OpenAPI document because a client reads the member either way:
// the durable, installation-wide record set of spec 012's request log,
// which no build answers until its exporter lands.
const sourceArchive = "archive"

// usageResponse is GET /v1/usage: the rows, never null, and no page,
// since the ninety day range bound is what keeps the row count finite.
type usageResponse struct {
	Items []metering.Row `json:"items"`
}

// recordsResponse is GET /v1/requests: the records, the cursor of the
// next page while rows remain, and the set they were read from.
type recordsResponse struct {
	Items      []metering.Record `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
	Source     string            `json:"source"`
}

// usageQuery is one usage route's query as the handler parsed it: the
// metering query both routes build, the paging of GET /v1/requests,
// and whether the filters already select nothing, which is an empty
// items and no question for the store.
type usageQuery struct {
	q      metering.RecordQuery
	limit  int
	cursor string
	empty  bool
}

// usage is GET /v1/usage: the aggregates of spec 009 under the
// authorizer's filter, unpaged.
func (c *call) usage(ctx context.Context) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	q, err := c.usageQuery(ctx, false)
	if err != nil {
		return err
	}
	if err := c.authorizeUsage(ctx, &q); err != nil {
		return err
	}
	if q.empty {
		return c.writeJSON(http.StatusOK, usageResponse{Items: []metering.Row{}}, 0)
	}
	rows, rerr := serve.Usage(ctx, c.h.o.Store, q.q.Query, c.h.o.Now())
	if rerr != nil {
		return usageError(rerr)
	}
	return c.writeJSON(http.StatusOK, usageResponse{Items: rows}, 0)
}

// requests is GET /v1/requests: the records of spec 009, newest first,
// paged by cursor and next_cursor, with the source they were read from
// beside them.
func (c *call) requests(ctx context.Context) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	q, err := c.usageQuery(ctx, true)
	if err != nil {
		return err
	}
	if err := c.authorizeUsage(ctx, &q); err != nil {
		return err
	}
	if q.empty {
		return c.writeJSON(http.StatusOK, recordsResponse{Items: []metering.Record{}, Source: sourceMemory}, 0)
	}
	records, next, rerr := c.h.o.Store.Usage().Records(ctx, q.q, store.Page{Limit: q.limit, Cursor: q.cursor})
	if rerr != nil {
		return mapError(rerr)
	}
	if records == nil {
		records = []metering.Record{}
	}
	return c.writeJSON(http.StatusOK, recordsResponse{Items: records, NextCursor: next, Source: sourceMemory}, 0)
}

// usageQuery parses the route's query, fills the range it left open,
// holds it to spec 009's parameter table, and resolves every name to
// an id, so the resource the authorizer sees and the query the store
// reads carry ids alone.
func (c *call) usageQuery(ctx context.Context, records bool) (usageQuery, *Error) {
	q, err := parseUsageQuery(c.r.URL.Query(), records)
	if err != nil {
		return q, err
	}
	q.q.Query = q.q.WithDefaults(c.h.o.Now())
	if verr := q.q.Validate(); verr != nil {
		return q, usageError(verr)
	}
	for _, ref := range []struct {
		kind, prefix string
		refs         *[]string
	}{
		{v1.KindKey, v1.PrefixKey, &q.q.Keys},
		{v1.KindModel, v1.PrefixModel, &q.q.Models},
		{v1.KindProvider, v1.PrefixProvider, &q.q.Providers},
	} {
		ids, err := c.resolveUsageIDs(ctx, ref.kind, ref.prefix, *ref.refs)
		if err != nil {
			return q, err
		}
		// Every name the parameter carried names nothing, so the filter
		// selects nothing; an empty list would select everything.
		if len(*ref.refs) > 0 && len(ids) == 0 {
			q.empty = true
		}
		*ref.refs = ids
	}
	return q, nil
}

// resolveUsageIDs maps each reference of one parameter to an id: the
// reference itself when it carries the kind's prefix, since a Key's
// key_ id keeps working after the Key is deleted; the live object's id
// when a name names one; and nothing when it names none, which narrows
// the query rather than refusing, for the same reason a filter outside
// the authorizer's does.
func (c *call) resolveUsageIDs(ctx context.Context, kind, prefix string, refs []string) ([]string, *Error) {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref, prefix) {
			out = append(out, ref)
			continue
		}
		if hasKindPrefix(ref) {
			continue
		}
		obj, _, err := c.h.o.Store.Objects().ByName(ctx, kind, ref)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, obj.ID())
	}
	return out, nil
}

// authorizeUsage asks usage.read with the resolved Key ids and the
// owners of the query, and narrows the query by the allow's filter:
// owners narrow the owners and labels add their pairs, and an
// intersection that selects nothing is an empty items. The file mode
// asks nothing, because there is no bearer and no authorizer.
func (c *call) authorizeUsage(ctx context.Context, q *usageQuery) *Error {
	if c.h.fileMode() {
		return nil
	}
	d, err := c.authorize(ctx, auth.ActionUsageRead, auth.UsageRead(q.q.Keys, q.q.Owners))
	if err != nil {
		return err
	}
	if d.Filter == nil {
		return nil
	}
	narrowed, ok := metering.Intersect(q.q.Query, d.Filter.Owners, d.Filter.Labels)
	q.q.Query = narrowed
	if !ok {
		q.empty = true
	}
	return nil
}

// usageError answers a *metering.QueryError as invalid_field at the
// parameter it names, with its developer detail, and anything else
// through mapError.
func usageError(err error) *Error {
	if qe, ok := errors.AsType[*metering.QueryError](err); ok {
		return refuse(CodeInvalidField, qe.Detail, qe.Field)
	}
	return mapError(err)
}

// parseUsageQuery reads the parameters spec 009's tables define for the
// route and refuses any other, or a value outside its rule, with
// invalid_field at the parameter's name, the same strictness a list
// has. records selects GET /v1/requests, whose table carries status,
// error, stream, limit, and cursor and carries no by or interval.
func parseUsageQuery(values url.Values, records bool) (usageQuery, *Error) {
	out := usageQuery{}
	out.q.Labels = map[string]string{}
	if records {
		out.limit = defaultLimit
	}
	q := &out.q
	for name, vals := range values {
		value := one(vals)
		switch {
		case name == "from", name == "to":
			at, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return out, refuse(CodeInvalidField, name+" "+strconv.Quote(value)+" is not an RFC 3339 instant", name)
			}
			if name == "from" {
				q.From = at
			} else {
				q.To = at
			}
		case name == "by" && !records:
			for _, v := range vals {
				for d := range strings.SplitSeq(v, ",") {
					q.By = append(q.By, metering.Dimension(d))
				}
			}
		case name == "interval" && !records:
			q.Interval = metering.Interval(value)
		case name == "key":
			q.Keys = append(q.Keys, vals...)
		case name == "model":
			q.Models = append(q.Models, vals...)
		case name == "provider":
			q.Providers = append(q.Providers, vals...)
		case name == "owner":
			q.Owners = append(q.Owners, vals...)
		case name == "label":
			for _, v := range vals {
				key, val, ok := strings.Cut(v, "=")
				if !ok || key == "" {
					return out, refuse(CodeInvalidField, "label "+strconv.Quote(v)+" is not name=value", "label")
				}
				// Two values for one label name match nothing.
				if have, dup := q.Labels[key]; dup && have != val {
					out.empty = true
					continue
				}
				q.Labels[key] = val
			}
		case name == "status" && records:
			switch metering.Status(value) {
			case metering.StatusOK, metering.StatusRefused, metering.StatusFailed:
				q.Status = metering.Status(value)
			default:
				return out, refuse(CodeInvalidField, "status "+strconv.Quote(value)+" is not ok, refused, or failed", "status")
			}
		case name == "error" && records:
			q.Error = value
		case name == "stream" && records:
			switch value {
			case "true", "false":
				stream := value == "true"
				q.Stream = &stream
			default:
				return out, refuse(CodeInvalidField, "stream "+strconv.Quote(value)+" is not true or false", "stream")
			}
		case name == "limit" && records:
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > maxRecordLimit {
				return out, refuse(CodeInvalidField, "limit "+strconv.Quote(value)+" is not an integer from 1 to "+strconv.Itoa(maxRecordLimit), "limit")
			}
			out.limit = n
		case name == "cursor" && records:
			out.cursor = value
		case name == "source" && records:
			return out, refuse(CodeInvalidField, "source is a member of the response and not a parameter: this replica answers "+sourceMemory+", its own ring of records, until a request log archive is configured", "source")
		default:
			return out, refuse(CodeInvalidField, "this route defines no parameter "+strconv.Quote(name), name)
		}
	}
	if len(q.Labels) == 0 {
		q.Labels = nil
	}
	return out, nil
}
