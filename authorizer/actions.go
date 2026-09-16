// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"maps"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The actions of spec 006's table: every question luxd asks an
// authorizer, and with the resource shapes below the whole of what this
// module adds to the shared contract's envelope. A constant never
// changes its string and never disappears; a new action is a new row in
// that table first and a constant here second.
const (
	ActionProviderCreate = "provider.create"
	ActionProviderRead   = "provider.read"
	ActionProviderUpdate = "provider.update"
	ActionProviderDelete = "provider.delete"
	ActionProviderTunnel = "provider.tunnel"
	ActionProviderList   = "provider.list"
	ActionModelCreate    = "model.create"
	ActionModelRead      = "model.read"
	ActionModelUpdate    = "model.update"
	ActionModelDelete    = "model.delete"
	ActionModelList      = "model.list"
	ActionModelUse       = "model.use"
	ActionKeyCreate      = "key.create"
	ActionKeyRead        = "key.read"
	ActionKeyUpdate      = "key.update"
	ActionKeyDelete      = "key.delete"
	ActionKeyList        = "key.list"
	ActionBudgetCreate   = "budget.create"
	ActionBudgetRead     = "budget.read"
	ActionBudgetUpdate   = "budget.update"
	ActionBudgetDelete   = "budget.delete"
	ActionBudgetList     = "budget.list"
	ActionBudgetDraw     = "budget.draw"
	ActionUsageRead      = "usage.read"
)

// KindUsage is the resource kind of usage.read, which is no manifest
// kind: the resource names the Keys and owners a query asks about.
const KindUsage = "Usage"

// vocabulary is spec 006's table as data, in the spec's order: every
// action luxd asks, each paired with the resource kind it acts on. It is
// the package's one declaration of that table, and Vocabulary, Actions,
// Kind, and Known are four readings of the same value.
var vocabulary = must(authz.NewVocabulary("lux",
	authz.Action{Name: ActionProviderCreate, Kind: v1.KindProvider},
	authz.Action{Name: ActionProviderRead, Kind: v1.KindProvider},
	authz.Action{Name: ActionProviderUpdate, Kind: v1.KindProvider},
	authz.Action{Name: ActionProviderDelete, Kind: v1.KindProvider},
	authz.Action{Name: ActionProviderTunnel, Kind: v1.KindProvider},
	authz.Action{Name: ActionProviderList, Kind: v1.KindProvider},
	authz.Action{Name: ActionModelCreate, Kind: v1.KindModel},
	authz.Action{Name: ActionModelRead, Kind: v1.KindModel},
	authz.Action{Name: ActionModelUpdate, Kind: v1.KindModel},
	authz.Action{Name: ActionModelDelete, Kind: v1.KindModel},
	authz.Action{Name: ActionModelList, Kind: v1.KindModel},
	authz.Action{Name: ActionModelUse, Kind: v1.KindModel},
	authz.Action{Name: ActionKeyCreate, Kind: v1.KindKey},
	authz.Action{Name: ActionKeyRead, Kind: v1.KindKey},
	authz.Action{Name: ActionKeyUpdate, Kind: v1.KindKey},
	authz.Action{Name: ActionKeyDelete, Kind: v1.KindKey},
	authz.Action{Name: ActionKeyList, Kind: v1.KindKey},
	authz.Action{Name: ActionBudgetCreate, Kind: v1.KindBudget},
	authz.Action{Name: ActionBudgetRead, Kind: v1.KindBudget},
	authz.Action{Name: ActionBudgetUpdate, Kind: v1.KindBudget},
	authz.Action{Name: ActionBudgetDelete, Kind: v1.KindBudget},
	authz.Action{Name: ActionBudgetList, Kind: v1.KindBudget},
	authz.Action{Name: ActionBudgetDraw, Kind: v1.KindBudget},
	authz.Action{Name: ActionUsageRead, Kind: KindUsage},
))

// must is the constructor's error, which is a mistake in the table above
// and in no caller: an entry with no name or no kind, or an action named
// twice. A table that cannot be read as a map from action to kind is a
// programming error, so the package refuses to load rather than letting
// half a table answer a question.
func must(v authz.Vocabulary, err error) authz.Vocabulary {
	if err != nil {
		panic("authorizer: " + err.Error())
	}
	return v
}

