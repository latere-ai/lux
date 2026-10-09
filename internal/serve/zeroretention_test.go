// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// bodies is an upstream that answers one chat completion and keeps the
// body of every request it read.
type bodies struct {
	mu   sync.Mutex
	seen []string
}

func (b *bodies) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	b.seen = append(b.seen, string(body))
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, openaiChatResponse)
}

func (b *bodies) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.seen)
}

func (b *bodies) last() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.seen) == 0 {
		return ""
	}
	return b.seen[len(b.seen)-1]
}

// TestZeroRetentionReachesTheDoorAtTheNextTail is spec 047's eighth
// criterion: a Key update that sets spec.zeroRetention is a key.updated
// row, and a replica serves the Key with the flag from its next journal
// read: the replica that took the write at once, the other after its
// own tail and not before. From then on a Model whose target declares
// nothing is refused zero_retention_unavailable without an upstream
// call, and a declaring target's upstream reads the zero-retention
// fields.
func TestZeroRetentionReachesTheDoorAtTheNextTail(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	declaring, plain := &bodies{}, &bodies{}
	ds, ps := httptest.NewServer(declaring), httptest.NewServer(plain)
	t.Cleanup(ds.Close)
	t.Cleanup(ps.Close)
	h.provider(t, "keeps-nothing", v1.DialectOpenAI, ds.URL+"/v1", func(p *v1.Provider) {
		p.Spec.Discovery.Mode = v1.DiscoveryNone
		p.Spec.ZeroRetention = &v1.ZeroRetention{RequestFields: map[string]any{"provider": map[string]any{"zdr": true}}}
	})
	h.provider(t, "plain", v1.DialectOpenAI, ps.URL+"/v1", func(p *v1.Provider) { p.Spec.Discovery.Mode = v1.DiscoveryNone })
	available := true
	for _, m := range []*v1.Model{h.declare(t, "private", "keeps-nothing", "gpt-4.1"), h.declare(t, "public", "plain", "gpt-4.1")} {
		if err := h.st.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Available: &available}); err != nil {
			t.Fatal(err)
		}
	}
	k, value := h.key(t, "workload", nil)
	type replica struct {
		cache *KeyCache
		doors *gateway.Handler
	}
	var replicas []replica
	for range 2 {
		cache, snap := h.replica(t, h.st)
		doors := gateway.New(gateway.Options{
			Keys: cache, Catalog: snap, Credentials: snap,
			Router:  gateway.NewTargetRouter(gateway.RouterOptions{Catalog: snap, Now: h.clock}),
			Clients: h.clients, Version: "test", Now: h.clock,
		})
		replicas = append(replicas, replica{cache, doors})
	}
	call := func(r replica, model string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(chat(model)))
		req.Header.Set("Authorization", "Bearer "+value)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.doors.ServeHTTP(rec, req)
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if rec.Code != http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("a refusal that is not the envelope: %v\n%s", err, rec.Body)
			}
		}
		return rec.Code, env.Error.Code
	}
	for i, r := range replicas {
		if status, _ := call(r, "public"); status != http.StatusOK {
			t.Fatalf("replica %d before the flag: %d", i, status)
		}
		if status, _ := call(r, "private"); status != http.StatusOK || strings.Contains(declaring.last(), `"zdr"`) {
			t.Fatalf("replica %d before the flag: %d, the upstream read %s", i, status, declaring.last())
		}
	}

	obj, version, err := h.st.Objects().Get(ctx, v1.KindKey, k.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur := obj.(*v1.Key)
	cur.Spec.ZeroRetention = true
	if _, err := h.st.Objects().Put(ctx, cur, version); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventKeyUpdated, cur)
	// The replica that took the write tails as its commit returns.
	replicas[0].cache.Tail(ctx)

	before := plain.count()
	if status, code := call(replicas[0], "public"); status != http.StatusForbidden || code != string(gateway.CodeZeroRetentionUnavailable) {
		t.Fatalf("the replica that tailed: %d %s", status, code)
	}
	if plain.count() != before {
		t.Fatal("the refused request reached the upstream that declares nothing")
	}
	if status, _ := call(replicas[1], "public"); status != http.StatusOK {
		t.Fatalf("the replica that has not tailed refused inside its window: %d", status)
	}
	replicas[1].cache.Tail(ctx)
	for i, r := range replicas {
		if status, code := call(r, "public"); code != string(gateway.CodeZeroRetentionUnavailable) {
			t.Fatalf("replica %d after its tail: %d %s", i, status, code)
		}
		if status, _ := call(r, "private"); status != http.StatusOK || !strings.Contains(declaring.last(), `"provider":{"zdr":true}`) {
			t.Fatalf("replica %d after its tail: %d, the upstream read %s", i, status, declaring.last())
		}
	}
}
