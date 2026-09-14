// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestCodesHaveOneSentenceEach is the registers rule over the table:
// every code has a status and one fixed user sentence, no two codes share
// a sentence, and a string that is not a code has status 500 and no
// sentence.
func TestCodesHaveOneSentenceEach(t *testing.T) {
	seen := map[string]Code{}
	for _, c := range Codes() {
		if c.Status() < 400 || c.Status() > 504 {
			t.Errorf("%s: status %d is not an error status", c, c.Status())
		}
		msg := c.Message()
		if msg == "" || !strings.HasSuffix(msg, ".") || strings.Count(msg, ". ") > 0 {
			t.Errorf("%s: message %q is not one sentence", c, msg)
		}
		if other, dup := seen[msg]; dup {
			t.Errorf("%s and %s share the sentence %q", c, other, msg)
		}
		seen[msg] = c
	}
	if Code("bogus").Status() != http.StatusInternalServerError || Code("bogus").Message() != "" {
		t.Error("a string that is not a code has a status or a sentence")
	}
	if ClientClosed.Message() != "" {
		t.Error("client_closed has a user sentence, but it is never an HTTP answer")
	}
}

func TestDetailHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain ascii", "plain ascii"},
		{"line\nbreak", "line%0Abreak"},
		{"tab\there", "tab%09here"},
		{"unicode é", "unicode %C3%A9"},
		{"del\x7f", "del%7F"},
	}
	for _, c := range cases {
		if got := detailHeader(c.in); got != c.want {
			t.Errorf("detailHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := detailHeader(strings.Repeat("x", 2000))
	if len(long) != maxDetailBytes {
		t.Errorf("a long detail is %d bytes, want %d", len(long), maxDetailBytes)
	}
	// The cut lands on an escape boundary: 1023 plain bytes then one
	// escaped byte cannot fit, so the escaped byte is dropped whole.
	cut := detailHeader(strings.Repeat("x", 1023) + "\n" + "tail")
	if len(cut) != 1023 || strings.Contains(cut, "%") {
		t.Errorf("the cut split an escape: %d bytes, %q", len(cut), cut[1015:])
	}
}

// TestEnvelopeShapes renders one failure in each door's shape and checks
// the members the spec names: the code where the shape has a code member,
// the fixed sentence in message, and the detail only on the lux door.
func TestEnvelopeShapes(t *testing.T) {
	f := fail(CodeModelNotFound, "no Model named \"x\"")
	id := "req_01TEST"

	var oa openaiError
	if err := json.Unmarshal(envelope(v1.DialectOpenAI, id, f), &oa); err != nil {
		t.Fatal(err)
	}
	if oa.Error.Code != "model_not_found" || oa.Error.Type != "model_not_found" || oa.Error.Message != CodeModelNotFound.Message() || oa.Error.Param != nil {
		t.Errorf("openai envelope: %+v", oa)
	}

	var an anthropicError
	if err := json.Unmarshal(envelope(v1.DialectAnthropic, id, f), &an); err != nil {
		t.Fatal(err)
	}
	if an.Type != "error" || an.Error.Type != "model_not_found" || an.Error.Message != CodeModelNotFound.Message() || an.RequestID != id {
		t.Errorf("anthropic envelope: %+v", an)
	}

	var ge geminiError
	if err := json.Unmarshal(envelope(v1.DialectGemini, id, f), &ge); err != nil {
		t.Fatal(err)
	}
	if ge.Error.Code != 404 || ge.Error.Status != "NOT_FOUND" || len(ge.Error.Details) != 1 || ge.Error.Details[0].Reason != "model_not_found" || ge.Error.Details[0].Domain != "lux" || ge.Error.Details[0].Type != "type.googleapis.com/google.rpc.ErrorInfo" {
		t.Errorf("gemini envelope: %+v", ge)
	}

	var lx struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(envelope(v1.DialectLux, id, f), &lx); err != nil {
		t.Fatal(err)
	}
	if lx.Error.Code != "model_not_found" || lx.Error.Message != CodeModelNotFound.Message() || lx.Error.Details["request_id"] != id || lx.Error.Details["detail"] != f.detail {
		t.Errorf("lux envelope: %+v", lx)
	}
	for _, d := range []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini} {
		if strings.Contains(string(envelope(d, id, f)), f.detail) {
			t.Errorf("%s envelope carries the developer detail in its body", d)
		}
	}
	// A path under no door renders the lux shape, and a lux failure with
	// no detail has no detail member.
	if string(envelope("", id, fail(CodeNotFound, ""))) != string(envelope(v1.DialectLux, id, fail(CodeNotFound, ""))) {
		t.Error("no door does not render the lux shape")
	}
	if strings.Contains(string(envelope(v1.DialectLux, id, fail(CodeNotFound, ""))), `"detail"`) {
		t.Error("an empty detail is written")
	}
}

func TestGoogleStatusNames(t *testing.T) {
	cases := map[int]string{
		400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND",
		413: "INVALID_ARGUMENT", 429: "RESOURCE_EXHAUSTED", 502: "UNAVAILABLE", 503: "UNAVAILABLE",
		504: "DEADLINE_EXCEEDED", 500: "INTERNAL",
	}
	for status, want := range cases {
		if got := googleStatus(status); got != want {
			t.Errorf("googleStatus(%d) = %s, want %s", status, got, want)
		}
	}
}

func TestWriteFailureHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	f := &failure{code: CodeRateLimited, detail: "bucket empty\nfor 1.2s", retryAfter: 1200 * time.Millisecond}
	writeFailure(rec, v1.DialectOpenAI, "req_1", f)
	if rec.Code != 429 {
		t.Errorf("status %d", rec.Code)
	}
	h := rec.Header()
	if h.Get(HeaderError) != "rate_limited" || h.Get(HeaderErrorDetail) != "bucket empty%0Afor 1.2s" || h.Get("Retry-After") != "2" || h.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", h)
	}
	if f.Error() != "rate_limited: bucket empty\nfor 1.2s" || fail(CodeNotFound, "").Error() != "not_found" {
		t.Error("failure.Error renders wrongly")
	}
	r := &Refusal{Code: CodeSpendExceeded, Detail: "window full"}
	if r.Error() != "spend_exceeded: window full" || (&Refusal{Code: CodeSpendExceeded}).Error() != "spend_exceeded" {
		t.Error("Refusal.Error renders wrongly")
	}
}

