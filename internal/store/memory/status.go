// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The two halves of status, as spec 010's table divides them. Put
// stores the control plane's half and clears the observed members;
// PutStatus keeps the observed half as the kind's store struct beside
// the row; a read clones the stored object and lays the observed
// members over it.

// clone copies one object through its JSON form, which is the copy the
// store keeps and the copy it hands out. The write-only values, a
// Provider's credential and a Key's supplied value, do not survive the
// trip, so the store never holds one.
func clone(obj v1.Object) (v1.Object, error) {
	var out v1.Object
	switch obj.(type) {
	case *v1.Provider:
		out = &v1.Provider{}
	case *v1.Model:
		out = &v1.Model{}
	case *v1.Key:
		out = &v1.Key{}
	case *v1.Budget:
		out = &v1.Budget{}
	default:
		return nil, fmt.Errorf("memory: %T is not one of the four kinds", obj)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("memory: encoding the %s: %w", obj.Kind(), err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("memory: decoding the %s: %w", obj.Kind(), err)
	}
	return out, nil
}

// rowFields are the status members every kind has and the row owns,
// reached as pointers so Put writes them back into the caller's object.
type rowFields struct {
	id, owner            *string
	version              *int64
	createdAt, updatedAt *time.Time
	labels               map[string]string
}

// fieldsOf refuses any type but the four kinds', so a foreign Object
// whose Kind says Provider is a plain error and never a row.
func fieldsOf(obj v1.Object) (rowFields, error) {
	switch x := obj.(type) {
	case *v1.Provider:
		return rowFields{&x.Status.ID, &x.Status.Owner, &x.Status.Version, &x.Status.CreatedAt, &x.Status.UpdatedAt, x.Metadata.Labels}, nil
	case *v1.Model:
		return rowFields{&x.Status.ID, &x.Status.Owner, &x.Status.Version, &x.Status.CreatedAt, &x.Status.UpdatedAt, x.Metadata.Labels}, nil
	case *v1.Key:
		return rowFields{&x.Status.ID, &x.Status.Owner, &x.Status.Version, &x.Status.CreatedAt, &x.Status.UpdatedAt, x.Metadata.Labels}, nil
	case *v1.Budget:
		return rowFields{&x.Status.ID, &x.Status.Owner, &x.Status.Version, &x.Status.CreatedAt, &x.Status.UpdatedAt, x.Metadata.Labels}, nil
	default:
		return rowFields{}, fmt.Errorf("memory: %T is not one of the four kinds", obj)
	}
}

// prepare makes the stored copy of an object the control plane's half
// alone: the observed members are cleared, a Model with no source is
// declared, and a Key's status.value, which is the create response's
// alone, is dropped so no value is ever a row.
func prepare(obj v1.Object) {
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.Health, x.Status.Discovered, x.Status.Tunnel = nil, nil, nil
	case *v1.Model:
		if x.Status.Source == "" {
			x.Status.Source = v1.SourceDeclared
		}
		x.Status.Available, x.Status.Targets = nil, nil
	case *v1.Key:
		x.Status.Value = ""
		x.Status.State, x.Status.Usage, x.Status.LastUsedAt = "", nil, time.Time{}
	case *v1.Budget:
		x.Status.State, x.Status.Spent, x.Status.Remaining, x.Status.ResetsAt, x.Status.Keys = "", nil, nil, time.Time{}, nil
	}
}

// sourceOf is a Model's source and "" for the other kinds.
func sourceOf(obj v1.Object) v1.Source {
	if m, ok := obj.(*v1.Model); ok {
		return m.Status.Source
	}
	return ""
}

// targetsOf is the Provider reference of every target of a Model, as
// written, a name or a prv_ id.
func targetsOf(obj v1.Object) []string {
	m, ok := obj.(*v1.Model)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m.Spec.Targets))
	for _, t := range m.Spec.Targets {
		out = append(out, t.Provider)
	}
	return out
}

