// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Discover sets BaseReplacesV1 from the installation's /.well-known/lux.
//
// A BaseURL without a path is an installation's root, where the control
// plane is at /v1 under both mounts, since a base path is what the other
// mount replaces /v1 with: Discover clears BaseReplacesV1 and sends
// nothing. Otherwise it reads the document and compares two of its
// members. Each door is P + "/" + dialect for one address P, the public
// URL, and the control plane is P + "/v1" at the root and under a base
// path the listener prefixes whole, or P itself where the base path
// stands in the place of /v1. Only that relation is read and never a
// host, so a client that reaches the installation at an address other
// than the public one, such as a cluster-internal one, keeps its own, and
// no bearer is ever sent to an address the document named.
//
// A failed request is the error Do returns, and a document that shows
// neither shape is an error wrapping an *UnreadableError; either leaves
// BaseReplacesV1 as it was.
func (c *Client) Discover(ctx context.Context) error {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil {
		return &TransportError{Method: http.MethodGet, URL: c.BaseURL, Err: err}
	}
	if base.Path == "" {
		c.BaseReplacesV1 = false
		return nil
	}
	resp, err := c.WellKnown(ctx)
	if err != nil {
		return err
	}
	var doc struct {
		API   string            `json:"api"`
		Doors map[string]string `json:"doors"`
	}
	unreadable := &UnreadableError{Method: http.MethodGet, URL: c.url(wellKnownPath), Status: resp.Status, RequestID: resp.RequestID, Body: resp.Body}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return unreadable
	}
	replaces, err := replacesV1(doc.API, doc.Doors)
	if err != nil {
		return fmt.Errorf("%w: %w", unreadable, err)
	}
	c.BaseReplacesV1 = replaces
	return nil
}

// wellKnownPath is the discovery document's path under BaseURL.
const wellKnownPath = "/.well-known/lux"

// replacesV1 reads the mount from a discovery document's api and doors
// members: false for a control plane at P + "/v1", true for one at P,
// where P is the one address every door extends with its dialect.
func replacesV1(api string, doors map[string]string) (bool, error) {
	if len(doors) == 0 {
		return false, fmt.Errorf("the document names no door")
	}
	public := ""
	for _, dialect := range slices.Sorted(maps.Keys(doors)) {
		door := doors[dialect]
		p, ok := strings.CutSuffix(door, "/"+dialect)
		switch {
		case !ok || p == "":
			return false, fmt.Errorf("the %s door %q is not an address plus /%s", dialect, door, dialect)
		case public != "" && p != public:
			return false, fmt.Errorf("the doors extend two addresses, %s and %s", public, p)
		}
		public = p
	}
	switch api {
	case public + versionSegment:
		return false, nil
	case public:
		return true, nil
	}
	return false, fmt.Errorf("the control plane %q is neither %s%s nor %s", api, public, versionSegment, public)
}
