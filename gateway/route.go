// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"strings"

	v1 "latere.ai/x/lux/manifest/v1"
)

// RouteClass is what the gateway does with a path under a door.
type RouteClass string

// The classes of spec 004, and Served for the routes the gateway answers
// itself and never forwards: the model list and read on every door, and
// a token count no upstream answers.
const (
	ClassTranslated RouteClass = "translated"
	ClassModel      RouteClass = "model"
	ClassOpaque     RouteClass = "opaque"
	ClassServed     RouteClass = "served"
)

// operation is one row of the door table, the thing a route does.
type operation int

const (
	opNone operation = iota
	opChatCompletions
	opResponses
	opEmbeddings
	opMessages
	opAnthropicCount
	opGeminiGenerate
	opGeminiStream
	opGeminiCount
	opGeminiEmbed
	opGenerate
	opLuxCount
	opModelsList
	opModelsRead
	opOpaque
)

// route is a matched row: the door, the class, the operation, the route
// template the record carries, the model name when the table reads it
// from the path, and the caller's path relative to the door.
type route struct {
	door     v1.Dialect
	class    RouteClass
	op       operation
	template string
	model    string // from the path, on the gemini model routes and the model read
	rest     string // the path under the door, with the door prefix removed
}

// doors are the four prefixes, each a dialect.
var doors = []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux}

// versionPrefix is the dialect's own version prefix, removed from a
// passthrough path before it is joined to the base URL, so
// /openai/v1/chat/completions against https://api.example.com/v1
// reaches /v1/chat/completions once and not twice.
func versionPrefix(d v1.Dialect) string {
	if d == v1.DialectGemini {
		return "/v1beta"
	}
	return "/v1"
}

// door reads the door prefix off a path: the dialect and the rest, or
// "" when the path is under no door.
func door(path string) (v1.Dialect, string) {
	for _, d := range doors {
		prefix := "/" + string(d)
		if path == prefix {
			return d, "/"
		}
		if strings.HasPrefix(path, prefix+"/") {
			return d, path[len(prefix):]
		}
	}
	return "", ""
}

// match reads the door table for one method and path: the route, or a
// not_found failure carrying the door for the envelope. A path under a
// door that is not under the dialect's version prefix, a method the table
// does not list for a translated or model route, and anything outside
// the four doors is not_found.
func match(method, path string) (route, *failure) {
	d, rest := door(path)
	if d == "" {
		return route{}, fail(CodeNotFound, "the path is under none of the doors /openai, /anthropic, /gemini, /lux")
	}
	rt := route{door: d, rest: rest}
	switch d {
	case v1.DialectOpenAI:
		return matchOpenAI(rt, method, rest)
	case v1.DialectAnthropic:
		return matchAnthropic(rt, method, rest)
	case v1.DialectGemini:
		return matchGemini(rt, method, rest)
	case v1.DialectLux:
		return matchLux(rt, method, rest)
	}
	return route{}, notInTable(d, method, rest)
}

// RouteTemplate is the door table's route template for a request with
// method and path, the path rooted at the doors as the handler reads it
// (/openai/v1/chat/completions), or "" when the table has no row for
// them. The template holds a model's place with {model} and an opaque
// route's rest with *, so it is bounded by the table and never carries
// the caller's own path: the value a listener in front of the doors
// labels its request metrics and spans with. It is the route the
// handler records as Record.Route for the same request.
func RouteTemplate(method, path string) string {
	rt, f := match(method, path)
	if f != nil {
		return ""
	}
	return rt.template
}

func notInTable(d v1.Dialect, method, rest string) *failure {
	return fail(CodeNotFound, method+" "+rest+" is not in the /"+string(d)+" door's route table")
}

// modelsRoute matches GET <prefix>/models and GET <prefix>/models/{model}
// on any door; the name may carry slashes.
func modelsRoute(rt route, method, rest, prefix string) (route, bool) {
	if !strings.HasPrefix(rest, prefix+"/models") {
		return rt, false
	}
	tail := rest[len(prefix+"/models"):]
	switch {
	case tail == "":
		rt.op, rt.class, rt.template = opModelsList, ClassServed, "/"+string(rt.door)+prefix+"/models"
	case strings.HasPrefix(tail, "/") && len(tail) > 1:
		rt.op, rt.class, rt.template = opModelsRead, ClassServed, "/"+string(rt.door)+prefix+"/models/{model}"
		rt.model = tail[1:]
	default:
		return rt, false
	}
	if method != http.MethodGet {
		return rt, false
	}
	return rt, true
}

func matchOpenAI(rt route, method, rest string) (route, *failure) {
	if r, ok := modelsRoute(rt, method, rest, "/v1"); ok {
		return r, nil
	}
	switch rest {
	case "/v1/chat/completions":
		rt.op, rt.class = opChatCompletions, ClassTranslated
	case "/v1/responses":
		rt.op, rt.class = opResponses, ClassTranslated
	case "/v1/embeddings":
		rt.op, rt.class = opEmbeddings, ClassModel
	case "/v1/models":
		return route{}, notInTable(rt.door, method, rest)
	default:
		if !strings.HasPrefix(rest, "/v1/") || strings.HasPrefix(rest, "/v1/models/") {
			return route{}, notInTable(rt.door, method, rest)
		}
		rt.op, rt.class, rt.template = opOpaque, ClassOpaque, "/openai/v1/*"
		return rt, nil
	}
	if method != http.MethodPost {
		return route{}, notInTable(rt.door, method, rest)
	}
	rt.template = "/openai" + rest
	return rt, nil
}

