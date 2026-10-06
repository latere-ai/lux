// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// staleKeys is a KeyRefresher over the fixture's Keys: ByHash answers
// what a replica's cache holds, Refresh what the store holds, which a
// test sets apart from the cache, and every Refresh is counted.
type staleKeys struct {
	*fakeKeys
	mu        sync.Mutex
	stored    map[string]*v1.Key // by hash; a hash absent here is the cached Key
	gone      map[string]bool    // by hash: the store holds no Key for it
	err       error
	refreshes atomic.Int64
}

func (s *staleKeys) Refresh(_ context.Context, hash string) (*v1.Key, error) {
	s.refreshes.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.gone[hash] {
		return nil, nil
	}
	if k, ok := s.stored[hash]; ok {
		return k, nil
	}
	return s.fakeKeys.byHash[hash], nil
}

// store sets the Key the store holds for value.
func (s *staleKeys) store(value string, k *v1.Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored[hashValue(value)] = k
}

// staleWorld is the fixture with its Keys behind staleKeys.
func staleWorld(t *testing.T) (*world, *staleKeys) {
	t.Helper()
	w := newWorld(t)
	keys := &staleKeys{fakeKeys: w.keys, stored: map[string]*v1.Key{}, gone: map[string]bool{}}
	o := w.options()
	o.Keys = keys
	w.h = New(o)
	return w, keys
}

// restricted is the fixture's Key that selects gpt and alias alone.
func restricted(t *testing.T, w *world) *v1.Key {
	t.Helper()
	k, err := w.keys.ByHash(t.Context(), hashValue(restrictedValue))
	if err != nil || k == nil {
		t.Fatalf("the restricted Key: %v, %v", k, err)
	}
	return k
}

// widened is a copy of k, as the store holds it after a change, with
// its selectors replaced and a label of the change.
func widened(k *v1.Key, selectors ...string) *v1.Key {
	c := *k
	c.Spec.Models = selectors
	c.Metadata.Labels = map[string]string{"team": "widened"}
	return &c
}

// asRestricted posts a chat for model with the restricted Key's value.
func asRestricted(w *world, model string) *httptest.ResponseRecorder {
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody(model, false))
	r.Header.Set("Authorization", "Bearer "+restrictedValue)
	return w.do(r)
}

// TestRereadBeforeModelNotAllowed is spec 044 at the door: a cached Key
// that does not select the Model is read once from the store before the
// refusal, and the store's Key, which selects it, serves the request and
// is the record's; a Model the cached Key selects is served with no
// reread.
func TestRereadBeforeModelNotAllowed(t *testing.T) {
	w, keys := staleWorld(t)
	w.anthropic.respondJSON(200, anthropicResponse)
	w.openai.respondJSON(200, openaiChatResponse)
	k := restricted(t, w)
	keys.store(restrictedValue, widened(k, "gpt", "alias", "claude"))

	rec := asRestricted(w, "claude")
	if rec.Code != http.StatusOK {
		t.Fatalf("the widened Model: %d %s %s", rec.Code, rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	if n := keys.refreshes.Load(); n != 1 {
		t.Errorf("%d rereads, want one", n)
	}
	if got := w.recorder.last(t); got.KeyID != k.Status.ID || got.Labels["team"] != "widened" {
		t.Errorf("the record names Key %s labels %v, want the store's", got.KeyID, got.Labels)
	}
	if got := w.limiter.last(); got.Key == nil || !allowed(got.Key, "claude") {
		t.Errorf("the reservation was made for the cached Key: %+v", got.Key)
	}

	if rec := asRestricted(w, "gpt"); rec.Code != http.StatusOK {
		t.Fatalf("a selected Model: %d %s", rec.Code, rec.Body.String())
	}
	if n := keys.refreshes.Load(); n != 1 {
		t.Errorf("a served request reread the Key: %d rereads", n)
	}
}

// TestRereadIsOnePerRefusal: a Key the store also refuses the Model is
// refused model_not_allowed with one reread per request, never more,
// whether the Model is called or read on the door; the door's model
// list rereads nothing.
func TestRereadIsOnePerRefusal(t *testing.T) {
	w, keys := staleWorld(t)
	for i := 1; i <= 3; i++ {
		rec := asRestricted(w, "claude")
		if errorCode(t, rec) != CodeModelNotAllowed || !strings.Contains(rec.Header().Get(HeaderErrorDetail), `matches "claude"`) {
			t.Fatalf("request %d: %s %q", i, rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
		}
		if n := keys.refreshes.Load(); n != int64(i) {
			t.Fatalf("after %d refusals, %d rereads", i, n)
		}
	}
	read := w.request(http.MethodGet, "/openai/v1/models/claude", "")
	read.Header.Set("Authorization", "Bearer "+restrictedValue)
	if rec := w.do(read); errorCode(t, rec) != CodeModelNotAllowed || keys.refreshes.Load() != 4 {
		t.Errorf("the model read: %s after %d rereads", rec.Header().Get(HeaderError), keys.refreshes.Load())
	}
	list := w.request(http.MethodGet, "/openai/v1/models", "")
	list.Header.Set("Authorization", "Bearer "+restrictedValue)
	if rec := w.do(list); rec.Code != http.StatusOK || keys.refreshes.Load() != 4 {
		t.Errorf("the model list: %d after %d rereads", rec.Code, keys.refreshes.Load())
	}
}

// TestRereadOutcomes: the store's answer decides what the cached Key
// could not. A reread that fails leaves the cached refusal with the
// failure in its detail; a value no Key has any longer is
// unauthenticated and the record names no Key; a Key disabled in the
// store is key_disabled; and a KeyLookup that cannot reread refuses on
// the cached Key.
func TestRereadOutcomes(t *testing.T) {
	t.Run("the read fails", func(t *testing.T) {
		w, keys := staleWorld(t)
		keys.err = errors.New("the store is down")
		rec := asRestricted(w, "claude")
		if errorCode(t, rec) != CodeModelNotAllowed || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "rereading the Key failed: the store is down") {
			t.Errorf("%s %q", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
		}
	})
	t.Run("the Key is gone", func(t *testing.T) {
		w, keys := staleWorld(t)
		keys.gone[hashValue(restrictedValue)] = true
		rec := asRestricted(w, "claude")
		if errorCode(t, rec) != CodeUnauthenticated {
			t.Fatalf("%s %q", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
		}
		if got := w.recorder.last(t); got.KeyID != "" || got.KeyPrefix != "" || got.Owner != "" || got.Labels != nil {
			t.Errorf("an unauthenticated record names a Key: %+v", got)
		}
	})
	t.Run("the Key is disabled", func(t *testing.T) {
		w, keys := staleWorld(t)
		k := widened(restricted(t, w), "*")
		k.Spec.Disabled = true
		keys.store(restrictedValue, k)
		if rec := asRestricted(w, "claude"); errorCode(t, rec) != CodeKeyDisabled {
			t.Errorf("%s %q", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
		}
	})
	t.Run("no Refresh", func(t *testing.T) {
		w := newWorld(t)
		if rec := asRestricted(w, "claude"); errorCode(t, rec) != CodeModelNotAllowed || w.keys.calls.Load() != 1 {
			t.Errorf("%s after %d lookups", rec.Header().Get(HeaderError), w.keys.calls.Load())
		}
	})
}
