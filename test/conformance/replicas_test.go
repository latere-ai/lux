// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestKeyWidenedOnOneReplicaServesOnTheOther is spec 044 over two
// reference servers that share one store, two replicas of one
// installation. A Key's models widened through the first replica's API
// are served on the second replica's door at once: inside the second
// replica's Key cache window and before its journal tail, which this
// test holds an hour away so that only the door's reread can serve it.
func TestKeyWidenedOnOneReplicaServesOnTheOther(t *testing.T) {
	shared := memory.New()
	a := startServer(t, serverOptions{store: shared})
	b := startServer(t, serverOptions{store: shared, keyTail: time.Hour, keyCache: time.Hour})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"one","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	token := a.mint("alice")
	apply := func(kind, name string, spec map[string]any) map[string]any {
		t.Helper()
		resp := a.control(t, token, http.MethodPut, "/v1/"+plurals[kind]+"/"+name, map[string]any{"spec": spec})
		if resp.Status != http.StatusCreated && resp.Status != http.StatusOK {
			t.Fatalf("PUT %s %s: %d %s", kind, name, resp.Status, excerpt(resp.Body))
		}
		return resp.json(t)
	}
	apply(v1.KindProvider, "replicas-openai", map[string]any{
		"dialect": "openai", "baseURL": upstream.URL + "/v1",
		"discovery": map[string]any{"mode": "none"}, "health": map[string]any{"mode": "none"},
	})
	for _, name := range []string{"replicas-one", "replicas-two"} {
		apply(v1.KindModel, name, map[string]any{"targets": []map[string]any{{"provider": "replicas-openai", "model": "one"}}})
	}
	value := str(apply(v1.KindKey, "replicas-session", map[string]any{"models": []string{"replicas-one"}}), "status.value")

	// control sends any request with a bearer; on a door the bearer is
	// the Key's value.
	chatOn := func(s *server, model string) *response {
		t.Helper()
		return s.control(t, value, http.MethodPost, "/openai/v1/chat/completions", chat(model, false))
	}
	// The second replica caches the Key as it is before the change.
	if resp := chatOn(b, "replicas-one"); resp.Status != http.StatusOK {
		t.Fatalf("the second replica before the change: %d %s", resp.Status, excerpt(resp.Body))
	}

	apply(v1.KindKey, "replicas-session", map[string]any{"models": []string{"replicas-one", "replicas-two"}})
	resp := chatOn(b, "replicas-two")
	if resp.Status != http.StatusOK {
		t.Fatalf("the second replica refused the widened Model: %d %s %q", resp.Status, resp.Header.Get(gateway.HeaderError), resp.Header.Get(gateway.HeaderErrorDetail))
	}
	if resp := chatOn(b, "replicas-one"); resp.Status != http.StatusOK {
		t.Errorf("the Model selected before the change: %d %s", resp.Status, excerpt(resp.Body))
	}
}
