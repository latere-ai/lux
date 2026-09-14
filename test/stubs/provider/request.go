// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	v1 "latere.ai/x/lux/manifest/v1"
)

// routeKind is one row of the stub's route table.
type routeKind int

const (
	routeChat routeKind = iota
	routeResponses
	routeEmbeddings
	routeMessages
	routeAnthropicCount
	routeGemini // the model routes, before the verb is read
	routeGeminiGenerate
	routeGeminiStream
	routeGeminiCount
	routeGeminiEmbed
	routeGenerate
	routeLuxCount
	routeModels
)

// geminiRoute reads the verb after the colon of a gemini model route.
func geminiRoute(verb string) (routeKind, bool) {
	switch verb {
	case "generateContent":
		return routeGeminiGenerate, true
	case "streamGenerateContent":
		return routeGeminiStream, true
	case "countTokens":
		return routeGeminiCount, true
	case "embedContent":
		return routeGeminiEmbed, true
	}
	return routeGemini, false
}

// streams reports whether the route has a streamed shape.
func streams(rt routeKind) bool {
	switch rt {
	case routeChat, routeResponses, routeMessages, routeGeminiStream, routeGenerate:
		return true
	case routeEmbeddings, routeAnthropicCount, routeGemini, routeGeminiGenerate, routeGeminiCount, routeGeminiEmbed, routeLuxCount, routeModels:
		return false
	}
	return false
}

// parsed is what the stub reads off one request: the upstream model
// name, the last user text, whether a stream was asked for and in which
// framing, and a gemini route's verb.
type parsed struct {
	model  string
	text   string
	stream bool
	sse    bool // a gemini stream asked for with alt=sse
	verb   string
}

// parseRequest reads the body and the path of one request. A body that
// is not JSON reads as no model and no text, which is the deterministic
// answer for an empty request.
func parseRequest(d v1.Dialect, rt routeKind, r *http.Request, body []byte) parsed {
	var p parsed
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	if rt == routeGemini {
		tail := r.PathValue("model")
		if i := strings.LastIndexByte(tail, ':'); i >= 0 {
			p.model, p.verb = tail[:i], tail[i+1:]
		} else {
			p.model = tail
		}
		p.stream = p.verb == "streamGenerateContent"
		p.sse = r.URL.Query().Get("alt") == "sse"
	} else {
		p.model, _ = doc["model"].(string)
		p.stream, _ = doc["stream"].(bool)
	}
	p.text = lastUserText(d, rt, doc)
	return p
}

// lastUserText is the text the digest is taken over: the last user
// message's text on every dialect, the input of an embedding, and "" when
// the body carries none.
func lastUserText(d v1.Dialect, rt routeKind, doc map[string]any) string {
	switch {
	case rt == routeEmbeddings:
		return lastString(doc["input"])
	case rt == routeResponses:
		if s, ok := doc["input"].(string); ok {
			return s
		}
		return lastUserOf(doc["input"], "content")
	case d == v1.DialectGemini:
		return lastUserOf(doc["contents"], "parts")
	case d == v1.DialectLux:
		return lastUserOf(doc["messages"], "blocks")
	}
	return lastUserOf(doc["messages"], "content")
}

// lastUserOf walks a message list from the end for the last entry whose
// role is user, or names no role, and returns the text of its member.
func lastUserOf(list any, member string) string {
	items, _ := list.([]any)
	for i := len(items) - 1; i >= 0; i-- {
		m, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "" && role != "user" {
			continue
		}
		return textOf(m[member])
	}
	return ""
}

// textOf reads a string, or joins the text members of a list of parts.
func textOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	parts, _ := v.([]any)
	var texts []string
	for _, p := range parts {
		if m, ok := p.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				texts = append(texts, s)
			}
		}
	}
	return strings.Join(texts, "")
}

// lastString reads a string, or the last string of a list.
func lastString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	items, _ := v.([]any)
	for i := len(items) - 1; i >= 0; i-- {
		if s, ok := items[i].(string); ok {
			return s
		}
	}
	return ""
}

// Content is the deterministic assistant content for a dialect, a model,
// and the last user text: stub:<dialect>:<model>:<first 8 hex of the
// SHA-256 of the text>.
func Content(d v1.Dialect, model, text string) string {
	return "stub:" + string(d) + ":" + model + ":" + Digest(text)
}

// Digest is the first 8 hex characters of the SHA-256 of s.
func Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