func matchAnthropic(rt route, method, rest string) (route, *failure) {
	if r, ok := modelsRoute(rt, method, rest, "/v1"); ok {
		return r, nil
	}
	switch rest {
	case "/v1/messages":
		rt.op, rt.class = opMessages, ClassTranslated
	case "/v1/messages/count_tokens":
		rt.op, rt.class = opAnthropicCount, ClassModel
	case "/v1/models":
		return route{}, notInTable(rt.door, method, rest)
	default:
		if !strings.HasPrefix(rest, "/v1/") || strings.HasPrefix(rest, "/v1/models/") {
			return route{}, notInTable(rt.door, method, rest)
		}
		rt.op, rt.class, rt.template = opOpaque, ClassOpaque, "/anthropic/v1/*"
		return rt, nil
	}
	if method != http.MethodPost {
		return route{}, notInTable(rt.door, method, rest)
	}
	rt.template = "/anthropic" + rest
	return rt, nil
}

// geminiOps are the four model routes of the gemini door, by the verb
// after the colon.
var geminiOps = map[string]operation{
	"generateContent":       opGeminiGenerate,
	"streamGenerateContent": opGeminiStream,
	"countTokens":           opGeminiCount,
	"embedContent":          opGeminiEmbed,
}

func matchGemini(rt route, method, rest string) (route, *failure) {
	if strings.HasPrefix(rest, "/v1beta/models/") {
		tail := rest[len("/v1beta/models/"):]
		if i := strings.LastIndexByte(tail, ':'); i > 0 {
			if op, ok := geminiOps[tail[i+1:]]; ok {
				if method != http.MethodPost {
					return route{}, notInTable(rt.door, method, rest)
				}
				rt.op, rt.class, rt.model = op, ClassModel, tail[:i]
				rt.template = "/gemini/v1beta/models/{model}:" + tail[i+1:]
				return rt, nil
			}
		}
	}
	if r, ok := modelsRoute(rt, method, rest, "/v1beta"); ok {
		return r, nil
	}
	switch {
	case rest == "/v1beta/models":
		return route{}, notInTable(rt.door, method, rest)
	case strings.HasPrefix(rest, "/v1beta/models/") && !strings.Contains(rest, ":"):
		return route{}, notInTable(rt.door, method, rest)
	case strings.HasPrefix(rest, "/v1beta/"), strings.HasPrefix(rest, "/v1/"):
		rt.op, rt.class, rt.template = opOpaque, ClassOpaque, "/gemini/*"
		return rt, nil
	}
	return route{}, notInTable(rt.door, method, rest)
}

func matchLux(rt route, method, rest string) (route, *failure) {
	if r, ok := modelsRoute(rt, method, rest, "/v1"); ok {
		return r, nil
	}
	switch rest {
	case "/v1/generate":
		rt.op, rt.class = opGenerate, ClassTranslated
	case "/v1/count_tokens":
		rt.op, rt.class = opLuxCount, ClassModel
	default:
		return route{}, notInTable(rt.door, method, rest)
	}
	if method != http.MethodPost {
		return route{}, notInTable(rt.door, method, rest)
	}
	rt.template = "/lux" + rest
	return rt, nil
}

// modelFromBody reports whether the route reads its model name from the
// body's model member rather than from the path.
func (r route) modelFromBody() bool {
	switch r.op {
	case opChatCompletions, opResponses, opEmbeddings, opMessages, opAnthropicCount, opGenerate, opLuxCount:
		return true
	case opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed, opModelsList, opModelsRead, opOpaque, opNone:
		return false
	}
	return false
}

// count reports whether the route is a token count, which reserves zero
// tokens and may be answered from the estimate.
func (r route) count() bool {
	switch r.op {
	case opAnthropicCount, opLuxCount, opGeminiCount:
		return true
	case opChatCompletions, opResponses, opEmbeddings, opMessages, opGeminiGenerate, opGeminiStream, opGeminiEmbed, opGenerate, opModelsList, opModelsRead, opOpaque, opNone:
		return false
	}
	return false
}

// upstreamPath is the target dialect's path for the operation on a
// translation, relative to the base URL's path. The openai target serves
// two translated routes; responses is chosen for the reasoning family
// by the caller of this function.
func upstreamPath(target v1.Dialect, op operation, responses bool) string {
	switch target {
	case v1.DialectOpenAI:
		if responses {
			return "/responses"
		}
		return "/chat/completions"
	case v1.DialectAnthropic:
		if op == opAnthropicCount || op == opLuxCount {
			return "/messages/count_tokens"
		}
		return "/messages"
	case v1.DialectLux:
		return "/generate"
	case v1.DialectGemini:
		return ""
	}
	return ""
}

// passthroughPath is the caller's path relative to the door with the
// dialect's version prefix removed, so it joins to the base URL's path
// without repeating the version. The gemini door strips /v1beta and, on
// an opaque route, /v1 as well, because Google's base URL carries the
// version and a path under either prefix is the same API.
func passthroughPath(d v1.Dialect, rest string) string {
	prefix := versionPrefix(d)
	if strings.HasPrefix(rest, prefix+"/") {
		return rest[len(prefix):]
	}
	if d == v1.DialectGemini && strings.HasPrefix(rest, "/v1/") {
		return rest[len("/v1"):]
	}
	return rest
}
