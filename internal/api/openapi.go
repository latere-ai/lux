// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/auth"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The OpenAPI document of spec 011, generated from the kinds' Go types,
// the route table, and the error table, so a generated client and the
// server agree by construction. tools/apidoc writes it as
// api/openapi.yaml; the handler serves the same document as JSON at
// GET /v1/openapi.json; TestOpenAPIIsCurrent holds the committed file to
// a fresh generation.

// ordered is a JSON object whose members keep the order they were
// written in, so the document reads in the Go struct order and two
// generations are byte-identical.
type ordered []member

// member is one key and value of an ordered object.
type member struct {
	key   string
	value any
}

// obj builds an ordered object from alternating keys and values; a key
// that is not a string is a bug in the generator and a panic.
func obj(kv ...any) ordered {
	out := make(ordered, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			panic("api: an OpenAPI member key is not a string")
		}
		out = append(out, member{key, kv[i+1]})
	}
	return out
}

// set appends or replaces one member.
func (o ordered) set(key string, value any) ordered {
	for i, m := range o {
		if m.key == key {
			o[i].value = value
			return o
		}
	}
	return append(o, member{key, value})
}

// MarshalJSON writes the members in order.
func (o ordered) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(m.value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// toYAML converts the document into the shapes the YAML encoder keeps
// in order: a MapSlice per object, a slice per list, scalars as they are.
func toYAML(v any) any {
	switch x := v.(type) {
	case ordered:
		out := make(yaml.MapSlice, 0, len(x))
		for _, m := range x {
			out = append(out, yaml.MapItem{Key: m.key, Value: toYAML(m.value)})
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, toYAML(e))
		}
		return out
	case []string:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, e)
		}
		return out
	}
	return v
}

// OpenAPIYAML renders the document as the YAML committed at
// api/openapi.yaml. A document that does not encode is a bug in the
// generator and a panic, as it is for the JSON.
func OpenAPIYAML() []byte {
	data, err := yaml.MarshalWithOptions(toYAML(openAPIDocument()), yaml.Indent(2), yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		panic("api: the OpenAPI document does not encode as YAML: " + err.Error())
	}
	return data
}

// openAPIJSON renders the document as the JSON GET /v1/openapi.json
// serves, with a trailing newline.
func openAPIJSON() []byte {
	data, err := json.Marshal(openAPIDocument())
	if err != nil {
		panic("api: the OpenAPI document does not encode: " + err.Error())
	}
	return append(data, '\n')
}

// openAPI is GET /v1/openapi.json; no bearer, because a document that
// describes the API carries no installation's data.
func (c *call) openAPI(context.Context) *Error {
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(c.h.openapi)))
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(c.h.openapi)
	return nil
}

