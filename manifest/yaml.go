// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// parseYAML parses one YAML document into a generic tree of
// map[string]any, []any, string, json.Number, bool, and nil, resolving
// anchors, aliases, and merge keys itself so the expansion is counted
// against MaxScalarBytes as it happens rather than after it has happened.
func parseYAML(body []byte) (map[string]any, error) {
	file, err := parser.ParseBytes(body, 0)
	if err != nil {
		return nil, malformedYAML(err)
	}
	if len(file.Docs) > 1 {
		return nil, refuse(CodeMultiDocument, "the body carries "+strconv.Itoa(len(file.Docs))+" YAML documents")
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return nil, refuse(CodeMalformedBody, "the document is empty")
	}
	w := &yamlWalk{anchors: map[string]ast.Node{}}
	v, err := w.node(file.Docs[0].Body, "", 0)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, refuse(CodeMalformedBody, "the document is not a mapping")
	}
	return m, nil
}

// malformedYAML renders the parser's error with its line and column.
func malformedYAML(err error) *Error {
	var syntax *yaml.SyntaxError
	if errors.As(err, &syntax) && syntax.Token != nil && syntax.Token.Position != nil {
		p := syntax.Token.Position
		return refuse(CodeMalformedBody, "yaml: "+syntax.Message+" at "+at(p.Line, p.Column))
	}
	return refuse(CodeMalformedBody, "yaml: "+err.Error())
}

// yamlWalk converts the AST, one node at a time, under the two bounds.
type yamlWalk struct {
	anchors map[string]ast.Node
	bounds  bounds
}

// position renders a node's position for a developer detail.
func position(n ast.Node) string {
	if tk := n.GetToken(); tk != nil && tk.Position != nil {
		return " at " + at(tk.Position.Line, tk.Position.Column)
	}
	return ""
}

func (w *yamlWalk) node(n ast.Node, path string, depth int) (any, error) {
	switch n := n.(type) {
	case *ast.MappingNode:
		if err := deeper(path, depth+1); err != nil {
			return nil, err
		}
		return w.mapping(n.Values, path, depth+1)
	case *ast.MappingValueNode:
		// A mapping of one pair is parsed as the pair itself.
		if err := deeper(path, depth+1); err != nil {
			return nil, err
		}
		return w.mapping([]*ast.MappingValueNode{n}, path, depth+1)
	case *ast.SequenceNode:
		if err := deeper(path, depth+1); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(n.Values))
		for i, v := range n.Values {
			child, err := w.node(v, indexPath(path, i), depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, child)
		}
		return out, nil
	case *ast.AnchorNode:
		v, err := w.node(n.Value, path, depth)
		if err != nil {
			return nil, err
		}
		// Registered after its value is walked, so an alias inside its
		// own anchor is an alias with no anchor and not a cycle.
		w.anchors[n.Name.GetToken().Value] = n.Value
		return v, nil
	case *ast.AliasNode:
		name := n.Value.GetToken().Value
		target, ok := w.anchors[name]
		if !ok {
			return nil, refuse(CodeMalformedBody, "yaml: alias *"+name+" names no anchor"+position(n))
		}
		return w.node(target, path, depth)
	case *ast.TagNode:
		if n.Start != nil && n.Start.Value == "!!str" {
			s := n.Value.GetToken().Value
			return s, w.bounds.scalar(path, len(s))
		}
		return w.node(n.Value, path, depth)
	case *ast.LiteralNode:
		return n.Value.Value, w.bounds.scalar(path, len(n.Value.Value))
	case *ast.StringNode:
		return n.Value, w.bounds.scalar(path, len(n.Value))
	case *ast.IntegerNode:
		var s string
		switch v := n.Value.(type) {
		case int64:
			s = strconv.FormatInt(v, 10)
		case uint64:
			s = strconv.FormatUint(v, 10)
		default:
			s = n.GetToken().Value
		}
		return json.Number(s), w.bounds.scalar(path, len(s))
	case *ast.FloatNode:
		s := strconv.FormatFloat(n.Value, 'g', -1, 64)
		return json.Number(s), w.bounds.scalar(path, len(s))
	case *ast.BoolNode:
		return n.Value, nil
	case *ast.NullNode:
		return nil, nil
	case *ast.InfinityNode, *ast.NanNode:
		return nil, refuse(CodeInvalidField, "JSON has no infinity or NaN"+position(n), path)
	default:
		return nil, refuse(CodeMalformedBody, "yaml: unexpected "+n.Type().String()+" node"+position(n))
	}
}

// mapping builds an object from its pairs: explicit keys first, with a
// duplicate refused, then the merge keys, which add only what is absent,
// so an explicit key wins over a merged one as YAML says it does.
func (w *yamlWalk) mapping(pairs []*ast.MappingValueNode, path string, depth int) (map[string]any, error) {
	m := make(map[string]any, len(pairs))
	var merges []ast.Node
	for _, kv := range pairs {
		if kv.Key.IsMergeKey() {
			merges = append(merges, kv.Value)
			continue
		}
		key, err := w.key(kv.Key)
		if err != nil {
			return nil, err
		}
		if _, dup := m[key]; dup {
			return nil, refuse(CodeMalformedBody, "yaml: duplicate key "+strconv.Quote(key)+position(kv.Key))
		}
		if err := w.bounds.scalar(fieldPath(path, key), len(key)); err != nil {
			return nil, err
		}
		v, err := w.node(kv.Value, fieldPath(path, key), depth)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
	for _, mn := range merges {
		v, err := w.node(mn, path, depth)
		if err != nil {
			return nil, err
		}
		if err := mergeInto(m, v, mn); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// mergeInto applies one merge value, a mapping or a list of mappings.
func mergeInto(m map[string]any, v any, n ast.Node) error {
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			if _, ok := m[k]; !ok {
				m[k] = val
			}
		}
		return nil
	case []any:
		for _, item := range v {
			if err := mergeInto(m, item, n); err != nil {
				return err
			}
		}
		return nil
	default:
		return refuse(CodeMalformedBody, "yaml: a merge key takes a mapping or a list of mappings"+position(n))
	}
}

// key turns a mapping key into the string JSON needs: a string as it is,
// a number or a bool as its text, anything else refused.
func (w *yamlWalk) key(k ast.MapKeyNode) (string, error) {
	switch k := k.(type) {
	case *ast.StringNode:
		return k.Value, nil
	case *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode:
		return k.GetToken().Value, nil
	case *ast.MappingKeyNode:
		if inner, ok := k.Value.(ast.MapKeyNode); ok {
			return w.key(inner)
		}
	case *ast.TagNode:
		if inner, ok := k.Value.(ast.MapKeyNode); ok {
			return w.key(inner)
		}
	}
	return "", refuse(CodeMalformedBody, "yaml: a key must be a string"+position(k))
}
