// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"net/url"
)

// List is GET path with the query, followed through next_cursor until
// the last page or until limit items are held, limit 0 being no bound.
// One page under the bound comes back byte for byte; otherwise the pages
// are folded into one envelope whose items keep each item's bytes, whose
// next_cursor is absent, and whose other members are the last page's.
func (c *Client) List(ctx context.Context, path string, query url.Values, limit int) (*Response, error) {
	q := url.Values{}
	maps.Copy(q, query)
	var (
		first   *Response
		items   []jsontext.Value
		members []member
		pages   int
	)
	for {
		resp, err := c.Do(ctx, Request{Method: http.MethodGet, Path: path, Query: q})
		if err != nil {
			return nil, err
		}
		page, err := parsePage(resp.Body)
		if err != nil {
			return nil, &UnreadableError{Method: http.MethodGet, URL: c.url(path), Status: resp.Status, RequestID: resp.RequestID, Body: resp.Body}
		}
		pages++
		if first == nil {
			first = resp
		}
		items = append(items, page.items...)
		members = page.members
		if page.next == "" || (limit > 0 && len(items) >= limit) {
			break
		}
		q.Set("cursor", page.next)
	}
	if pages == 1 && (limit == 0 || len(items) <= limit) {
		return first, nil
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	body, err := assemble(items, members)
	if err != nil {
		return nil, err
	}
	first.Body = body
	return first, nil
}

// member is one top-level member of a list page in the order it arrived,
// with its value's bytes untouched.
type member struct {
	key string
	raw jsontext.Value
}

// page is one list response taken apart.
type page struct {
	members []member
	items   []jsontext.Value
	next    string
}

// parsePage reads {"items": [...], "next_cursor": "...", ...} member by
// member, so the order and the bytes of what the server sent survive.
func parsePage(body []byte) (page, error) {
	var p page
	dec := jsontext.NewDecoder(bytes.NewReader(body), serverJSON)
	tok, err := dec.ReadToken()
	if err != nil {
		return p, err
	}
	if tok.Kind() != '{' {
		return p, errNotEnvelope
	}
	seen := false
	for dec.PeekKind() != '}' {
		kt, err := dec.ReadToken()
		if err != nil {
			return p, err
		}
		key := kt.String()
		raw, err := dec.ReadValue()
		if err != nil {
			return p, err
		}
		raw = raw.Clone()
		switch key {
		case "items":
			if err := json.Unmarshal(raw, &p.items, serverJSON); err != nil {
				return p, err
			}
			seen = true
		case "next_cursor":
			if err := json.Unmarshal(raw, &p.next); err != nil {
				return p, err
			}
		default:
			p.members = append(p.members, member{key: key, raw: raw})
		}
	}
	if !seen {
		return p, errNotEnvelope
	}
	return p, nil
}

// errNotEnvelope is what parsePage reports for a body that is not a list
// envelope: not an object, or without items.
var errNotEnvelope = &jsontext.SyntacticError{Err: errors.New("not a list envelope")}

// assemble renders one envelope: items first, then the other members in
// their order, next_cursor left out, and the newline every body of the
// API ends in.
func assemble(items []jsontext.Value, members []member) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"items":[`)
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
	for _, m := range members {
		b.WriteByte(',')
		k, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.raw)
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}
