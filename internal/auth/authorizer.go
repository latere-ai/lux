// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"latere.ai/x/pkg/authz"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Authorizer asks one authz.Authorizer, the shared client over the
// operator's endpoint or the OwnerPolicy, in Lux's vocabulary, and reads
// the answer into Lux's codes: a deny on the request's own action is
// forbidden with the authorizer's reason as the developer detail, and a
// call that produced no decision is authorizer_unavailable. The cache,
// the retry, and the timeout are the shared client's; nothing here
// decides.
type Authorizer struct {
	a authz.Authorizer
}

// NewAuthorizer wraps the decision maker.
func NewAuthorizer(a authz.Authorizer) *Authorizer { return &Authorizer{a: a} }

// Decision is an allow with what came with it: the limits decoded into
// Lux's figures, the filter of a list action, and the time the shared
// client holds the allow for.
type Decision struct {
	Limits Limits
	Filter *authz.Filter
	TTL    time.Duration
}

// Request builds the envelope for one question: the caller's three
// subject fields and every claim, the action, its resource, and what the
// authorizer learns about the request itself. Workload is never set,
// because a data plane request asks nothing.
func Request(c Caller, action string, res authz.Resource, info authz.Caller) authz.Request {
	claims := c.Claims
	if claims == nil {
		claims = map[string]any{}
	}
	return authz.Request{
		Subject:  c.Subject,
		Issuer:   c.Issuer,
		Sub:      c.Sub,
		Claims:   claims,
		Action:   action,
		Resource: res,
		Request:  info,
	}
}

// Decide asks the request's own action and returns the allow, or an
// *Error: forbidden on a deny, authorizer_unavailable when no decision
// could be had or the answer's limits do not read as Lux's figures.
func (z *Authorizer) Decide(ctx context.Context, c Caller, action string, res authz.Resource, info authz.Caller) (Decision, error) {
	d, err := z.ask(ctx, c, action, res, info)
	if err != nil {
		return Decision{}, err
	}
	if !d.Allow {
		return Decision{}, refuse(CodeForbidden, "authz deny subject="+c.Subject+" action="+action+" resource="+res.Kind+"/"+res.ID+": "+d.Reason)
	}
	limits, err := DecodeLimits(d)
	if err != nil {
		return Decision{}, refuse(CodeAuthorizerUnavailable, "the answer to "+action+" carries limits this gateway cannot read: "+err.Error())
	}
	return Decision{Limits: limits, Filter: d.Filter, TTL: d.TTL}, nil
}

// The lux.authorizer span of spec 019's table, one per question asked,
// under the lux.api span of the request that asks: the action and the
// decision, allow, deny, or unavailable, and never the subject.
const (
	SpanAuthorizer = "lux.authorizer"
	AttrAction     = "lux.action"
	AttrDecision   = "lux.decision"
	tracerScope    = "latere.ai/x/lux/internal/auth"
)

// The decision attribute's closed set, the metric's vocabulary.
const (
	DecisionAllow       = "allow"
	DecisionDeny        = "deny"
	DecisionUnavailable = "unavailable"
)

// ask sends one envelope inside its span and maps a call that produced
// no decision to authorizer_unavailable. A deny comes back as a
// Decision.
func (z *Authorizer) ask(ctx context.Context, c Caller, action string, res authz.Resource, info authz.Caller) (authz.Decision, error) {
	ctx, span := otel.Tracer(tracerScope).Start(ctx, SpanAuthorizer, trace.WithAttributes(attribute.String(AttrAction, action)))
	defer span.End()
	d, err := z.a.Authorize(ctx, Request(c, action, res, info))
	switch {
	case err != nil:
		span.SetAttributes(attribute.String(AttrDecision, DecisionUnavailable))
		return authz.Decision{}, refuse(CodeAuthorizerUnavailable, err.Error())
	case d.Allow:
		span.SetAttributes(attribute.String(AttrDecision, DecisionAllow))
	default:
		span.SetAttributes(attribute.String(AttrDecision, DecisionDeny))
	}
	return d, nil
}

// Check sends the probe every authorizer denies, the action and kind
// spec 017's check command names, and reports an authorizer that answered
// nothing or allowed it. The shared client holds the probe's deny as it
// holds any deny, under the anonymous subject and the reserved id, which
// no request about an object ever shares: no object has that id.
func (z *Authorizer) Check(ctx context.Context) error {
	return authz.Check(ctx, z.a, ActionProviderRead, v1.KindProvider)
}