// openAPIDocument is the document.
func openAPIDocument() ordered {
	s := newSchemas()
	for _, k := range kinds {
		s.kind(k.name)
	}
	s.add("Error", errorSchema())
	s.add("Self", s.of(reflect.TypeFor[Self]()))
	s.add("SelfLimits", s.of(reflect.TypeFor[SelfLimits]()))
	s.add("Filter", s.of(reflect.TypeFor[authz.Filter]()))
	s.add("WellKnown", s.of(reflect.TypeFor[WellKnown]()))
	s.add("UsageList", obj("type", "object", "required", []string{"items"}, "properties", obj(
		"items", obj("type", "array", "items", s.of(reflect.TypeFor[metering.Row]())),
	)))
	s.add("RecordList", obj("type", "object", "required", []string{"items", "source"}, "properties", obj(
		"items", obj("type", "array", "items", s.of(reflect.TypeFor[metering.Record]())),
		"next_cursor", obj("type", "string", "description", "The cursor of the next page; absent on the last."),
		"source", obj("type", "string", "enum", []string{sourceArchive, sourceMemory}, "description", "Which record set answered: the replica's own ring, or the installation-wide archive when a request log exporter is configured."),
	)))
	paths := ordered{}
	for _, k := range kinds {
		paths = append(paths, kindPaths(k)...)
	}
	paths = append(paths,
		member{"/v1/usage", obj("get", operation("readUsage", "Usage aggregated over a range, grouped by at most three dimensions and bucketed by an interval; unpaged, and no row sums two currencies. A filter outside the authorizer's own is an empty items.", auth.ActionUsageRead,
			[]any{ref("parameters", "from"), ref("parameters", "to"), ref("parameters", "by"), ref("parameters", "interval"), ref("parameters", "usageKey"), ref("parameters", "usageModel"), ref("parameters", "usageProvider"), ref("parameters", "usageOwner"), ref("parameters", "usageLabel")},
			nil, response("200", "The rows.", "UsageList")))},
		member{"/v1/requests", obj("get", operation("listRequests", "Usage records over a range, newest first, paged by limit and cursor, with the record set that answered beside them.", auth.ActionUsageRead,
			[]any{ref("parameters", "from"), ref("parameters", "to"), ref("parameters", "usageKey"), ref("parameters", "usageModel"), ref("parameters", "usageProvider"), ref("parameters", "usageOwner"), ref("parameters", "usageLabel"), ref("parameters", "status"), ref("parameters", "error"), ref("parameters", "stream"), ref("parameters", "recordLimit"), ref("parameters", "cursor")},
			nil, response("200", "One page of records.", "RecordList")))},
		member{"/v1/self", obj("get", operation("readSelf", "The caller's identity, who decides permission, and what this replica remembers granting the subject.", "none", nil, nil, response("200", "The caller.", "Self")))},
		member{"/v1/openapi.json", obj("get", operation("readOpenAPI", "This document as JSON; no bearer.", "none", []any{}, nil, obj("200", obj("description", "The document.", "content", obj("application/json", obj("schema", obj("type", "object"))))))).set("security", []any{})},
		member{"/.well-known/lux", obj("get", operation("readWellKnown", "The server's identity: the build, the API and the doors under LUX_PUBLIC_URL, the issuers, the audience, and the mode; no bearer.", "none", []any{}, nil, response("200", "The server.", "WellKnown")).set("security", []any{}))},
	)
	errs := make([]any, 0, len(codes))
	for _, c := range codes {
		errs = append(errs, obj("code", string(c), "status", c.Status(), "message", c.Message()))
	}
	return obj(
		"openapi", "3.1.0",
		"info", obj(
			"title", "Lux API",
			"version", "v1",
			"description", "The control plane of Lux under /v1: apply a Provider, Model, Key, or Budget by name with PUT, read one by id or name, list with limit and cursor, delete, and rotate a Key. Every request body is a manifest of "+v1.APIVersion+" in JSON or YAML; every response is JSON; every refusal is one envelope with one code from the error table in x-lux-errors. Every URL is under LUX_PUBLIC_URL.",
		),
		"paths", paths,
		"components", obj(
			"securitySchemes", obj("bearer", obj("type", "http", "scheme", "bearer", "bearerFormat", "JWT", "description", "A token from an issuer in LUX_OIDC_ISSUERS with aud containing LUX_OIDC_AUDIENCE.")),
			"parameters", parameters(),
			"headers", headers(),
			"schemas", s.ordered(),
		),
		"security", []any{obj("bearer", []any{})},
		"x-lux-errors", errs,
	)
}

