// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"encoding/json"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The two halves of status, as spec 010's table divides them. Put
// stores the control plane's half in the status column with the
// observed members taken out; PutStatus concatenates the incoming
// members, under the same JSON names the kinds render them by, onto the
// observed column; a read concatenates the two columns and decodes the
// result as the kind. A member PutStatus leaves zero is absent from
// the incoming document and so leaves the stored one alone, and a
// non-nil empty Targets is an empty array, which replaces the list.

// observedMembers are the status members of each kind that PutStatus
// writes and Put never does, by JSON name.
var observedMembers = map[string][]string{
	v1.KindProvider: {"health", "discovered", "tunnel"},
	v1.KindModel:    {"available", "targets"},
	v1.KindKey:      {"state", "usage", "lastUsedAt"},
	v1.KindBudget:   {"state", "spent", "remaining", "resetsAt", "keys"},
}

// newObject is the empty value of a kind, for a decode.
func newObject(kind string) (v1.Object, error) {
	switch kind {
	case v1.KindProvider:
		return &v1.Provider{}, nil
	case v1.KindModel:
		return &v1.Model{}, nil
	case v1.KindKey:
		return &v1.Key{}, nil
	case v1.KindBudget:
		return &v1.Budget{}, nil
	default:
		return nil, fmt.Errorf("postgres: %q is not one of the four kinds", kind)
	}
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
		return rowFields{}, fmt.Errorf("postgres: %T is not one of the four kinds", obj)
	}
}

// encoded is one object as its row: the columns the filters read and the
// two JSON halves Put writes, each JSON column as the text it is bound as.
type encoded struct {
	kind, name, owner, source string
	labels                    string // a JSON object
	providers                 []string
	spec                      string                     // {"metadata": ..., "spec": ...}
	status                    map[string]json.RawMessage // the control plane's half
}

// encode marshals the object once, which drops the write-only values
// the encoders skip, splits it into the two halves, and takes the
// observed members and a Key's status.value out of the control half.
func encode(obj v1.Object) (*encoded, error) {
	f, err := fieldsOf(obj)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("postgres: encoding the %s: %w", obj.Kind(), err)
	}
	var doc struct {
		Metadata json.RawMessage            `json:"metadata"`
		Spec     json.RawMessage            `json:"spec"`
		Status   map[string]json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("postgres: decoding the %s: %w", obj.Kind(), err)
	}
	if doc.Status == nil {
		doc.Status = map[string]json.RawMessage{}
	}
	labels, err := labelsText(f.labels)
	if err != nil {
		return nil, err
	}
	e := &encoded{kind: obj.Kind(), name: obj.Name(), owner: obj.Owner(), status: doc.Status, labels: labels, providers: []string{}}
	for _, member := range observedMembers[e.kind] {
		delete(e.status, member)
	}
	switch x := obj.(type) {
	case *v1.Model:
		e.source = string(x.Status.Source)
		if e.source == "" {
			e.source = string(v1.SourceDeclared)
			e.status["source"] = json.RawMessage(`"` + e.source + `"`)
		}
		for _, t := range x.Spec.Targets {
			e.providers = append(e.providers, t.Provider)
		}
	case *v1.Key:
		delete(e.status, "value")
	}
	e.spec = `{"metadata":` + rawOr(doc.Metadata, "{}") + `,"spec":` + rawOr(doc.Spec, "{}") + `}`
	return e, nil
}

// stamp writes the row's members into the control half, so the stored
// document carries what the row says and a read renders it without a
// second lookup, and answers the half as the text the status column is
// bound as.
func (e *encoded) stamp(id string, version int64, createdAt, updatedAt time.Time) (string, error) {
	e.status["id"] = json.RawMessage(`"` + id + `"`)
	e.status["version"] = json.RawMessage(fmt.Sprint(version))
	for name, t := range map[string]time.Time{"createdAt": createdAt, "updatedAt": updatedAt} {
		b, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		e.status[name] = b
	}
	if e.owner != "" {
		e.status["owner"] = json.RawMessage(`"` + e.owner + `"`)
	}
	b, err := json.Marshal(e.status)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// rawOr is a raw message, or def when it is empty.
func rawOr(m json.RawMessage, def string) string {
	if len(m) == 0 {
		return def
	}
	return string(m)
}

// decode is the object a read returns: the two JSON halves, the status
// already merged with the observed column, as the kind's struct.
func decode(kind string, spec, status []byte) (v1.Object, error) {
	obj, err := newObject(kind)
	if err != nil {
		return nil, err
	}
	var parts struct {
		Metadata json.RawMessage `json:"metadata"`
		Spec     json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(spec, &parts); err != nil {
		return nil, fmt.Errorf("postgres: decoding a stored %s: %w", kind, err)
	}
	doc := `{"metadata":` + rawOr(parts.Metadata, "{}") + `,"spec":` + rawOr(parts.Spec, "{}") + `,"status":` + rawOr(status, "{}") + `}`
	if err := json.Unmarshal([]byte(doc), obj); err != nil {
		return nil, fmt.Errorf("postgres: decoding a stored %s: %w", kind, err)
	}
	return obj, nil
}

// setRow writes the row's id, version, and timestamps back into the
// caller's object, and the default source into a Model that had none.
func setRow(obj v1.Object, id string, version int64, createdAt, updatedAt time.Time) {
	f, err := fieldsOf(obj)
	if err != nil {
		return
	}
	*f.id, *f.version, *f.createdAt, *f.updatedAt = id, version, createdAt, updatedAt
	if m, ok := obj.(*v1.Model); ok && m.Status.Source == "" {
		m.Status.Source = v1.SourceDeclared
	}
}

// observedJSON renders the incoming observed struct as the document
// PutStatus concatenates onto the observed column: every non-zero
// member under its status name, nothing for a zero one. The struct is
// taken by value or by pointer; another type is refused.
func observedJSON(kind string, observed any) ([]byte, error) {
	m := map[string]any{}
	switch kind {
	case v1.KindProvider:
		in, err := as[store.ProviderObserved](observed, kind)
		if err != nil {
			return nil, err
		}
		put(m, "health", in.Health)
		put(m, "discovered", in.Discovered)
		put(m, "tunnel", in.Tunnel)
	case v1.KindModel:
		in, err := as[store.ModelObserved](observed, kind)
		if err != nil {
			return nil, err
		}
		put(m, "available", in.Available)
		if in.Targets != nil {
			m["targets"] = in.Targets
		}
	case v1.KindKey:
		in, err := as[store.KeyObserved](observed, kind)
		if err != nil {
			return nil, err
		}
		if in.State != "" {
			m["state"] = in.State
		}
		put(m, "usage", in.Usage)
		if !in.LastUsedAt.IsZero() {
			m["lastUsedAt"] = in.LastUsedAt
		}
	case v1.KindBudget:
		in, err := as[store.BudgetObserved](observed, kind)
		if err != nil {
			return nil, err
		}
		if in.State != "" {
			m["state"] = in.State
		}
		put(m, "spent", in.Spent)
		put(m, "remaining", in.Remaining)
		if !in.ResetsAt.IsZero() {
			m["resetsAt"] = in.ResetsAt
		}
		put(m, "keys", in.Keys)
	default:
		return nil, fmt.Errorf("postgres: PutStatus of kind %q, not one of the four", kind)
	}
	return json.Marshal(m)
}

// put adds a pointer member when it is set.
func put[T any](m map[string]any, name string, p *T) {
	if p != nil {
		m[name] = *p
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
		return zero, fmt.Errorf("postgres: PutStatus of a %s takes %T, got %T", kind, zero, v)
	}
}