// Vocabulary is the whole table as the shared contract's own type, which
// is the form a consumer reads it in: a control plane deciding for Lux
// validates an action and a resource kind against it, a conformance
// suite drives a case per row of it, and luxd's own client refuses an
// action outside it before the wire. Import it rather than pairing
// Actions with Kind by hand. The value is fresh on every call, so a
// caller may sort or trim what it gets.
func Vocabulary() authz.Vocabulary {
	return authz.Vocabulary{Core: vocabulary.Core, Actions: slices.Clone(vocabulary.Actions)}
}

// Actions lists every action of the vocabulary, in the table's order.
func Actions() []string {
	out := make([]string, 0, len(vocabulary.Actions))
	for _, a := range vocabulary.Actions {
		out = append(out, a.Name)
	}
	return out
}

// Kind is the resource kind an action acts on: the manifest kind its
// prefix names, Usage for usage.read, and "" for a string outside the
// vocabulary.
func Kind(action string) string {
	kind, _ := vocabulary.Kind(action)
	return kind
}

// Known reports whether action is one of the vocabulary.
func Known(action string) bool { return vocabulary.Known(action) }

// verb is the part of an action after the kind: create, read, list, use.
func verb(action string) string {
	_, v, _ := strings.Cut(action, ".")
	return v
}

// The resource of each action, exactly the fields of spec 006's table.
// Every list and map field is present and never null, so an authorizer
// in any language reads resource.labels as an object and resource.models
// as a list whether or not the manifest set them. A create carries no
// id, because no id exists yet; every action on an object that exists
// carries id and owner.

// ProviderCreate is provider.create's resource, from the manifest.
func ProviderCreate(p *v1.Provider) authz.Resource {
	return authz.NewResource(v1.KindProvider, "", map[string]any{
		"name":    p.Metadata.Name,
		"dialect": string(p.Spec.Dialect),
		"baseURL": p.Spec.BaseURL,
		"tunnel":  p.Spec.Tunnel,
		"labels":  labels(p.Metadata.Labels),
	})
}

// ProviderObject is the resource of provider.read, .update, .delete, and
// .tunnel, from the object the gateway loaded.
func ProviderObject(p *v1.Provider) authz.Resource {
	return authz.NewResource(v1.KindProvider, p.Status.ID, map[string]any{
		"name":    p.Metadata.Name,
		"owner":   p.Status.Owner,
		"dialect": string(p.Spec.Dialect),
		"baseURL": p.Spec.BaseURL,
		"tunnel":  p.Spec.Tunnel,
		"labels":  labels(p.Metadata.Labels),
	})
}

// ProviderList is provider.list's resource: the kind alone.
func ProviderList() authz.Resource { return authz.NewResource(v1.KindProvider, "", nil) }

// ModelCreate is model.create's resource, from the manifest.
func ModelCreate(m *v1.Model) authz.Resource {
	targets := make([]map[string]any, 0, len(m.Spec.Targets))
	for _, t := range m.Spec.Targets {
		targets = append(targets, map[string]any{"provider": t.Provider, "model": t.Model})
	}
	return authz.NewResource(v1.KindModel, "", map[string]any{
		"name":    m.Metadata.Name,
		"targets": targets,
		"labels":  labels(m.Metadata.Labels),
	})
}

// ModelObject is the resource of model.read, .update, and .delete.
func ModelObject(m *v1.Model) authz.Resource {
	return authz.NewResource(v1.KindModel, m.Status.ID, map[string]any{
		"name":   m.Metadata.Name,
		"owner":  m.Status.Owner,
		"source": string(m.Status.Source),
		"labels": labels(m.Metadata.Labels),
	})
}

// ModelList is model.list's resource.
func ModelList() authz.Resource { return authz.NewResource(v1.KindModel, "", nil) }

