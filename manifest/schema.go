// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The scalar types the schema spells as strings with a syntax of their
// own, checked here so the refusal carries the field's path.
var (
	timeType  = reflect.TypeFor[time.Time]()
	moneyType = reflect.TypeFor[v1.Money]()
)

// schemaWalk holds a generic tree to a kind's Go type: the json tags of
// the type are the schema, the same tags encoding/json reads, so one
// definition decides every field question and the path is spelled the
// same from a YAML and a JSON body. A write-only field, an unexported
// member with a writeonly tag, is taken out of the tree into writeOnly by
// path, because encoding/json will not set it.
type schemaWalk struct {
	writeOnly map[string]string
}

// field is one key the schema admits under a struct.
type field struct {
	typ       reflect.Type
	writeOnly bool
}

// fieldsOf reads the schema of one struct type from its tags.
func fieldsOf(t reflect.Type) map[string]field {
	out := map[string]field{}
	for i := range t.NumField() {
		f := t.Field(i)
		if wo, ok := f.Tag.Lookup("writeonly"); ok {
			out[wo] = field{typ: f.Type, writeOnly: true}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out[name] = field{typ: f.Type}
	}
	return out
}

// object checks the keys of one mapping against a struct type.
func (w *schemaWalk) object(m map[string]any, t reflect.Type, path string) error {
	fields := fieldsOf(t)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		f, ok := fields[k]
		if !ok {
			return refuse(CodeUnknownField, "the schema of "+t.Name()+" has no field "+k, fieldPath(path, k))
		}
		if f.writeOnly {
			if m[k] == nil {
				delete(m, k)
				continue
			}
			s, ok := m[k].(string)
			if !ok {
				return refuse(CodeInvalidField, "expected a string", fieldPath(path, k))
			}
			w.writeOnly[fieldPath(path, k)] = s
			delete(m, k)
			continue
		}
		if err := w.value(fieldPath(path, k), m[k], f.typ); err != nil {
			return err
		}
	}
	return nil
}

// value checks one tree value against the Go type it decodes into.
func (w *schemaWalk) value(path string, v any, t reflect.Type) error {
	if v == nil {
		return nil // null is absent
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		s, ok := v.(string)
		if !ok {
			return refuse(CodeInvalidField, "expected an RFC 3339 timestamp", path)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return refuse(CodeInvalidField, "not an RFC 3339 timestamp: "+err.Error(), path)
		}
		return nil
	case moneyType:
		s, ok := v.(string)
		if !ok {
			return refuse(CodeInvalidField, "money is a decimal string, not a number", path)
		}
		if _, err := v1.ParseMoney(s); err != nil {
			return refuse(CodeInvalidField, err.Error(), path)
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return refuse(CodeInvalidField, "expected an object", path)
		}
		return w.object(m, t, path)
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return refuse(CodeInvalidField, "expected an object", path)
		}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if err := w.value(keyPath(path, k), m[k], t.Elem()); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return refuse(CodeInvalidField, "expected a list", path)
		}
		for i, item := range a {
			if err := w.value(indexPath(path, i), item, t.Elem()); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		if _, ok := v.(string); !ok {
			return refuse(CodeInvalidField, "expected a string", path)
		}
		return nil
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			return refuse(CodeInvalidField, "expected true or false", path)
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := v.(json.Number)
		if !ok {
			return refuse(CodeInvalidField, "expected an integer", path)
		}
		i, err := n.Int64()
		if err != nil {
			return refuse(CodeInvalidField, "expected an integer, not "+string(n), path)
		}
		if reflect.Zero(t).OverflowInt(i) {
			return refuse(CodeInvalidField, string(n)+" is out of range", path)
		}
		return nil
	default:
		return nil
	}
}
