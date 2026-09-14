// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// document is GET /v1/openapi.json as the suite reads it: the path items
// and the component schemas, enough to hold every /v1 answer to the
// schema its operation names. The vocabulary checked is the one spec
// 011's generator writes: type, properties, required,
// additionalProperties, items, enum, const, pattern, format date-time,
// minimum, maximum, and $ref into the components; readOnly, writeOnly,
// description, and default carry no constraint.
type document struct {
	paths   map[string]any
	schemas map[string]any
}

// parseDocument reads the document.
func parseDocument(data []byte) (*document, error) {
	var doc struct {
		OpenAPI    string         `json:"openapi"`
		Paths      map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") || len(doc.Paths) == 0 {
		return nil, errors.New("the document is not an OpenAPI 3 document with paths")
	}
	return &document{paths: doc.Paths, schemas: doc.Components.Schemas}, nil
}

// operation finds the path item and operation for a request, or nil when
// the document names no such route, which is itself reported by the
// caller: a path the server answered and the document does not describe
// is a drift.
func (d *document) operation(method, path string) map[string]any {
	var best map[string]any
	bestLiteral := -1
	for template, item := range d.paths {
		literal, ok := matchTemplate(template, path)
		if !ok || literal <= bestLiteral {
			continue
		}
		it, _ := item.(map[string]any)
		op, _ := it[strings.ToLower(method)].(map[string]any)
		if op == nil {
			continue
		}
		best, bestLiteral = op, literal
	}
	return best
}

// matchTemplate matches a path against a template: a literal segment
// matches itself, {name} matches one segment, and under /v1/models it
// matches every remaining segment, since a Model name carries slashes.
// literal counts the literal segments matched, so /v1/keys/{name}/rotate
// wins over a wider template.
func matchTemplate(template, path string) (literal int, ok bool) {
	ts, ps := strings.Split(template, "/"), strings.Split(path, "/")
	models := strings.HasPrefix(template, "/v1/models/")
	for i, seg := range ts {
		if i >= len(ps) {
			return 0, false
		}
		if strings.HasPrefix(seg, "{") {
			if models && i == len(ts)-1 {
				return literal, true // the rest is the name
			}
			continue
		}
		if seg != ps[i] {
			return 0, false
		}
		literal++
	}
	return literal, len(ts) == len(ps)
}

// validate holds one answer to its operation's response: the body
// against the schema of the status, or the default response for a
// refusal. A 204 carries no body. The problems are one line each.
func (d *document) validate(method, path string, status int, contentType string, body []byte) []string {
	op := d.operation(method, path)
	if op == nil {
		// A method the table does not list is answered not_found in the
		// envelope, which is the default response of every operation;
		// the path item alone is enough to hold it.
		return d.validateDefault(path, status, contentType, body)
	}
	responses, _ := op["responses"].(map[string]any)
	resp, _ := responses[strconv.Itoa(status)].(map[string]any)
	if resp == nil {
		resp, _ = responses["default"].(map[string]any)
	}
	if resp == nil {
		return []string{"the operation describes no response for " + strconv.Itoa(status)}
	}
	return d.validateResponse(resp, status, contentType, body)
}

// validateDefault holds an answer on a path the document has but whose
// method it does not list, and an answer on a path it lacks, to the
// Error schema, since a refusal is the one body either can carry.
func (d *document) validateDefault(path string, status int, contentType string, body []byte) []string {
	if status < 400 {
		return []string{"no operation in the document describes this route, and the answer is not a refusal"}
	}
	return d.validateResponse(map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}}, status, contentType, body)
}