// merge lays the kind's observed struct over the incoming one under the
// zero-member rule: a nil pointer, an empty string, or a zero time
// leaves the stored member, and a non-nil empty Targets clears the
// list. The result is a fresh value, never the stored one edited.
func merge(kind string, current, incoming any) (any, error) {
	switch kind {
	case v1.KindProvider:
		in, err := as[store.ProviderObserved](incoming, kind)
		if err != nil {
			return nil, err
		}
		cur := valueOf[store.ProviderObserved](current)
		if in.Health != nil {
			cur.Health = ptr(*in.Health)
		}
		if in.Discovered != nil {
			cur.Discovered = ptr(*in.Discovered)
		}
		if in.Tunnel != nil {
			cur.Tunnel = ptr(*in.Tunnel)
		}
		return &cur, nil
	case v1.KindModel:
		in, err := as[store.ModelObserved](incoming, kind)
		if err != nil {
			return nil, err
		}
		cur := valueOf[store.ModelObserved](current)
		if in.Available != nil {
			cur.Available = ptr(*in.Available)
		}
		if in.Targets != nil {
			cur.Targets = nil
			if len(in.Targets) > 0 {
				cur.Targets = slices.Clone(in.Targets)
			}
		}
		return &cur, nil
	case v1.KindKey:
		in, err := as[store.KeyObserved](incoming, kind)
		if err != nil {
			return nil, err
		}
		cur := valueOf[store.KeyObserved](current)
		if in.State != "" {
			cur.State = in.State
		}
		if in.Usage != nil {
			cur.Usage = copyUsage(in.Usage)
		}
		if !in.LastUsedAt.IsZero() {
			cur.LastUsedAt = in.LastUsedAt
		}
		return &cur, nil
	default:
		in, err := as[store.BudgetObserved](incoming, kind)
		if err != nil {
			return nil, err
		}
		cur := valueOf[store.BudgetObserved](current)
		if in.State != "" {
			cur.State = in.State
		}
		if in.Spent != nil {
			cur.Spent = ptr(*in.Spent)
		}
		if in.Remaining != nil {
			cur.Remaining = ptr(*in.Remaining)
		}
		if !in.ResetsAt.IsZero() {
			cur.ResetsAt = in.ResetsAt
		}
		if in.Keys != nil {
			cur.Keys = ptr(*in.Keys)
		}
		return &cur, nil
	}
}

// as reads the observed argument as the kind's struct, by value or by
// pointer, and refuses any other type with the developer's detail.
func as[T any](v any, kind string) (T, error) {
	var zero T
	switch x := v.(type) {
	case T:
		return x, nil
	case *T:
		if x == nil {
			return zero, nil
		}
		return *x, nil
	default:
		return zero, fmt.Errorf("memory: PutStatus of a %s takes %T, got %T", kind, zero, v)
	}
}

// valueOf is the stored observed half as a value to edit, or the zero
// value when the row has none yet.
func valueOf[T any](current any) T {
	if p, ok := current.(*T); ok && p != nil {
		return *p
	}
	var zero T
	return zero
}

// apply lays the stored observed half over a freshly cloned object,
// copying every pointer so the caller's object shares nothing with the
// row.
func apply(obj v1.Object, observed any) {
	switch x := obj.(type) {
	case *v1.Provider:
		if o, ok := observed.(*store.ProviderObserved); ok {
			if o.Health != nil {
				x.Status.Health = ptr(*o.Health)
			}
			if o.Discovered != nil {
				x.Status.Discovered = ptr(*o.Discovered)
			}
			if o.Tunnel != nil {
				x.Status.Tunnel = ptr(*o.Tunnel)
			}
		}
	case *v1.Model:
		if o, ok := observed.(*store.ModelObserved); ok {
			if o.Available != nil {
				x.Status.Available = ptr(*o.Available)
			}
			x.Status.Targets = slices.Clone(o.Targets)
		}
	case *v1.Key:
		if o, ok := observed.(*store.KeyObserved); ok {
			x.Status.State = o.State
			x.Status.Usage = copyUsage(o.Usage)
			x.Status.LastUsedAt = o.LastUsedAt
		}
	case *v1.Budget:
		if o, ok := observed.(*store.BudgetObserved); ok {
			x.Status.State = o.State
			if o.Spent != nil {
				x.Status.Spent = ptr(*o.Spent)
			}
			if o.Remaining != nil {
				x.Status.Remaining = ptr(*o.Remaining)
			}
			x.Status.ResetsAt = o.ResetsAt
			if o.Keys != nil {
				x.Status.Keys = ptr(*o.Keys)
			}
		}
	}
}

func copyUsage(u *v1.KeyUsage) *v1.KeyUsage {
	if u == nil {
		return nil
	}
	out := *u
	if u.Window != nil {
		out.Window = ptr(*u.Window)
	}
	return &out
}

func ptr[T any](v T) *T { return &v }