// TestWriteRefusalPicksTheDoorShape: a refusal written before the handler
// takes the shape of the door its path names, the lux shape under no
// door, and carries the code, the detail, and Retry-After like any
// failure the handler writes.
func TestWriteRefusalPicksTheDoorShape(t *testing.T) {
	for path, want := range map[string]string{
		"/openai/v1/chat/completions": `"type":"rate_limited"`,
		"/anthropic/v1/messages":      `"type":"error"`,
		"/gemini/v1beta/models":       `"status":"RESOURCE_EXHAUSTED"`,
		"/lux/v1/generate":            `"request_id":"req_1"`,
		"/v1/keys":                    `"request_id":"req_1"`,
	} {
		rec := httptest.NewRecorder()
		WriteRefusal(rec, path, "req_1", CodeRateLimited, "address 203.0.113.9 is spent", 1500*time.Millisecond)
		h := rec.Header()
		if rec.Code != 429 || h.Get(HeaderError) != "rate_limited" || h.Get("Retry-After") != "2" || h.Get(HeaderErrorDetail) != "address 203.0.113.9 is spent" {
			t.Errorf("%s: status %d headers %v", path, rec.Code, h)
		}
		if body := rec.Body.String(); !strings.Contains(body, want) || !strings.Contains(body, CodeRateLimited.Message()) {
			t.Errorf("%s: body %s lacks %s", path, body, want)
		}
	}
}

func TestStreamErrorFrames(t *testing.T) {
	f := fail(CodeUpstreamError, "cut")
	oa := string(streamErrorFrame(v1.DialectOpenAI, "req_1", f))
	if !strings.HasPrefix(oa, "data: {\"error\":") || !strings.HasSuffix(oa, "}\n\n") || strings.Contains(oa, "[DONE]") {
		t.Errorf("openai frame %q", oa)
	}
	for _, d := range []v1.Dialect{v1.DialectAnthropic, v1.DialectLux} {
		fr := string(streamErrorFrame(d, "req_1", f))
		if !strings.HasPrefix(fr, "event: error\ndata: {") || !strings.HasSuffix(fr, "}\n\n") {
			t.Errorf("%s frame %q", d, fr)
		}
	}
	if streamErrorFrame(v1.DialectGemini, "req_1", f) != nil || streamErrorFrame("", "req_1", f) != nil {
		t.Error("the gemini door writes an error frame of its own")
	}
}
