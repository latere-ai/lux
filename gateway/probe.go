// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// probe is what the JSON probe of stage 5 reads from a body: the
// top-level model and stream members and the dialect's output-token
// member, which is also what the reservation of spec 007 reads.
type probe struct {
	model     string
	hasModel  bool
	stream    bool
	maxTokens int64 // max_tokens, max_completion_tokens, max_output_tokens, or generationConfig.maxOutputTokens; 0 when absent
}

// probeBody reads the probe's members. A body that is not a JSON object
// is an error; a member of the wrong type is read as absent, because the
// dialect's own server owns the verdict on it.
func probeBody(body []byte) (probe, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return probe{}, fmt.Errorf("the body is not a JSON object: %w", err)
	}
	if top == nil {
		return probe{}, errors.New("the body is null, not a JSON object")
	}
	var p probe
	if raw, ok := top["model"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			p.model, p.hasModel = s, true
		}
	}
	if raw, ok := top["stream"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			p.stream = b
		}
	}
	for _, member := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if raw, ok := top[member]; ok {
			var n int64
			if json.Unmarshal(raw, &n) == nil && n > 0 {
				p.maxTokens = n
				break
			}
		}
	}
	if raw, ok := top["generationConfig"]; ok && p.maxTokens == 0 {
		var gc struct {
			MaxOutputTokens int64 `json:"maxOutputTokens"`
		}
		if json.Unmarshal(raw, &gc) == nil && gc.MaxOutputTokens > 0 {
			p.maxTokens = gc.MaxOutputTokens
		}
	}
	return p, nil
}

// The JSON member edits below are the two changes a passthrough body
// admits: the model name and stream_options.include_usage. They splice
// bytes rather than re-encode, so every byte but the one member is the
// caller's or the provider's.

// errNotObject is a body whose top level is not an object.
var errNotObject = errors.New("not a JSON object")

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipSpace(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	return i
}

// skipString returns the index after the string that opens at b[i].
func skipString(b []byte, i int) (int, error) {
	if i >= len(b) || b[i] != '"' {
		return 0, errors.New("expected a string")
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, errors.New("unterminated string")
}

// skipValue returns the index after the value that starts at b[i].
func skipValue(b []byte, i int) (int, error) {
	i = skipSpace(b, i)
	if i >= len(b) {
		return 0, errors.New("expected a value")
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, err := skipString(b, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated object or array")
	default:
		j := i
		for j < len(b) && !isSpace(b[j]) && b[j] != ',' && b[j] != '}' && b[j] != ']' {
			j++
		}
		if j == i {
			return 0, errors.New("expected a value")
		}
		return j, nil
	}
}

// member finds the member named key of the object that opens at b[start]
// and returns the span of its value, or ok false when the object has no
// such member.
func member(b []byte, start int, key string) (valStart, valEnd int, ok bool, err error) {
	i := skipSpace(b, start)
	if i >= len(b) || b[i] != '{' {
		return 0, 0, false, errNotObject
	}
	i++
	for {
		i = skipSpace(b, i)
		if i >= len(b) {
			return 0, 0, false, errors.New("unterminated object")
		}
		if b[i] == '}' {
			return 0, 0, false, nil
		}
		if b[i] == ',' {
			i++
			continue
		}
		keyEnd, err := skipString(b, i)
		if err != nil {
			return 0, 0, false, err
		}
		var k string
		if err := json.Unmarshal(b[i:keyEnd], &k); err != nil {
			return 0, 0, false, err
		}
		i = skipSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return 0, 0, false, errors.New("expected a colon")
		}
		vs := skipSpace(b, i+1)
		ve, err := skipValue(b, vs)
		if err != nil {
			return 0, 0, false, err
		}
		if k == key {
			return vs, ve, true, nil
		}
		i = ve
	}
}

// splice replaces b[start:end] with repl in a fresh slice.
func splice(b []byte, start, end int, repl []byte) []byte {
	out := make([]byte, 0, len(b)-(end-start)+len(repl))
	out = append(out, b[:start]...)
	out = append(out, repl...)
	return append(out, b[end:]...)
}

// insertMember writes `"key": value` as the first member of the object
// that opens at b[objStart], with a comma when the object is not empty.
func insertMember(b []byte, objStart int, key string, value []byte) []byte {
	i := skipSpace(b, objStart) + 1
	next := skipSpace(b, i)
	m := append([]byte(strconv.Quote(key)+":"), value...)
	if next < len(b) && b[next] != '}' {
		m = append(m, ',')
	}
	return splice(b, i, i, m)
}

// rewriteModel replaces the value of the top-level model member, or of
// message.model when the top level has none, with name, and returns the
// body unchanged when neither is present or the member is not a string.
func rewriteModel(b []byte, name string) []byte {
	repl := []byte(strconv.Quote(name))
	if vs, ve, ok, err := member(b, 0, "model"); err == nil && ok && b[vs] == '"' {
		return splice(b, vs, ve, repl)
	}
	if ms, _, ok, err := member(b, 0, "message"); err == nil && ok {
		if vs, ve, ok, err := member(b, ms, "model"); err == nil && ok && b[vs] == '"' {
			return splice(b, vs, ve, repl)
		}
	}
	return b
}

// setIncludeUsage sets stream_options.include_usage to true, replacing
// the member when present and inserting stream_options, or the member
// inside it, when absent. A body the scanner cannot read is returned
// unchanged.
func setIncludeUsage(b []byte) []byte {
	vs, ve, ok, err := member(b, 0, "stream_options")
	if err != nil {
		return b
	}
	if !ok || b[vs] != '{' {
		if ok {
			return splice(b, vs, ve, []byte(`{"include_usage":true}`))
		}
		return insertMember(b, 0, "stream_options", []byte(`{"include_usage":true}`))
	}
	is, ie, found, err := member(b, vs, "include_usage")
	if err != nil {
		return b
	}
	if found {
		return splice(b, is, ie, []byte("true"))
	}
	return insertMember(b, vs, "include_usage", []byte("true"))
}