// kindPaths are the four routes of one kind, and the rotate of a Key.
func kindPaths(k kind) ordered {
	kindRef := "#/components/schemas/" + k.name
	itemParams := []any{pathName(k)}
	listParams := []any{ref("parameters", "label"), ref("parameters", "owner"), ref("parameters", "limit"), ref("parameters", "cursor")}
	if k.name == v1.KindModel {
		listParams = append(listParams, ref("parameters", "source"), ref("parameters", "provider"))
	}
	body := obj("required", true, "description", "A "+k.name+" manifest, or one without apiVersion, kind, and metadata.name, which the route supplies.", "content", obj(
		"application/json", obj("schema", obj("$ref", kindRef)),
		"application/yaml", obj("schema", obj("$ref", kindRef)),
	))
	item := "/v1/" + k.plural + "/{name}"
	list := obj("get", operation("list"+k.name+"s", "List "+k.name+"s under the authorizer's filter, paged by limit and cursor.", k.list, listParams, nil,
		response("200", "One page.", k.name+"List")))
	itemOps := obj(
		"put", operation("apply"+k.name, "Create the "+k.name+" when no object of the name exists, update it when one does; 201 on create, 200 on update. Apply is by name and only by name.", k.create+" or "+k.update,
			append(itemParams, ref("parameters", "ifMatch"), ref("parameters", "ifNoneMatch")), body,
			objectResponse("200", "The object as updated.", k.name), objectResponse("201", "The object as created; a Key carries status.value once.", k.name)),
		"get", operation("read"+k.name, "Read one "+k.name+" by id or name, with its status.", k.read, itemParams, nil, objectResponse("200", "The object.", k.name)),
		"delete", operation("delete"+k.name, "Delete one "+k.name+" by id or name; 204 with no body.", k.del, append(itemParams, ref("parameters", "ifMatch")), nil, obj("204", obj("description", "Deleted."))),
	)
	out := ordered{{"/v1/" + k.plural, list}, {item, itemOps}}
	if k.name == v1.KindKey {
		out = append(out, member{item + "/rotate", obj("post", operation("rotateKey", "Mint a new value for the Key, keeping its id, name, spec, owner, and windows; 200 with status.value. A repeat is a second rotation.", k.update, itemParams, nil, objectResponse("200", "The Key with its new value.", k.name)))})
	}
	return out
}

// pathName is the {name} parameter of a kind's item route.
func pathName(k kind) ordered {
	desc := "The " + k.name + "'s name on an apply; its name or its " + k.prefix + " id otherwise."
	schema := obj("type", "string")
	if k.name == v1.KindModel {
		desc += " A Model name may carry /, so the route matches any number of segments, percent-encoded or not."
	}
	return obj("name", "name", "in", "path", "required", true, "description", desc, "schema", schema)
}

// operation is one operation's object.
func operation(id, summary, action string, params []any, body ordered, responses ...ordered) ordered {
	res := ordered{}
	for _, r := range responses {
		res = append(res, r...)
	}
	res = append(res, member{"default", obj("description", "A refusal: one code of x-lux-errors with its fixed sentence.", "headers", obj("Lux-Request-Id", ref("headers", "Lux-Request-Id")), "content", obj("application/json", obj("schema", obj("$ref", "#/components/schemas/Error"))))})
	op := obj("operationId", id, "summary", summary, "x-lux-action", action)
	if params != nil {
		op = op.set("parameters", params)
	}
	if body != nil {
		op = op.set("requestBody", body)
	}
	return op.set("responses", res)
}

// objectResponse is a response carrying one object with its ETag.
func objectResponse(status, description, schema string) ordered {
	return obj(status, obj("description", description, "headers", obj("ETag", ref("headers", "ETag"), "Lux-Request-Id", ref("headers", "Lux-Request-Id")), "content", obj("application/json", obj("schema", obj("$ref", "#/components/schemas/"+schema)))))
}

// response is a response carrying one schema.
func response(status, description, schema string) ordered {
	return obj(status, obj("description", description, "headers", obj("Lux-Request-Id", ref("headers", "Lux-Request-Id")), "content", obj("application/json", obj("schema", obj("$ref", "#/components/schemas/"+schema)))))
}

func ref(section, name string) ordered {
	return obj("$ref", "#/components/"+section+"/"+name)
}

