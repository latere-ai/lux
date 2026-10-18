// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The fixture's Keys of spec 049: one that opens the lux door alone, and
// one that opens the lux door alone with passthrough.
const (
	luxOnlyValue     = "lux_luxonly0123456789abcdefghijklmnopqrstuv"
	luxOnlyPassValue = "lux_luxonlypass0123456789abcdefghijklmnopqr"
)

// doorRequest is one request on each door the fixture serves: a model
// route, the door's model list, its model read, and an opaque route on
// the three doors that have them, so a door refusal can be held to every
// operation a door has.
type doorRequest struct {
	name, method, path, body string
}

func doorRequests(d v1.Dialect) []doorRequest {
	switch d {
	case v1.DialectOpenAI:
		return []doorRequest{
			{"chat", http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)},
			{"models", http.MethodGet, "/openai/v1/models", ""},
			{"model", http.MethodGet, "/openai/v1/models/gpt", ""},
			{"opaque", http.MethodPost, "/openai/v1/files", `{}`},
		}
	case v1.DialectAnthropic:
		return []doorRequest{
			{"messages", http.MethodPost, "/anthropic/v1/messages", messagesBody("claude", false)},
			{"models", http.MethodGet, "/anthropic/v1/models", ""},
			{"model", http.MethodGet, "/anthropic/v1/models/claude", ""},
			{"opaque", http.MethodPost, "/anthropic/v1/files", `{}`},
		}
	case v1.DialectGemini:
		return []doorRequest{
			{"generate", http.MethodPost, "/gemini/v1beta/models/gemini:generateContent", `{"contents":[]}`},
			{"models", http.MethodGet, "/gemini/v1beta/models", ""},
			{"model", http.MethodGet, "/gemini/v1beta/models/gemini", ""},
			{"opaque", http.MethodPost, "/gemini/v1/files", `{}`},
		}
	default:
		return []doorRequest{
			{"generate", http.MethodPost, "/lux/v1/generate", luxBody("luxm", false)},
			{"models", http.MethodGet, "/lux/v1/models", ""},
			{"model", http.MethodGet, "/lux/v1/models/luxm", ""},
		}
	}
}

// doorsWorld is the fixture with the two Keys of spec 049 added and
// every stub answering its dialect.
func doorsWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.key("luxonly", luxOnlyValue, "*").Spec.Doors = []v1.Dialect{v1.DialectLux}
	pass := w.key("luxonlypass", luxOnlyPassValue, "*")
	pass.Spec.Doors, pass.Spec.Passthrough = []v1.Dialect{v1.DialectLux}, true
	w.openai.respondJSON(200, openaiChatResponse)
	w.anthropic.respondJSON(200, anthropicResponse)
	w.gemini.respondJSON(200, geminiResponse)
	w.lux.respondJSON(200, luxResponse)
	return w
}

// as sends r with value as the bearer.
func (w *world) as(value string, r doorRequest) *httptest.ResponseRecorder {
	req := w.request(r.method, r.path, r.body)
	req.Header.Set("Authorization", "Bearer "+value)
	return w.do(req)
}

// upstreamCalls is the requests every stub has read.
func (w *world) upstreamCalls() int {
	n := 0
	for _, s := range w.stubs {
		n += s.count()
	}
	return n
}

