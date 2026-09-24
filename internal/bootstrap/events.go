// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"latere.ai/x/lux/internal/events"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The events a bootstrap write raises are spec 012's rows for an apply,
// <kind>.created and <kind>.updated, with the data /v1 writes for them,
// so a sink and the Key cache read a bootstrapped object's change as
// they read any other; the reason is events.ReasonBootstrap, and the
// subject and request id are empty, because no caller made the change.

// eventType is the type of the row for a created or an updated object.
func eventType(kind string, created bool) string {
	switch {
	case kind == v1.KindProvider && created:
		return events.ProviderCreated
	case kind == v1.KindProvider:
		return events.ProviderUpdated
	case kind == v1.KindModel && created:
		return events.ModelCreated
	case kind == v1.KindModel:
		return events.ModelUpdated
	case kind == v1.KindKey:
		// A Key is created or left alone, never updated here.
		return events.KeyCreated
	case created:
		return events.BudgetCreated
	default:
		return events.BudgetUpdated
	}
}

// createdData is the data of a <kind>.created row, the members of its
// row in events.Table.
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
		if len(x.Spec.Budgets) > 0 {
			data["budgets"] = x.Spec.Budgets
		}
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