// parameters are the query and header parameters of the route table.
func parameters() ordered {
	query := func(name, desc string, schema ordered) ordered {
		return obj("name", name, "in", "query", "required", false, "description", desc, "schema", schema)
	}
	return obj(
		"label", query("label", "k=v, repeatable; every pair must match.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"owner", query("owner", "A rendered subject; intersected with the authorizer's filter, so one outside it is an empty list.", obj("type", "string")),
		"limit", query("limit", "Page size, default 50, at most 200; above is invalid_field.", obj("type", "integer", "minimum", 1, "maximum", maxLimit, "default", defaultLimit)),
		"cursor", query("cursor", "The previous page's next_cursor, opaque; one from another kind or filter is invalid_field.", obj("type", "string")),
		"source", query("source", "Models by source.", obj("type", "string", "enum", []string{string(v1.SourceDeclared), string(v1.SourceDiscovered)})),
		"provider", query("provider", "Models with a target on the Provider, by name or prv_ id.", obj("type", "string")),
		"from", query("from", "The start of the range, RFC 3339; default 24 hours before to. The range is at most 90 days.", obj("type", "string", "format", "date-time")),
		"to", query("to", "The end of the range, RFC 3339; default now. It must be after from.", obj("type", "string", "format", "date-time")),
		"by", query("by", "The dimensions to group by, comma separated, at most three: key, model, provider, owner, door, status, or label:<name>. None is one total row.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"interval", query("interval", "The bucket width; none is one row per group over the whole range.", obj("type", "string", "enum", []string{string(metering.IntervalNone), string(metering.IntervalHour), string(metering.IntervalDay), string(metering.IntervalMonth)}, "default", string(metering.IntervalNone))),
		"usageKey", query("key", "A Key by name or key_ id, repeatable; a filter, not a grouping. A name that names no Key is an empty items, and a key_ id keeps working after the Key is deleted.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"usageModel", query("model", "A Model by name or mdl_ id, repeatable.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"usageProvider", query("provider", "A Provider by name or prv_ id, repeatable.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"usageOwner", query("owner", "A rendered subject, repeatable; intersected with the authorizer's filter, so one outside it is an empty items.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"usageLabel", query("label", "name=value over the Key's labels, repeatable; every pair must match.", obj("type", "array", "items", obj("type", "string"))).set("style", "form").set("explode", true),
		"status", query("status", "Records of one outcome.", obj("type", "string", "enum", []string{string(metering.StatusOK), string(metering.StatusRefused), string(metering.StatusFailed)})),
		"error", query("error", "Records carrying one error code of x-lux-errors.", obj("type", "string")),
		"stream", query("stream", "Streamed records alone, or unstreamed alone.", obj("type", "boolean")),
		"recordLimit", query("limit", "Page size, default 50, at most 1000; above is invalid_field.", obj("type", "integer", "minimum", 1, "maximum", maxRecordLimit, "default", defaultLimit)),
		"ifMatch", obj("name", "If-Match", "in", "header", "required", false, "description", "* for the object must exist, or one quoted integer for exactly this version; a mismatch is conflict, a free name not_found. Anything else is invalid_field.", "schema", obj("type", "string")),
		"ifNoneMatch", obj("name", "If-None-Match", "in", "header", "required", false, "description", "* for create only; a taken name is already_exists. Anything else is invalid_field.", "schema", obj("type", "string")),
	)
}

// headers are the response headers every route writes.
func headers() ordered {
	return obj(
		"Lux-Request-Id", obj("description", "The server's id of this request, req_ and a ULID; a caller's own is replaced.", "schema", obj("type", "string")),
		"ETag", obj("description", "The object's status.version as one quoted integer, the value If-Match takes.", "schema", obj("type", "string")),
		"RateLimit-Limit", obj("description", "The subject's requests a minute on this replica.", "schema", obj("type", "integer")),
		"RateLimit-Remaining", obj("description", "Requests left in the window.", "schema", obj("type", "integer")),
		"RateLimit-Reset", obj("description", "Seconds until the window is full again.", "schema", obj("type", "integer")),
	)
}

// errorSchema is the envelope of spec 011.
func errorSchema() ordered {
	names := make([]string, 0, len(codes))
	for _, c := range codes {
		names = append(names, string(c))
	}
	return obj("type", "object", "required", []string{"error"}, "properties", obj("error", obj(
		"type", "object", "required", []string{"code", "message", "details"},
		"properties", obj(
			"code", obj("type", "string", "enum", names),
			"message", obj("type", "string", "description", "The one fixed user sentence of the code."),
			"details", obj("type", "object", "required", []string{"request_id"}, "properties", obj(
				"request_id", obj("type", "string"),
				"paths", obj("type", "array", "items", obj("type", "string"), "description", "The JSON paths of the fields the code names."),
				"detail", obj("type", "string", "description", "One developer sentence."),
			)),
		),
	)))
}

// schemas collects the component schemas as they are reached, in the
// order they are reached, each named by its Go type.
type schemas struct {
	names  []string
	byName map[string]ordered
}

func newSchemas() *schemas { return &schemas{byName: map[string]ordered{}} }

func (s *schemas) add(name string, schema ordered) {
	if _, ok := s.byName[name]; !ok {
		s.names = append(s.names, name)
	}
	s.byName[name] = schema
}

func (s *schemas) ordered() ordered {
	out := make(ordered, 0, len(s.names))
	for _, n := range s.names {
		out = append(out, member{n, s.byName[n]})
	}
	return out
}

// kind adds one kind's schema and its list: the Go type's members with
// apiVersion and kind in front, which MarshalJSON writes and the struct
// does not carry, and status read-only.
func (s *schemas) kind(name string) {
	t := reflect.TypeOf(zeroObject(name)).Elem()
	s.add(name, obj())
	props := obj(
		"apiVersion", obj("type", "string", "const", v1.APIVersion),
		"kind", obj("type", "string", "const", name),
	)
	for _, p := range s.properties(t) {
		if status, ok := p.value.(ordered); ok && p.key == "status" {
			p.value = status.set("readOnly", true).set("description", "Written by the server and ignored on apply.")
		}
		props = append(props, p)
	}
	s.add(name, obj("type", "object", "required", []string{"apiVersion", "kind", "metadata", "spec"}, "properties", props))
	s.add(name+"List", obj("type", "object", "required", []string{"items"}, "properties", obj(
		"items", obj("type", "array", "items", obj("$ref", "#/components/schemas/"+name)),
		"next_cursor", obj("type", "string", "description", "The cursor of the next page; absent on the last."),
	)))
}

// enums are the string types whose values are a closed set.
var enums = map[reflect.Type][]string{
	reflect.TypeFor[v1.Dialect]():       {string(v1.DialectOpenAI), string(v1.DialectAnthropic), string(v1.DialectGemini), string(v1.DialectLux)},
	reflect.TypeFor[v1.Scheme]():        {string(v1.SchemeBearer), string(v1.SchemeRaw)},
	reflect.TypeFor[v1.DiscoveryMode](): {string(v1.DiscoveryAuto), string(v1.DiscoveryNone)},
	reflect.TypeFor[v1.HealthMode]():    {string(v1.HealthProbe), string(v1.HealthPassive), string(v1.HealthNone)},
	reflect.TypeFor[v1.HealthState]():   {string(v1.HealthUnknown), string(v1.HealthHealthy), string(v1.HealthDegraded), string(v1.HealthUnreachable)},
	reflect.TypeFor[v1.TunnelState]():   {string(v1.TunnelConnected), string(v1.TunnelDisconnected)},
	reflect.TypeFor[v1.Fallback]():      {string(v1.FallbackOnError), string(v1.FallbackNever)},
	reflect.TypeFor[v1.Modality]():      {string(v1.ModalityText), string(v1.ModalityImage), string(v1.ModalityAudio), string(v1.ModalityVideo), string(v1.ModalityFile), string(v1.ModalityEmbedding)},
	reflect.TypeFor[v1.Source]():        {string(v1.SourceDeclared), string(v1.SourceDiscovered)},
	reflect.TypeFor[v1.KeyState]():      {string(v1.KeyActive), string(v1.KeyDisabled), string(v1.KeyExpired), string(v1.KeyExhausted)},
	reflect.TypeFor[v1.BudgetState]():   {string(v1.BudgetOpen), string(v1.BudgetExhausted)},
	reflect.TypeFor[metering.Status]():  {string(metering.StatusOK), string(metering.StatusRefused), string(metering.StatusFailed)},
}

// fieldSchemas are the members whose shape is not their type's: a
// record's targetDialect is the dialect set plus the empty string, which
// a refused request carries because no target was chosen (spec 009).
var fieldSchemas = map[reflect.Type]map[string]ordered{
	reflect.TypeFor[metering.Record](): {
		"TargetDialect": obj("type", "string", "enum", append([]string{""}, enums[reflect.TypeFor[v1.Dialect]()]...), "description", "The dialect of the target that answered; empty when no target was chosen, as on a refused request."),
	},
}

// scalars are the named types with one JSON shape of their own.
var scalars = map[reflect.Type]ordered{
	reflect.TypeFor[v1.Money]():    obj("type", "string", "pattern", `^[0-9]{1,12}(\.[0-9]{1,6})?$`, "description", "A money amount as a decimal string of at most 12 integer and 6 fraction digits."),
	reflect.TypeFor[v1.Duration](): obj("type", "string", "description", "A Go duration, such as 10m or 168h."),
	reflect.TypeFor[v1.Window]():   obj("type", "string", "description", "A spend window: a Go duration from 1m to 8760h, month, or none."),
	reflect.TypeFor[time.Time]():   obj("type", "string", "format", "date-time"),
	reflect.TypeFor[auth.Policy](): obj("type", "string", "enum", []string{string(auth.PolicyAuthorizer), string(auth.PolicyOwner), string(auth.PolicyFile)}),
}

// of is the schema of a Go type: a $ref for a named struct, added to the
// components on the way, and the shape of anything else.
func (s *schemas) of(t reflect.Type) ordered {
	if sc, ok := scalars[t]; ok {
		return sc
	}
	if values, ok := enums[t]; ok {
		return obj("type", "string", "enum", values)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return s.of(t.Elem())
	case reflect.String:
		return obj("type", "string")
	case reflect.Bool:
		return obj("type", "boolean")
	case reflect.Int, reflect.Int64:
		return obj("type", "integer")
	case reflect.Slice:
		return obj("type", "array", "items", s.of(t.Elem()))
	case reflect.Map:
		if t.Elem().Kind() == reflect.Interface {
			return obj("type", "object", "additionalProperties", true)
		}
		return obj("type", "object", "additionalProperties", s.of(t.Elem()))
	case reflect.Struct:
		name := t.Name()
		if _, ok := s.byName[name]; !ok {
			s.add(name, obj())
			schema := obj("type", "object")
			if req := required(t); len(req) > 0 {
				schema = schema.set("required", req)
			}
			s.add(name, schema.set("properties", s.properties(t)))
		}
		return obj("$ref", "#/components/schemas/"+name)
	default:
		panic("api: no OpenAPI shape for " + t.String())
	}
}

// properties are the struct's JSON members in field order, the
// write-only members included as writeOnly strings.
func (s *schemas) properties(t reflect.Type) ordered {
	props := ordered{}
	for f := range t.Fields() {
		if wo := f.Tag.Get("writeonly"); wo != "" {
			props = append(props, member{wo, obj("type", "string", "writeOnly", true, "description", "Write-only: decoded into a member no encoding carries, so no response, event, or log line returns it.")})
			continue
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || name == "" {
			continue
		}
		if sc, ok := fieldSchemas[t][f.Name]; ok {
			props = append(props, member{name, sc})
			continue
		}
		props = append(props, member{name, s.of(f.Type)})
	}
	return props
}

// required lists the members every encoding of the struct carries: the
// exported fields with no omitempty and no omitzero.
func required(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero") {
			continue
		}
		out = append(out, name)
	}
	return out
}