// TestKeyDoors is spec 049 at the door: a Key whose spec.doors names the
// lux door is served there and refused door_not_allowed, 403, in each
// other door's own error shape, on a model route, the door's model list,
// its model read, and an opaque route alike, with no upstream called and
// the door refusal before route_not_allowed; a Key with passthrough is
// refused the same way; a disabled or an expired Key answers its state
// first; and a Key without the member is served on every door.
func TestKeyDoors(t *testing.T) {
	w := doorsWorld(t)
	for _, d := range doors {
		for _, r := range doorRequests(d) {
			t.Run(string(d)+" "+r.name, func(t *testing.T) {
				before := w.upstreamCalls()
				for _, value := range []string{luxOnlyValue, luxOnlyPassValue} {
					rec := w.as(value, r)
					if d == v1.DialectLux {
						if rec.Code != http.StatusOK {
							t.Fatalf("its own door: %d %s %q", rec.Code, rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
						}
						continue
					}
					if got := errorCode(t, rec); got != CodeDoorNotAllowed {
						t.Fatalf("Key %s on %s: %s %q", value[:12], d, got, rec.Header().Get(HeaderErrorDetail))
					}
					checkEnvelope(t, d, rec, CodeDoorNotAllowed)
					detail := rec.Header().Get(HeaderErrorDetail)
					if !strings.Contains(detail, "/lux") || !strings.Contains(detail, "/"+string(d)) {
						t.Errorf("the detail names neither the Key's doors nor this one: %q", detail)
					}
				}
				if d != v1.DialectLux && w.upstreamCalls() != before {
					t.Errorf("a refused request reached an upstream %d times", w.upstreamCalls()-before)
				}
			})
		}
	}

	// The served requests on the lux door reached its upstream.
	if w.lux.count() == 0 {
		t.Error("no request on the lux door reached its upstream")
	}

	// A Key's state is decided before its doors.
	for name, value := range map[string]string{"disabled": disabledKeyValue, "expired": expiredKeyValue} {
		k, err := w.keys.ByHash(t.Context(), hashValue(value))
		if err != nil || k == nil {
			t.Fatalf("the %s Key: %v", name, err)
		}
		k.Spec.Doors = []v1.Dialect{v1.DialectLux}
		want := map[string]Code{"disabled": CodeKeyDisabled, "expired": CodeKeyExpired}[name]
		if rec := w.as(value, doorRequests(v1.DialectOpenAI)[0]); errorCode(t, rec) != want {
			t.Errorf("a %s Key on another door: %s", name, rec.Header().Get(HeaderError))
		}
	}

	// A Key without the member opens every door.
	for _, d := range doors {
		r := doorRequests(d)[0]
		if rec := w.as(keyValue, r); rec.Code != http.StatusOK {
			t.Errorf("a Key without doors on %s: %d %s", d, rec.Code, rec.Header().Get(HeaderError))
		}
	}
}

// TestKeyDoorsRecorded: a door refusal is recorded as every Key refusal
// is, refused with the code, the door, the Key's identity and labels,
// the request's labels, no attempt, and no tokens, and it is counted in
// lux_requests_total with its door and code.
func TestKeyDoorsRecorded(t *testing.T) {
	w, reg := metered(t)
	w.key("luxonly", luxOnlyValue, "*").Spec.Doors = []v1.Dialect{v1.DialectLux}
	req := w.request(http.MethodPost, "/anthropic/v1/messages", messagesBody("claude", false))
	req.Header.Set("Authorization", "Bearer "+luxOnlyValue)
	req.Header.Set(HeaderLabels, "trial=t-1")
	if rec := w.do(req); errorCode(t, rec) != CodeDoorNotAllowed {
		t.Fatalf("%s", rec.Header().Get(HeaderError))
	}
	got := w.recorder.last(t)
	if got.Status != StatusRefused || got.Error != CodeDoorNotAllowed || got.Door != v1.DialectAnthropic {
		t.Errorf("record status %s error %s door %s", got.Status, got.Error, got.Door)
	}
	if got.KeyID != "key_LUXONLY"+strings.Repeat("0", 19) || got.KeyPrefix != luxOnlyValue[:12] || got.Owner == "" || got.Labels["team"] != "luxonly" {
		t.Errorf("record Key %s prefix %s owner %q labels %v", got.KeyID, got.KeyPrefix, got.Owner, got.Labels)
	}
	if got.RequestLabels["trial"] != "t-1" || len(got.Attempts) != 0 || got.Tokens != (Tokens{}) {
		t.Errorf("record request labels %v attempts %d tokens %+v", got.RequestLabels, len(got.Attempts), got.Tokens)
	}
	found := false
	for _, labels := range series(scrape(reg), MetricRequests) {
		if labels["door"] == string(v1.DialectAnthropic) && labels["status"] == string(StatusRefused) && labels["code"] == string(CodeDoorNotAllowed) {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s series for the refusal:\n%s", MetricRequests, scrape(reg))
	}
}

// TestKeyDoorsOnReread: a Key reread past the cache before
// model_not_allowed passes through the door rule again, so a Key the
// store holds without this door is refused door_not_allowed, and one the
// store holds with it is served.
func TestKeyDoorsOnReread(t *testing.T) {
	w, keys := staleWorld(t)
	w.anthropic.respondJSON(200, anthropicResponse)
	k := restricted(t, w)
	narrowed := widened(k, "gpt", "alias", "claude")
	narrowed.Spec.Doors = []v1.Dialect{v1.DialectLux}
	keys.store(restrictedValue, narrowed)
	if rec := asRestricted(w, "claude"); errorCode(t, rec) != CodeDoorNotAllowed || keys.refreshes.Load() != 1 {
		t.Fatalf("a reread Key without the door: %s after %d rereads", rec.Header().Get(HeaderError), keys.refreshes.Load())
	}
	opened := widened(k, "gpt", "alias", "claude")
	opened.Spec.Doors = []v1.Dialect{v1.DialectLux, v1.DialectOpenAI}
	keys.store(restrictedValue, opened)
	if rec := asRestricted(w, "claude"); rec.Code != http.StatusOK {
		t.Fatalf("a reread Key with the door: %d %s", rec.Code, rec.Header().Get(HeaderError))
	}
}
