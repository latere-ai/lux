// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import "encoding/json"

// The document this front serves at GET /v1/openapi.json, which is what
// a generated client is built from and what the conformance suite holds
// every answer of a run to. It describes what this front serves and no
// more: a platform that adds a route adds it here, and a platform that
// generates its API from types generates this too.

// openAPIDocument builds the document for one public URL.
func openAPIDocument(base string) []byte {
	paths := map[string]any{
		"/.well-known/lux": map[string]any{"get": operation(objectResponse("200"))},
		"/v1/openapi.json": map[string]any{"get": operation(objectResponse("200"))},
		"/v1/self":         map[string]any{"get": operation(objectResponse("200"))},
		"/v1/usage":        map[string]any{"get": operation(rowsResponse())},
		"/v1/requests":     map[string]any{"get": operation(recordsResponse())},
		"/v1/keys/{name}/rotate": map[string]any{
			"post": operation(refResponse("200", "Key")),
		},
	}
	for kind, plural := range plurals {
		paths["/v1/"+plural] = map[string]any{"get": operation(listResponse(kind))}
		paths["/v1/"+plural+"/{name}"] = map[string]any{
			"get":    operation(refResponse("200", kind)),
			"put":    operation(refResponse("200", kind), refResponse("201", kind)),
			"delete": operation(map[string]any{"204": map[string]any{"description": "deleted"}}),
		}
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "a platform front built on the lux packages",
			"version":     "v1beta1",
			"description": "The four kinds, the usage surface, and the doors, served by examples/plane.",
		},
		"servers": []any{map[string]any{"url": base}},
		"paths":   paths,
		"components": map[string]any{"schemas": map[string]any{
			"Provider": kindSchema(),
			"Model":    kindSchema(),
			"Key":      kindSchema(),
			"Budget":   kindSchema(),
			"Error":    errorSchema(),
		}},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		panic("plane: the OpenAPI document does not marshal: " + err.Error())
	}
	return append(out, '\n')
}

// operation is one operation: the responses given, and the error
// envelope for every refusal.
func operation(responses ...map[string]any) map[string]any {
	all := map[string]any{"default": jsonResponse("a refusal", ref("Error"))}
	for _, r := range responses {
		for status, body := range r {
			all[status] = body
		}
	}
	return map[string]any{"responses": all}
}

// jsonResponse is one response of a JSON body of the schema.
func jsonResponse(description string, schema map[string]any) map[string]any {
	return map[string]any{
		"description": description,
		"content":     map[string]any{"application/json": map[string]any{"schema": schema}},
	}
}

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

func refResponse(status, kind string) map[string]any {
	return map[string]any{status: jsonResponse("one "+kind, ref(kind))}
}

func objectResponse(status string) map[string]any {
	return map[string]any{status: jsonResponse("a document", map[string]any{"type": "object"})}
}

func listResponse(kind string) map[string]any {
	return map[string]any{"200": jsonResponse("a page of "+kind+"s", map[string]any{
		"type":     "object",
		"required": []any{"items"},
		"properties": map[string]any{
			"items":       map[string]any{"type": "array", "items": ref(kind)},
			"next_cursor": map[string]any{"type": "string"},
		},
	})}
}

func rowsResponse() map[string]any {
	return map[string]any{"200": jsonResponse("usage rows", map[string]any{
		"type":     "object",
		"required": []any{"items"},
		"properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{
			"type":     "object",
			"required": []any{"dimensions", "requests", "cost", "currency"},
			"properties": map[string]any{
				"dimensions": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"requests":   map[string]any{"type": "integer"},
				"cost":       map[string]any{"type": "integer"},
				"currency":   map[string]any{"type": "string"},
			},
		}}},
	})}
}

func recordsResponse() map[string]any {
	return map[string]any{"200": jsonResponse("the request records", map[string]any{
		"type":     "object",
		"required": []any{"items", "source"},
		"properties": map[string]any{
			"items":  map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": []any{"id", "status"}}},
			"source": map[string]any{"type": "string", "enum": []any{"memory", "archive"}},
		},
	})}
}

// kindSchema is one object of any of the four kinds: the envelope the
// contract fixes, and a spec and a status this front does not narrow
// further, since manifest and its v1 types own their shape.
func kindSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"apiVersion", "kind", "metadata", "spec", "status"},
		"properties": map[string]any{
			"apiVersion": map[string]any{"type": "string", "const": "lux.latere.ai/v1beta1"},
			"kind":       map[string]any{"type": "string", "enum": []any{"Provider", "Model", "Key", "Budget"}},
			"metadata": map[string]any{"type": "object", "required": []any{"name"}, "properties": map[string]any{
				"name":        map[string]any{"type": "string"},
				"labels":      map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"annotations": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			}},
			"spec":   map[string]any{"type": "object"},
			"status": map[string]any{"type": "object"},
		},
	}
}

// errorSchema is the one envelope every refusal carries.
func errorSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"error"},
		"properties": map[string]any{"error": map[string]any{
			"type":     "object",
			"required": []any{"code", "message"},
			"properties": map[string]any{
				"code":    map[string]any{"type": "string"},
				"message": map[string]any{"type": "string"},
				"details": map[string]any{"type": "object", "properties": map[string]any{
					"request_id": map[string]any{"type": "string"},
					"detail":     map[string]any{"type": "string"},
					"paths":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				}},
			},
		}},
	}
}
