// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
)

// ObjectLookup is what the owner policy asks the store: whether an id of
// a kind names an object, and who created it. The caller supplies it, so
// the policy is a function over the request and what was found and
// imports no store.
type ObjectLookup interface {
	Object(ctx context.Context, kind, id string) (authz.Object, error)
}

// The reasons Lux's rows deny with, beside the frame's probe, anonymous,
// and not_owner.
const (
	// ReasonAdminOnly is a Provider or Model declared, changed, or deleted
	// by a subject that is not an admin: those objects hold the operator's
	// credentials and prices.
	ReasonAdminOnly = "admin_only"
	// ReasonUnknownAction is a string outside the vocabulary, which no row
	// allows.
	ReasonUnknownAction = "unknown_action"
)

// OwnerPolicy is the decision the gateway makes when no authorizer is
// configured: authz.Policy's frame with Lux's rows in front of it. It is
// an authz.Authorizer, so the Authorizer over it and the one over the
// shared client are one type to the API.
//
// The rows, in order: the probe id is denied; the anonymous subject is
// denied; an admin may do everything; declaring, changing, or deleting a
// Provider or a Model is an admin's alone, except a Provider with tunnel
// true, which any subject may create and whose owner may update, delete,
// and tunnel; the catalog, provider.read, provider.list, model.read,
// model.list, and model.use, is every subject's; key.create and
// budget.create are every subject's; key.list, budget.list, and
// usage.read are allowed with the filter narrowed to the subject alone;
// every other action on an object named by id is the frame's, the owner
// allowed and everyone else not_owner whether or not the object exists.
// No limits are granted.
//
// Every allow is then intersected with the grants the caller's token
// carries, which is what luxd promises by reading them at the door
// (spec 006). A person's key narrowed to some Models or some Keys is
// answered grant everywhere else, and a grant on an object the person
// may not touch still reaches nothing: the rows above answer first.
type OwnerPolicy struct {
	// Admins are the rendered subjects of LUX_ADMIN_SUBJECTS.
	Admins []string
	// Objects answers the frame's question. Required.
	Objects ObjectLookup
}

// Authorize implements authz.Authorizer: the rows above, intersected
// with the grants the caller's token carries.
func (p *OwnerPolicy) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	d, err := p.decide(ctx, req)
	if err != nil {
		return authz.Decision{}, err
	}
	return restrict(req, d), nil
}

// grantCore qualifies a bare action the way a grant writes it,
// lux:key.read for key.read. It is the published vocabulary's own name,
// read once, because a comparison that forgets to qualify denies
// everything.
var grantCore = authorizer.Vocabulary().Core

// restrict is spec 006's conjunction at the site that decides: a
// personal access token carries the grants its holder chose, and an
// allow the grants do not cover becomes a deny with reason grant. It
// never turns a deny into an allow, so the policy above is the ceiling
// and a grant is never authority.
//
// A claim that cannot be read as grants is a deny, the way the shared
// scaffold answers one: the verifier at luxd's door refuses such a
// token, so one that reached here arrived another way and the closed
// answer is the only safe one.
func restrict(req authz.Request, d authz.Decision) authz.Decision {
	if !d.Allow {
		return d
	}
	grants, err := authz.ParseGrants(req.Claims)
	if err != nil {
		return authz.Decision{Reason: authz.ReasonGrant}
	}
	return authz.Restrict(grantCore, d, req, grants)
}

// decide is the policy's own answer, the rows of the comment above.
func (p *OwnerPolicy) decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	frame := authz.Policy{Admins: p.Admins}
	switch {
	case strings.EqualFold(req.Resource.ID, authz.ProbeID):
		return authz.Decision{Reason: authz.ReasonProbe}, nil
	case req.Subject == "":
		return authz.Decision{Reason: authz.ReasonAnonymous}, nil
	case !authorizer.Known(req.Action):
		return authz.Decision{Reason: ReasonUnknownAction}, nil
	case slices.Contains(p.Admins, req.Subject):
		return authz.Decision{Allow: true}, nil
	}
	switch req.Action {
	case authorizer.ActionOwnerAssign:
		return authz.Decision{Reason: ReasonAdminOnly}, nil
	case authorizer.ActionProviderCreate:
		if tunnel, _ := req.Resource.Fields["tunnel"].(bool); tunnel {
			return authz.Decision{Allow: true}, nil
		}
		return authz.Decision{Reason: ReasonAdminOnly}, nil
	case authorizer.ActionProviderUpdate, authorizer.ActionProviderDelete:
		if req.Action == authorizer.ActionProviderUpdate {
			if proposed, ok := req.Resource.Fields["proposed"].(map[string]any); ok {
				if spec, ok := proposed["spec"].(map[string]any); ok {
					if tunnel, _ := spec["tunnel"].(bool); !tunnel {
						return authz.Decision{Reason: ReasonAdminOnly}, nil
					}
				}
			}
		}
		if tunnel, _ := req.Resource.Fields["tunnel"].(bool); !tunnel {
			return authz.Decision{Reason: ReasonAdminOnly}, nil
		}
	case authorizer.ActionModelCreate, authorizer.ActionModelUpdate, authorizer.ActionModelDelete:
		return authz.Decision{Reason: ReasonAdminOnly}, nil
	case authorizer.ActionProviderRead, authorizer.ActionProviderList, authorizer.ActionModelRead, authorizer.ActionModelList, authorizer.ActionModelUse,
		authorizer.ActionKeyCreate, authorizer.ActionBudgetCreate:
		return authz.Decision{Allow: true}, nil
	case authorizer.ActionKeyList, authorizer.ActionBudgetList, authorizer.ActionUsageRead:
		return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{req.Subject}}}, nil
	}
	// The frame: key.read, .update, .delete, budget.read, .update,
	// .delete, .draw, provider.tunnel, and a tunnelled Provider's update
	// and delete. The object is looked up by the resource's kind and id;
	// a resource with no id names nothing, which the frame denies.
	obj := authz.Object{}
	if req.Resource.ID != "" {
		if p.Objects == nil {
			return authz.Decision{}, fmt.Errorf("owner policy: no object lookup to decide %s on %s/%s", req.Action, req.Resource.Kind, req.Resource.ID)
		}
		var err error
		obj, err = p.Objects.Object(ctx, req.Resource.Kind, req.Resource.ID)
		if err != nil {
			return authz.Decision{}, fmt.Errorf("owner policy: %s/%s: %w", req.Resource.Kind, req.Resource.ID, err)
		}
	}
	return frame.Decide(req, obj), nil
}
