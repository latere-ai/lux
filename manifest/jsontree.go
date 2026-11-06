// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strconv"
)

// parseJSON parses one JSON object into the same generic tree parseYAML
// builds, token by token, so a duplicate key is refused rather than
// silently taken last, and the same two bounds apply to both formats.
func parseJSON(body []byte) (map[string]any, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(body), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	w := &jsonWalk{dec: dec, body: body}
	v, err := w.value("", 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, refuse(CodeMalformedBody, "json: more than one value in the body")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, refuse(CodeMalformedBody, "json: the body is not an object")
	}
	return m, nil
}

type jsonWalk struct {
	dec    *jsontext.Decoder
	body   []byte
	bounds bounds
}

// malformed renders a decoder error with the line and column of its
// offset, or of the end of the body when it ended too soon.
func (w *jsonWalk) malformed(err error) *Error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		line, col := lineCol(w.body, len(w.body))
		return refuse(CodeMalformedBody, "json: the body ends before the value does, at "+at(line, col))
	}
	if syntax, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
		// ByteOffset is the position of the byte that failed.
		line, col := lineCol(w.body, int(syntax.ByteOffset))
		return refuse(CodeMalformedBody, "json: "+syntax.Error()+" at "+at(line, col))
	}
	return refuse(CodeMalformedBody, "json: "+err.Error())
}

func (w *jsonWalk) value(path string, depth int) (any, error) {
	tok, err := w.dec.ReadToken()
	if err != nil {
		return nil, w.malformed(err)
	}
	return w.from(tok, path, depth)
}

func (w *jsonWalk) from(tok jsontext.Token, path string, depth int) (any, error) {
	switch tok.Kind() {
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
	case '}', ']':
		return nil, refuse(CodeMalformedBody, "json: unexpected "+tok.String())
	case '"':
		t := tok.String()
		return t, w.bounds.scalar(path, len(t))
	case '0':
		t := tok.String()
		return jsontext.Value(t), w.bounds.scalar(path, len(t))
	case 't', 'f':
		return tok.Bool(), nil
	default:
		return nil, nil
	}
}

func (w *jsonWalk) object(path string, depth int) (map[string]any, error) {
	m := map[string]any{}
	for {
		tok, err := w.dec.ReadToken()
		if err != nil {
			return nil, w.malformed(err)
		}
		if tok.Kind() == '}' {
			return m, nil
		}
		if tok.Kind() != '"' {
			return nil, refuse(CodeMalformedBody, "json: an object key is not a string")
		}
		key := tok.String()
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
		tok, err := w.dec.ReadToken()
		if err != nil {
			return nil, w.malformed(err)
		}
		if tok.Kind() == ']' {
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
