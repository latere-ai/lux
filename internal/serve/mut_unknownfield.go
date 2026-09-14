// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_unknownfield

package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The mutation of spec 018's fourth row: a manifest field the schema
// does not know is accepted, by dropping it from the body before the API
// decodes it, path by path, until the manifest decodes. Only
// case003UnknownField may redden.
func init() {
	mutateHandler = func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/") {
				if body, ok := dropUnknownFields(r); ok {
					r.Body = io.NopCloser(bytes.NewReader(body))
					r.ContentLength = int64(len(body))
					r.Header.Set("Content-Type", "application/json")
				}
			}
			h.ServeHTTP(w, r)
		})
	}
}

// plurals maps the route's segment to the kind the hint names.
var plurals = map[string]string{"providers": v1.KindProvider, "models": v1.KindModel, "keys": v1.KindKey, "budgets": v1.KindBudget}

// dropUnknownFields reads the body, decodes it as the API would, and
// deletes every path an unknown_field refusal names; ok reports that the
// body changed.
func dropUnknownFields(r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	segments := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/"), "/", 2)
	kind, known := plurals[segments[0]]
	if !known || len(segments) < 2 {
		return nil, false
	}
	hint := manifest.Hint{APIVersion: v1.APIVersion, Kind: kind, Name: segments[1]}
	var tree map[string]any
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		if json.Unmarshal(raw, &tree) != nil {
			return nil, false
		}
	case strings.HasPrefix(ct, "application/yaml"), strings.HasPrefix(ct, "application/x-yaml"), strings.HasPrefix(ct, "text/yaml"):
		if yaml.Unmarshal(raw, &tree) != nil {
			return nil, false
		}
	default:
		return nil, false // a content type the API refuses is refused as it is
	}
	changed := false
	for range 16 {
		body, err := json.Marshal(tree)
		if err != nil {
			return nil, false
		}
		_, derr := manifest.Decode(body, "application/json", hint)
		var me *manifest.Error
		if !errors.As(derr, &me) || me.Code != manifest.CodeUnknownField || len(me.Paths) == 0 {
			if changed {
				return body, true
			}
			return nil, false
		}
		if !deletePath(tree, me.Paths[0]) {
			return nil, false
		}
		changed = true
	}
	return nil, false
}

// deletePath removes the member a dotted path with [n] indexes names.
func deletePath(tree map[string]any, path string) bool {
	var cur any = tree
	parts := splitPath(path)
	for i, part := range parts {
		last := i == len(parts)-1
		if idx, err := strconv.Atoi(part); err == nil {
			list, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(list) {
				return false
			}
			if last {
				return false // a list entry is a value, not a field
			}
			cur = list[idx]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if last {
			if _, present := m[part]; !present {
				return false
			}
			delete(m, part)
			return true
		}
		cur = m[part]
	}
	return false
}

// splitPath turns spec.targets[0].weigth into spec, targets, 0, weigth.
func splitPath(path string) []string {
	var out []string
	for part := range strings.SplitSeq(path, ".") {
		for {
			open := strings.IndexByte(part, '[')
			if open < 0 {
				if part != "" {
					out = append(out, part)
				}
				break
			}
			if open > 0 {
				out = append(out, part[:open])
			}
			end := strings.IndexByte(part, ']')
			if end < open {
				break
			}
			out = append(out, strings.Trim(part[open+1:end], `"`))
			part = part[end+1:]
		}
	}
	return out
}