func (d *document) validateResponse(resp map[string]any, status int, contentType string, body []byte) []string {
	content, _ := resp["content"].(map[string]any)
	if len(content) == 0 {
		if len(body) > 0 {
			return []string{"the response is described without a body and carries one"}
		}
		return nil
	}
	if status == 204 {
		return nil
	}
	media, _ := content["application/json"].(map[string]any)
	if media == nil {
		return nil
	}
	if !strings.HasPrefix(contentType, "application/json") {
		return []string{"Content-Type " + strconv.Quote(contentType) + " on a response the document describes as application/json"}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return []string{"the body is not JSON: " + err.Error()}
	}
	schema, _ := media["schema"].(map[string]any)
	var problems []string
	d.check(schema, value, "$", &problems)
	return problems
}

// resolve follows a $ref into the components.
func (d *document) resolve(schema map[string]any) map[string]any {
	for range 8 {
		ref, ok := schema["$ref"].(string)
		if !ok {
			return schema
		}
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		next, _ := d.schemas[name].(map[string]any)
		if next == nil {
			return map[string]any{"x-unresolved": ref}
		}
		schema = next
	}
	return schema
}

// check validates value against schema at path, appending problems.
func (d *document) check(schema map[string]any, value any, path string, problems *[]string) {
	schema = d.resolve(schema)
	if ref, ok := schema["x-unresolved"].(string); ok {
		*problems = append(*problems, path+": "+ref+" resolves to nothing")
		return
	}
	if c, ok := schema["const"]; ok && !equalJSON(c, value) {
		*problems = append(*problems, path+": "+render(value)+" is not the constant "+render(c))
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if equalJSON(e, value) {
				found = true
				break
			}
		}
		if !found {
			*problems = append(*problems, path+": "+render(value)+" is not in the enum")
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			*problems = append(*problems, path+": "+render(value)+" is not an object")
			return
		}
		for _, r := range toStrings(schema["required"]) {
			if _, ok := m[r]; !ok {
				*problems = append(*problems, path+": the required member "+r+" is absent")
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, v := range m {
			if ps, ok := props[k].(map[string]any); ok {
				d.check(ps, v, path+"."+k, problems)
				continue
			}
			switch ap := schema["additionalProperties"].(type) {
			case bool:
				if !ap {
					*problems = append(*problems, path+": the member "+k+" is not in the schema")
				}
			case map[string]any:
				d.check(ap, v, path+"."+k, problems)
			}
		}
	case "array":
		a, ok := value.([]any)
		if !ok {
			*problems = append(*problems, path+": "+render(value)+" is not an array")
			return
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, v := range a {
				d.check(items, v, path+"["+strconv.Itoa(i)+"]", problems)
			}
		}
	case "string":
		s, ok := value.(string)
		if !ok {
			*problems = append(*problems, path+": "+render(value)+" is not a string")
			return
		}
		if p, ok := schema["pattern"].(string); ok {
			if re, err := regexp.Compile(p); err == nil && !re.MatchString(s) {
				*problems = append(*problems, path+": "+strconv.Quote(s)+" does not match "+p)
			}
		}
		if f, _ := schema["format"].(string); f == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				*problems = append(*problems, path+": "+strconv.Quote(s)+" is not RFC 3339")
			}
		}
	case "integer", "number":
		n, ok := value.(json.Number)
		if !ok {
			*problems = append(*problems, path+": "+render(value)+" is not a number")
			return
		}
		if typ == "integer" && strings.ContainsAny(n.String(), ".eE") {
			*problems = append(*problems, path+": "+n.String()+" is not an integer")
		}
		f, _ := n.Float64()
		if lo, ok := numberOf(schema["minimum"]); ok && f < lo {
			*problems = append(*problems, path+": "+n.String()+" is below the minimum")
		}
		if hi, ok := numberOf(schema["maximum"]); ok && f > hi {
			*problems = append(*problems, path+": "+n.String()+" is above the maximum")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			*problems = append(*problems, path+": "+render(value)+" is not a boolean")
		}
	}
}

func toStrings(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, e := range a {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// equalJSON compares two decoded values by their JSON rendering.
func equalJSON(a, b any) bool { return render(a) == render(b) }

func render(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(data)
}
