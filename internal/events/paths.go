// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"

	v1 "latere.ai/x/lux/manifest/v1"
)

// ChangedPaths lists the JSON paths of metadata and spec that differ
// between two objects, dotted for a struct member and [n] for a list
// entry, in path order. The comparison is over each object's JSON, so
// the write-only members, which no encoding carries, never appear: a
// Provider whose credential alone changed reports no path. It is the
// data of every <kind>.updated row, whoever raises it.
func ChangedPaths(old, obj v1.Object) []string {
	var out []string
	diff("", controlTree(old), controlTree(obj), &out)
	slices.Sort(out)
	return out
}

// controlTree is the object's metadata and spec as a JSON tree.
func controlTree(obj v1.Object) map[string]any {
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