// ModelUse is model.use's resource: one selector of a Key and the Models
// it matches now, each by id, name, and owner. It carries no id, so the
// decision is never cached and binds the selector, not the matches.
func ModelUse(selector string, matched []v1.ModelRef) authz.Resource {
	refs := make([]map[string]any, 0, len(matched))
	for _, r := range matched {
		refs = append(refs, map[string]any{"id": r.ID, "name": r.Name, "owner": r.Owner})
	}
	return authz.NewResource(v1.KindModel, "", map[string]any{"selector": selector, "matched": refs})
}

// KeyCreate is key.create's resource, from the manifest.
func KeyCreate(k *v1.Key) authz.Resource {
	return authz.NewResource(v1.KindKey, "", map[string]any{
		"name":   k.Metadata.Name,
		"labels": labels(k.Metadata.Labels),
		"models": list(k.Spec.Models),
		"budget": k.Spec.Budget,
	})
}

// KeyObject is the resource of key.read, .update, and .delete.
func KeyObject(k *v1.Key) authz.Resource {
	return authz.NewResource(v1.KindKey, k.Status.ID, map[string]any{
		"name":   k.Metadata.Name,
		"owner":  k.Status.Owner,
		"prefix": k.Status.Prefix,
		"labels": labels(k.Metadata.Labels),
	})
}

// KeyList is key.list's resource.
func KeyList() authz.Resource { return authz.NewResource(v1.KindKey, "", nil) }

// BudgetCreate is budget.create's resource, from the manifest. An absent
// amount reads as "", since a manifest without one is refused at resolve
// and the authorizer is asked first.
func BudgetCreate(b *v1.Budget) authz.Resource {
	amount := ""
	if b.Spec.Amount != nil {
		amount = b.Spec.Amount.String()
	}
	return authz.NewResource(v1.KindBudget, "", map[string]any{
		"name":     b.Metadata.Name,
		"amount":   amount,
		"currency": b.Spec.Currency,
		"window":   string(b.Spec.Window),
		"labels":   labels(b.Metadata.Labels),
	})
}

// BudgetObject is the resource of budget.read, .update, .delete, and
// .draw.
func BudgetObject(b *v1.Budget) authz.Resource {
	return authz.NewResource(v1.KindBudget, b.Status.ID, map[string]any{
		"name":   b.Metadata.Name,
		"owner":  b.Status.Owner,
		"labels": labels(b.Metadata.Labels),
	})
}

// BudgetList is budget.list's resource.
func BudgetList() authz.Resource { return authz.NewResource(v1.KindBudget, "", nil) }

// UsageRead is usage.read's resource: the Key ids and the owners a query
// names, ids because the API resolves names first.
func UsageRead(keys, owners []string) authz.Resource {
	return authz.NewResource(KindUsage, "", map[string]any{"keys": list(keys), "owners": list(owners)})
}

// ResourceFor builds the resource of an action on one manifest object: the
// create shape for a create, the object shape for read, update, delete,
// tunnel, and draw, and the kind alone for a list. ok is false for an
// action whose resource is not an object, model.use and usage.read, for a
// kind the action does not act on, and for a string outside the
// vocabulary.
func ResourceFor(action string, obj v1.Object) (authz.Resource, bool) {
	if obj == nil || Kind(action) != obj.Kind() {
		return authz.Resource{}, false
	}
	v := verb(action)
	if v == "list" {
		return authz.NewResource(obj.Kind(), "", nil), true
	}
	switch x := obj.(type) {
	case *v1.Provider:
		if v == "create" {
			return ProviderCreate(x), true
		}
		return ProviderObject(x), true
	case *v1.Model:
		switch v {
		case "create":
			return ModelCreate(x), true
		case "use":
			return authz.Resource{}, false
		}
		return ModelObject(x), true
	case *v1.Key:
		if v == "create" {
			return KeyCreate(x), true
		}
		return KeyObject(x), true
	case *v1.Budget:
		if v == "create" {
			return BudgetCreate(x), true
		}
		return BudgetObject(x), true
	}
	return authz.Resource{}, false
}

// labels copies a label map, never nil.
func labels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

// list copies a string list, never nil.
func list(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}
