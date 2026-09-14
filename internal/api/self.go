// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Self is GET /v1/self: the caller's identity as the verifier rendered
// it, who decides permission, and what this replica remembers granting
// the subject. Limits and Filter are absent until a decision has been
// made for the subject on this replica; they report what was granted
// and grant nothing. In the file mode Policy is file and nothing else
// is set, since no bearer was presented.
type Self struct {
	Subject string         `json:"subject,omitempty"`
	Issuer  string         `json:"issuer,omitempty"`
	Sub     string         `json:"sub,omitempty"`
	Claims  map[string]any `json:"claims,omitempty"`
	Policy  auth.Policy    `json:"policy"`
	Limits  *SelfLimits    `json:"limits,omitempty"`
	Filter  *authz.Filter  `json:"filter,omitempty"`
}

// SelfLimits are the limits of the last allow in the wire names of spec
// 006's limits object, a member absent when the allow did not name it.
type SelfLimits struct {
	RequestsPerMinute       int    `json:"requests_per_minute,omitempty"`
	MaxKeyRequestsPerMinute int    `json:"max_key_requests_per_minute,omitempty"`
	MaxKeyTokensPerMinute   int    `json:"max_key_tokens_per_minute,omitempty"`
	MaxKeySpend             string `json:"max_key_spend,omitempty"`
	MaxKeyTTL               string `json:"max_key_ttl,omitempty"`
	MaxKeys                 int    `json:"max_keys,omitempty"`
}

// self asks no action: asking permission to see the identity the caller
// just presented would be a category error.
func (c *call) self(context.Context) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	out := Self{Policy: c.h.o.Auth.Policy}
	if c.h.fileMode() {
		return c.writeJSON(http.StatusOK, out, 0)
	}
	out.Subject, out.Issuer, out.Sub, out.Claims = c.caller.Subject, c.caller.Issuer, c.caller.Sub, c.caller.Claims
	if g, ok := c.h.grants.get(c.caller.Subject); ok {
		out.Filter = g.filter
		l := g.limits
		if l != (authorizer.Limits{}) {
			out.Limits = &SelfLimits{
				RequestsPerMinute:       l.RequestsPerMinute,
				MaxKeyRequestsPerMinute: l.Key.MaxRequestsPerMinute,
				MaxKeyTokensPerMinute:   l.Key.MaxTokensPerMinute,
				MaxKeys:                 l.MaxKeys,
			}
			if l.Key.MaxSpend > 0 {
				out.Limits.MaxKeySpend = l.Key.MaxSpend.String()
			}
			if l.Key.MaxTTL > 0 {
				out.Limits.MaxKeyTTL = string(v1.DurationOf(l.Key.MaxTTL))
			}
		}
	}
	return c.writeJSON(http.StatusOK, out, 0)
}

// WellKnown is GET /.well-known/lux: the one document a client reads
// before it has a token. Every URL is built from LUX_PUBLIC_URL; Mode is
// server or file.
type WellKnown struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	APIVersion string            `json:"apiVersion"`
	API        string            `json:"api"`
	OpenAPI    string            `json:"openapi"`
	Doors      map[string]string `json:"doors"`
	Dialects   []string          `json:"dialects"`
	Issuers    []string          `json:"issuers"`
	Audience   string            `json:"audience"`
	Mode       string            `json:"mode"`
}

// wellKnown needs no bearer: a document that describes the API carries
// no installation's data.
func (c *call) wellKnown(context.Context) *Error {
	base := c.h.o.PublicURL.String()
	doc := WellKnown{
		Name: "lux", Version: c.h.o.Version, APIVersion: v1.APIVersion,
		API: base + "/v1", OpenAPI: base + "/v1/openapi.json",
		Doors:    map[string]string{},
		Dialects: []string{string(v1.DialectOpenAI), string(v1.DialectAnthropic), string(v1.DialectGemini), string(v1.DialectLux)},
		Issuers:  []string{},
		Audience: "",
		Mode:     "server",
	}
	if doc.Version == "" {
		doc.Version = "dev"
	}
	for _, d := range doc.Dialects {
		doc.Doors[d] = base + "/" + d
	}
	if c.h.fileMode() {
		doc.Mode = "file"
	} else {
		doc.Issuers = c.h.o.Auth.Verifier.Issuers()
		doc.Audience = c.h.o.Auth.Verifier.Audience()
	}
	return c.writeJSON(http.StatusOK, doc, 0)
}
