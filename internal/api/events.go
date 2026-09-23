// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The events an apply, a rotate, and a delete raise, spec 012's rows for
// the API: <kind>.created, .updated, .deleted, and key.rotated, with
// the data of each row, journalled inside the transaction that writes
// the object so the row commits with it. The Key cache of spec 007
// evicts on key.updated, key.rotated, key.deleted, budget.updated, and
// budget.deleted, so those are raised exactly when the object changed.

// eventPrefix is the type's first half per kind.
func eventPrefix(kind string) string {
	switch kind {
	case v1.KindProvider:
		return "provider"
	case v1.KindModel:
		return "model"
	case v1.KindKey:
		return "key"
	default:
		return "budget"
	}
}

// journal writes one event of the request into tx.
func (c *call) journal(ctx context.Context, tx store.Store, typ string, obj v1.Object, data any) error {
	return serve.AppendEvent(ctx, tx.Journal(), serve.Event{
		ID: c.h.o.NewID(v1.PrefixEvent), Type: typ, At: c.start, Subject: c.caller.Subject,
		Reason: serve.ReasonRequest, RequestID: c.id, Object: obj, Data: data,
	})
}

// createdData is the data of a <kind>.created row.
func createdData(obj v1.Object) map[string]any {
	switch x := obj.(type) {
	case *v1.Provider:
		return map[string]any{"dialect": string(x.Spec.Dialect), "baseURL": x.Spec.BaseURL}
	case *v1.Model:
		targets := make([]map[string]any, 0, len(x.Spec.Targets))
		for _, t := range x.Spec.Targets {
			targets = append(targets, map[string]any{"provider": t.Provider, "model": t.Model})
		}
		return map[string]any{"targets": targets, "priced": x.Spec.Pricing != nil}
	case *v1.Key:
		data := map[string]any{"prefix": x.Status.Prefix, "models": x.Spec.Models, "budget": x.Spec.Budget}
		if !x.Status.ExpiresAt.IsZero() {
			data["expiresAt"] = x.Status.ExpiresAt
		}
		return data
	case *v1.Budget:
		data := map[string]any{"currency": x.Spec.Currency, "window": string(x.Spec.Window), "hard": x.Spec.Hard == nil || *x.Spec.Hard}
		if x.Spec.Amount != nil {
			data["amount"] = x.Spec.Amount.String()
		}
		return data
	}
	return map[string]any{}
}

// committed tells this replica's caches that a mutation committed.
func (c *call) committed(ctx context.Context) {
	if c.h.o.Committed != nil {
		c.h.o.Committed(ctx)
	}
}
