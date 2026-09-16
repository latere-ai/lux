// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
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
		items   []json.RawMessage
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
			return nil, &UnreadableError{Method: http.MethodGet, URL: c.BaseURL + path, Status: resp.Status, RequestID: resp.RequestID, Body: resp.Body}
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
	raw json.RawMessage
}

// page is one list response taken apart.
type page struct {
	members []member
	items   []json.RawMessage
	next    string
}

// parsePage reads {"items": [...], "next_cursor": "...", ...} member by
// member, so the order and the bytes of what the server sent survive.
func parsePage(body []byte) (page, error) {
	var p page
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return p, err
	}
	if tok != json.Delim('{') {
		return p, &json.SyntaxError{Offset: 0}
	}
	seen := false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return p, err
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return p, err
		}
		switch key {
		case "items":
			if err := json.Unmarshal(raw, &p.items); err != nil {
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
		return p, &json.SyntaxError{Offset: 0}
	}
	return p, nil
}

// assemble renders one envelope: items first, then the other members in
// their order, next_cursor left out, and the newline every body of the
// API ends in.
func assemble(items []json.RawMessage, members []member) ([]byte, error) {
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
