// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"errors"
	"net/http"
	"testing"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The Key read past the cache of spec 044: two data planes over one
// store, each with its own Key cache, stand for two replicas, and a Key
// changed through one is called through the other before that one's
// journal tail runs.

// widen writes k with selectors as one replica's API applies a change:
// the object and its key.updated row, then that replica's own tail, as
// the API's Committed hook runs it.
func (h *harness) widen(t *testing.T, through *plane, k *v1.Key, selectors ...string) {
	t.Helper()
	k.Spec.Models = selectors
	if _, err := h.st.Objects().Put(t.Context(), k, k.Status.Version); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventKeyUpdated, k)
	through.cache.Tail(t.Context())
}

// TestKeyWidenedThroughAnotherReplica: a Key whose models one replica
// widened is served the newly selected Model on the other replica at
// once, inside its cache window and before its tail, at the cost of one
// store read, after which that replica's cache holds the widened Key.
func TestKeyWidenedThroughAnotherReplica(t *testing.T) {
	h := newHarness(t)
	a := h.plane(t, nil)
	st, reg := h.instrumented()
	b := h.plane(t, st)
	oai, _ := a.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	a.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", nil)
	a.model(t, "mini", oai.Metadata.Name, "gpt-4.1-mini", nil)
	k, value := h.key(t, "session", func(k *v1.Key) { k.Spec.Models = []string{"gpt"} })
	for name, p := range map[string]*plane{"a": a, "b": b} {
		if rec := p.do(http.MethodPost, "/openai/v1/chat/completions", value, chat("gpt"), nil); rec.Code != http.StatusOK {
			t.Fatalf("replica %s before the change: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	h.widen(t, a, k, "gpt", "mini")
	if rec := a.do(http.MethodPost, "/openai/v1/chat/completions", value, chat("mini"), nil); rec.Code != http.StatusOK {
		t.Fatalf("the replica that applied the change: %d %s", rec.Code, rec.Body.String())
	}
	reads := ops(reg, "Keys.ByHash")
	rec := b.do(http.MethodPost, "/openai/v1/chat/completions", value, chat("mini"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("the other replica refused the widened Model: %d %s %s", rec.Code, rec.Header().Get(gateway.HeaderError), rec.Header().Get(gateway.HeaderErrorDetail))
	}
	if n := ops(reg, "Keys.ByHash") - reads; n != 1 {
		t.Errorf("the other replica read the Key %d times, want once", n)
	}
	if rec := b.do(http.MethodPost, "/openai/v1/chat/completions", value, chat("mini"), nil); rec.Code != http.StatusOK || ops(reg, "Keys.ByHash")-reads != 1 {
		t.Errorf("the next call: %d after %d reads; the cache does not hold the widened Key", rec.Code, ops(reg, "Keys.ByHash")-reads)
	}
}

// TestKeyLackingTheModelReadsOncePerRefusal: a Key the store also
// refuses the Model is refused model_not_allowed on every call, each
// refusal one store read and never more.
func TestKeyLackingTheModelReadsOncePerRefusal(t *testing.T) {
	h := newHarness(t)
	st, reg := h.instrumented()
	b := h.plane(t, st)
	oai, _ := b.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	b.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", nil)
	b.model(t, "mini", oai.Metadata.Name, "gpt-4.1-mini", nil)
	_, value := h.key(t, "narrow", func(k *v1.Key) { k.Spec.Models = []string{"gpt"} })
	if rec := b.do(http.MethodPost, "/openai/v1/chat/completions", value, chat("gpt"), nil); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	reads := ops(reg, "Keys.ByHash")
	for i := uint64(1); i <= 3; i++ {
		rec := b.do(http.MethodPost, "/openai/v1/chat/completions", value, chat("mini"), nil)
		if gateway.Code(rec.Header().Get(gateway.HeaderError)) != gateway.CodeModelNotAllowed {
			t.Fatalf("refusal %d: %d %s", i, rec.Code, rec.Body.String())
		}
		if n := ops(reg, "Keys.ByHash") - reads; n != i {
			t.Fatalf("after %d refusals, %d reads", i, n)
		}
	}
}

// TestRefresh holds the cache half: Refresh reads the store past a
// fresh entry and replaces it, counted as refresh; a Key deleted since
// is nil and leaves a negative entry; and a read that fails is the
// store's error and leaves the entry it would have replaced.
func TestRefresh(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	fail := map[string]bool{}
	st, reg := h.instrumented()
	c := h.cache(&broken{st, fail}, reg, DefaultKeyCache)
	k, value := h.key(t, "cached", func(k *v1.Key) { k.Spec.Models = []string{"gpt"} })
	hash := HashKeyValue(value)
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil {
		t.Fatalf("%v, %v", got, err)
	}
	k.Spec.Models = []string{"gpt", "mini"}
	if _, err := h.st.Objects().Put(ctx, k, k.Status.Version); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.ByHash(ctx, hash); len(got.Spec.Models) != 1 {
		t.Fatal("the change was seen before a tail or a refresh")
	}
	got, err := c.Refresh(ctx, hash)
	if err != nil || got == nil || len(got.Spec.Models) != 2 {
		t.Fatalf("Refresh = %v, %v", got, err)
	}
	if got, _ := c.ByHash(ctx, hash); len(got.Spec.Models) != 2 || hits(reg, "refresh") != 1 || hits(reg, "hit") != 2 {
		t.Errorf("after a refresh the cache serves %v; refresh %d hit %d", got.Spec.Models, hits(reg, "refresh"), hits(reg, "hit"))
	}

	fail["Keys.ByHash"] = true
	if got, err := c.Refresh(ctx, hash); !errors.Is(err, errBroken) || got != nil {
		t.Errorf("a failed read: %v, %v", got, err)
	}
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil || len(got.Spec.Models) != 2 {
		t.Errorf("a failed read dropped the entry: %v, %v", got, err)
	}
	delete(fail, "Keys.ByHash")

	if err := h.st.Transact(ctx, func(tx store.Store) error {
		if err := tx.Objects().Delete(ctx, v1.KindKey, k.Status.ID); err != nil {
			return err
		}
		return tx.Keys().Delete(ctx, k.Status.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Refresh(ctx, hash); err != nil || got != nil {
		t.Fatalf("a deleted Key: %v, %v", got, err)
	}
	if got, err := c.ByHash(ctx, hash); err != nil || got != nil || hits(reg, "negative") != 1 {
		t.Errorf("after the refresh the cache serves %v, %v; negative %d", got, err, hits(reg, "negative"))
	}
}
