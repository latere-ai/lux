// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// parseJSON parses one JSON object into the same generic tree parseYAML
// builds, token by token, so a duplicate key is refused rather than
// silently taken last, and the same two bounds apply to both formats.
func parseJSON(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	w := &jsonWalk{dec: dec, body: body}
	v, err := w.value("", 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, refuse(CodeMalformedBody, "json: more than one value in the body")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, refuse(CodeMalformedBody, "json: the body is not an object")
	}
	return m, nil
}

type jsonWalk struct {
	dec    *json.Decoder
	body   []byte
	bounds bounds
}

// malformed renders a decoder error with the line and column of its
// offset, or of the end of the body when it ended too soon.
func (w *jsonWalk) malformed(err error) *Error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		// Offset counts the bytes read, so the byte that failed is the
		// one before it.
		line, col := lineCol(w.body, int(syntax.Offset)-1)
		return refuse(CodeMalformedBody, "json: "+syntax.Error()+" at "+at(line, col))
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		line, col := lineCol(w.body, len(w.body))
		return refuse(CodeMalformedBody, "json: the body ends before the value does, at "+at(line, col))
	}
	return refuse(CodeMalformedBody, "json: "+err.Error())
}

func (w *jsonWalk) value(path string, depth int) (any, error) {
	tok, err := w.dec.Token()
	if err != nil {
		return nil, w.malformed(err)
	}
	return w.from(tok, path, depth)
}

func (w *jsonWalk) from(tok json.Token, path string, depth int) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			if err := deeper(path, depth+1); err != nil {
				return nil, err
			}
			return w.object(path, depth+1)
		case '[':
			if err := deeper(path, depth+1); err != nil {
				return nil, err
			}
			return w.array(path, depth+1)
		default:
			return nil, refuse(CodeMalformedBody, "json: unexpected "+string(t))
		}
	case string:
		return t, w.bounds.scalar(path, len(t))
	case json.Number:
		return t, w.bounds.scalar(path, len(t))
	case bool:
		return t, nil
	default:
		return nil, nil
	}
}

func (w *jsonWalk) object(path string, depth int) (map[string]any, error) {
	m := map[string]any{}
	for {
		tok, err := w.dec.Token()
		if err != nil {
			return nil, w.malformed(err)
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return m, nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil, refuse(CodeMalformedBody, "json: an object key is not a string")
		}
		if _, dup := m[key]; dup {
			return nil, refuse(CodeMalformedBody, "json: duplicate key "+strconv.Quote(key)+" in "+objectName(path))
		}
		if err := w.bounds.scalar(fieldPath(path, key), len(key)); err != nil {
			return nil, err
		}
		v, err := w.value(fieldPath(path, key), depth)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
}

func (w *jsonWalk) array(path string, depth int) ([]any, error) {
	out := []any{}
	for i := 0; ; i++ {
		tok, err := w.dec.Token()
		if err != nil {
			return nil, w.malformed(err)
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return out, nil
		}
		v, err := w.from(tok, indexPath(path, i), depth)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func objectName(path string) string {
	if path == "" {
		return "the document"
	}
	return path
}
