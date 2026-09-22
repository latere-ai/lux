// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"

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

// updatedData is the data of a <kind>.updated row: the paths of
// metadata and spec that differ between the stored object and the
// written one, dotted for a struct member and [n] for a list entry, in
// path order. The write-only members have no encoding and never appear,
// so a Provider whose credential alone changed reports no path.
func updatedData(old, obj v1.Object) map[string]any {
	var paths []string
	diff("", shape(old), shape(obj), &paths)
	slices.Sort(paths)
	return map[string]any{"paths": paths}
}

// shape is the object's metadata and spec as a JSON tree, what an
// idempotent apply compares and an update reports the paths of.
func shape(obj v1.Object) map[string]any {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil
	}
	return map[string]any{"metadata": tree["metadata"], "spec": tree["spec"]}
}

// diff appends the paths at which a and b differ.
func diff(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := make([]string, 0, len(am)+len(bm))
		for k := range am {
			keys = append(keys, k)
		}
		for k := range bm {
			if _, in := am[k]; !in {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			diff(join(path, k), am[k], bm[k], out)
		}
		return
	}
	al, alok := a.([]any)
	bl, blok := b.([]any)
	if alok && blok {
		for i := range max(len(al), len(bl)) {
			var x, y any
			if i < len(al) {
				x = al[i]
			}
			if i < len(bl) {
				y = bl[i]
			}
			diff(path+"["+strconv.Itoa(i)+"]", x, y, out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
