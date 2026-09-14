// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Identity and permission are the platform's, and here they are one
// OIDC issuer and one authorization endpoint. The gateway packages know
// neither: manifest.Resolve is handed an Actor and a Lookup, and the
// doors are handed a Key, so what verifies a person is this file's
// business alone.

// caller is a verified control plane caller: the rendered subject the
// contract stores in every owner field, its two halves, and the claims
// the authorizer reads and this program does not.
type caller struct {
	subject string
	issuer  string
	sub     string
	claims  map[string]any
}

// identity verifies a bearer against one issuer's key set.
type identity struct {
	issuer    string
	audience  string
	validator *jwt.Validator
}

// newIdentity reads the issuer's discovery document for its key set and
// builds the validator. A platform with its own tokens replaces this
// file and nothing else.
func newIdentity(ctx context.Context, client *http.Client, issuer, audience string) (*identity, error) {
	issuer = strings.TrimRight(issuer, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the issuer %s: %w", issuer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the issuer %s answered %d to discovery", issuer, resp.StatusCode)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.JWKSURI == "" {
		return nil, fmt.Errorf("the issuer %s serves no jwks_uri", issuer)
	}
	return &identity{
		issuer:   issuer,
		audience: audience,
		validator: jwt.New(jwt.Config{
			JWKSURL: doc.JWKSURI, Issuer: doc.Issuer, Audiences: []string{audience}, HTTPClient: client,
		}),
	}, nil
}

// authenticate verifies the request's bearer and renders its subject.
func (i *identity) authenticate(r *http.Request) (caller, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return caller{}, errors.New("no bearer")
	}
	claims, err := i.validator.Validate(raw)
	if err != nil {
		return caller{}, err
	}
	if claims.Sub == "" {
		return caller{}, errors.New("the token carries no sub")
	}
	c := caller{issuer: strings.TrimRight(claims.Iss, "/"), sub: claims.Sub, claims: map[string]any{}}
	c.subject = authz.Subject(c.issuer, c.sub)
	// Every claim goes to the authorizer verbatim; this program reads
	// none of them.
	if err := jwt.DecodePayload(raw, &c.claims); err != nil {
		return caller{}, err
	}
	return c, nil
}

// The action vocabulary the gateway's contract defines, which this
// front sends its authorizer unchanged, so one endpoint serves a
// platform's own binary and luxd alike.
const (
	actionProviderCreate = "provider.create"
	actionProviderRead   = "provider.read"
	actionProviderUpdate = "provider.update"
	actionProviderDelete = "provider.delete"
	actionProviderList   = "provider.list"
	actionModelCreate    = "model.create"
	actionModelRead      = "model.read"
	actionModelUpdate    = "model.update"
	actionModelDelete    = "model.delete"
	actionModelList      = "model.list"
	actionModelUse       = "model.use"
	actionKeyCreate      = "key.create"
	actionKeyRead        = "key.read"
	actionKeyUpdate      = "key.update"
	actionKeyDelete      = "key.delete"
	actionKeyList        = "key.list"
	actionBudgetCreate   = "budget.create"
	actionBudgetRead     = "budget.read"
	actionBudgetUpdate   = "budget.update"
	actionBudgetDelete   = "budget.delete"
	actionBudgetList     = "budget.list"
	actionBudgetDraw     = "budget.draw"
	actionUsageRead      = "usage.read"
)

// kindUsage is the resource kind of usage.read, which is no manifest
// kind.
const kindUsage = "Usage"

// resourceFor is the resource one action acts on: the kind, the id of
// an object that exists, and the fields an authorizer decides by. A
// create carries no id, as the object does not exist yet.
func resourceFor(kind string, obj v1.Object) authz.Resource {
	fields := map[string]any{"labels": map[string]string{}}
	id := ""
	if obj != nil {
		id = obj.ID()
		fields["name"] = obj.Name()
		if owner := obj.Owner(); owner != "" {
			fields["owner"] = owner
		}
		if labels := labelsOf(obj); labels != nil {
			fields["labels"] = labels
		}
	}
	return authz.NewResource(kind, id, fields)
}

// labelsOf is an object's labels, whatever its kind.
func labelsOf(obj v1.Object) map[string]string {
	switch x := obj.(type) {
	case *v1.Provider:
		return x.Metadata.Labels
	case *v1.Model:
		return x.Metadata.Labels
	case *v1.Key:
		return x.Metadata.Labels
	case *v1.Budget:
		return x.Metadata.Labels
	}
	return nil
}

// decide asks the platform's authorizer one question. A deny is a
// decision; an error is no decision, and this front fails closed on it
// exactly as the gateway does.
func (p *Plane) decide(ctx context.Context, c caller, action string, res authz.Resource, info authz.Caller) (authz.Decision, error) {
	return p.authorizer.Authorize(ctx, authz.Request{
		Subject: c.subject, Issuer: c.issuer, Sub: c.sub, Claims: c.claims,
		Action: action, Resource: res, Request: info,
	})
}
