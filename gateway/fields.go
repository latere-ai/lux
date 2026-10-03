// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// mergeRequestFields writes a Provider's requestFields into a request
// body (spec 042). The body's top-level object takes every member of
// fields: an object value is merged into the body's member of the same
// name by the same rule, starting from an empty object when the body's
// member is absent or is not an object; any other value, a list
// included, replaces the body's member whole, because no element of a
// list is identified across the two. A body member whose name equals a
// member of fields under case folding, and is not that name, is
// removed, so a reader that matches names case-insensitively reads the
// Provider's value and not the caller's.
//
// Each object the merge writes into is written again with its members
// sorted by name and a duplicate member collapsed to its last
// occurrence, which is the one bridge.Probe reads; every value the
// merge does not reach keeps its bytes. A body that is not a JSON
// object, an empty one included, is returned unchanged.
func mergeRequestFields(body []byte, fields map[string]any) ([]byte, error) {
	top, ok := jsonObject(body)
	if !ok {
		return body, nil
	}
	return mergeObject(top, fields)
}

// jsonObject decodes b as one JSON object with its member values left
// undecoded, and reports whether b is one: null, any other value, and
// bytes that are not JSON are not.
func jsonObject(b []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(b, &object) != nil {
		return nil, false
	}
	return object, object != nil
}

// mergeObject merges fields into one decoded object and encodes it.
func mergeObject(object map[string]json.RawMessage, fields map[string]any) ([]byte, error) {
	for name, value := range fields {
		for member := range object {
			if member != name && strings.EqualFold(member, name) {
				delete(object, member)
			}
		}
		var (
			raw []byte
			err error
		)
		if nested, ok := value.(map[string]any); ok {
			inner, isObject := jsonObject(object[name])
			if !isObject {
				inner = map[string]json.RawMessage{}
			}
			raw, err = mergeObject(inner, nested)
		} else {
			raw, err = encodeJSON(value)
		}
		if err != nil {
			return nil, err
		}
		object[name] = raw
	}
	return encodeObject(object)
}

// encodeObject writes one object with its members sorted by name, each
// value as it stands, so a value the merge did not reach is written
// with the bytes it was read with.
func encodeObject(object map[string]json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, name := range slices.Sorted(maps.Keys(object)) {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := encodeJSON(name)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(object[name])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// encodeJSON encodes one value of a Provider's requestFields, or a
// member name, compact and without escaping <, >, and &.
func encodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
