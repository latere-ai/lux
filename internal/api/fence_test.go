// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/store"
)

func TestKeyFenceLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	made := h.request(http.MethodPut, "/v1/keys/closed", keyJSON)
	if made.Code != http.StatusCreated {
		t.Fatal(made.Code, made.Body)
	}
	secret := keyValue(t, made)
	assertion := fmt.Sprintf(`{"owner":%q,"labels":{}}`, h.subject())
	endpoint := "/v1/keys/closed/fence"
	installed := h.request(http.MethodPost, endpoint, assertion)
	if installed.Code != http.StatusOK {
		t.Fatal(installed.Code, installed.Body)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		payload := ""
		if method == http.MethodPost {
			payload = assertion
		}
		got := h.request(method, endpoint, payload)
		if got.Code != http.StatusOK || !reflect.DeepEqual(body(t, got), body(t, installed)) {
			t.Fatal(got.Code, got.Body)
		}
		if strings.Contains(got.Body.String(), secret) || strings.Contains(got.Body.String(), "spec") || strings.Contains(got.Body.String(), "hash") {
			t.Fatal("fence exposed credentials/policy")
		}
	}
	wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/rotate", ""), CodeKeyFenced)
	wantCode(t, h.request(http.MethodPut, "/v1/keys/closed", keyJSON), CodeKeyFenced)
	// The original TTL, budget and metadata are retained by the normal API.
	disabled := strings.Replace(keyJSON, `"models":`, `"disabled":true,"models":`, 1)
	got := h.request(http.MethodPut, "/v1/keys/closed", disabled, "If-Match", made.Header().Get("ETag"))
	if got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body)
	}
	wantCode(t, h.request(http.MethodPut, "/v1/keys/closed", keyJSON), CodeKeyFenced)
	if got := h.request(http.MethodDelete, "/v1/keys/closed", ""); got.Code != http.StatusNoContent {
		t.Fatal(got.Code, got.Body)
	}
	wantCode(t, h.request(http.MethodPut, "/v1/keys/closed", keyJSON), CodeKeyFenced)
	wantCode(t, h.request(http.MethodPost, endpoint, `{"owner":"https://issuer.example|other"}`), CodeFenceConflict)
}

func TestKeyFenceAuthorizationAndOccupant(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	assertion := fmt.Sprintf(`{"owner":%q,"labels":{"tenant":"one"}}`, h.subject())
	h.stub.Deny(stub.Rule{Action: authorizer.ActionKeyFence}, "deny fence")
	wantCode(t, h.request(http.MethodPost, "/v1/keys/absent/fence", assertion), CodeForbidden)
	if _, err := h.st.KeyFences().Get(t.Context(), "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	h.stub.SetRules()
	h.advance(2 * time.Minute)
	h.stub.ClearRequests()
	got := h.request(http.MethodPost, "/v1/keys/absent/fence", assertion)
	if got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body)
	}
	requests := h.stub.Requests()
	if len(requests) != 1 || requests[0].Resource.Kind != "KeyFence" || requests[0].Resource.ID != "absent" || requests[0].Resource.String("owner") != h.subject() {
		t.Fatal(requests)
	}
	wantCode(t, h.request(http.MethodPut, "/v1/keys/absent", keyJSON), CodeKeyFenced)
	h.stub.Deny(stub.Rule{Action: authorizer.ActionKeyFenceRead}, "deny read")
	for _, name := range []string{"absent", "unknown"} {
		wantCode(t, h.request(http.MethodGet, "/v1/keys/"+name+"/fence", ""), CodeForbidden)
	}
	h.stub.SetRules()
	h.advance(2 * time.Minute)
	wantCode(t, h.request(http.MethodGet, "/v1/keys/unknown/fence", ""), CodeNotFound)
	if got := h.request(http.MethodPut, "/v1/keys/occupied", keyJSON); got.Code != http.StatusCreated {
		t.Fatal(got.Body)
	}
	wantCode(t, h.request(http.MethodPost, "/v1/keys/occupied/fence", `{"owner":"https://issuer.example|other"}`), CodeFenceConflict)
	if _, err := h.st.KeyFences().Get(t.Context(), "occupied"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestKeyFenceValidation(t *testing.T) {
	h := newHarness(t, nil)
	good := fmt.Sprintf(`{"owner":%q}`, h.subject())
	for _, raw := range []string{``, `[]`, `null`, `{`, `{"owner":`, `{"owner":"x","owner":"y"}`, `{"labels":{"x":"a","x":"b"}}`, `{"extra":true}`, `{"owner":42}`, `{"labels":null}`, `{"labels":[]}`, `{"labels":{"x":null}}`, `{"labels":{"x":42}}`, `{} {}`, `{} trailing`} {
		t.Run(raw, func(t *testing.T) {
			wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/fence", raw, "Content-Type", "application/json"), CodeInvalidRequest)
		})
	}
	for _, owner := range []string{"", "no-subject", "|empty", "issuer|", " issuer|person", strings.Repeat("x", 2050) + "|person"} {
		raw, _ := json.Marshal(map[string]string{"owner": owner})
		wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/fence", string(raw)), CodeInvalidField)
	}
	for _, name := range []string{"UPPER", "key_some-id", "-leading", strings.Repeat("x", 64)} {
		wantCode(t, h.request(http.MethodPost, "/v1/keys/"+name+"/fence", good), CodeInvalidField)
	}
	for _, header := range []string{"If-Match", "If-None-Match"} {
		wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/fence", good, header, "*"), CodeInvalidField)
	}
	wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/fence", good, "Content-Type", "application/yaml"), CodeUnsupportedMediaType)
	wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/fence", strings.Repeat("x", 5000)), CodeBodyTooLarge)
	wantCode(t, h.request(http.MethodPost, "/v1/keys/closed/fence", good, "Authorization", ""), CodeUnauthenticated)
	files, _ := fileModeHarness(t)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		got := httptest.NewRecorder()
		files.ServeHTTP(got, httptest.NewRequest(method, "/v1/keys/closed/fence", strings.NewReader(good)))
		code := CodeReadOnly
		if method == http.MethodGet {
			code = CodeNotFound
		}
		wantCode(t, got, code)
	}
}

func TestKeyFenceAuditIsAtomicAndIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	assertion := fmt.Sprintf(`{"owner":%q,"labels":{"tenant":"one"}}`, h.subject())
	const endpoint = "/v1/keys/concurrent/fence"
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for range 8 {
		wg.Go(func() { responses <- h.request(http.MethodPost, endpoint, assertion) })
	}
	wg.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body)
		}
	}
	rows, err := h.st.Journal().Since(t.Context(), 0, 100)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	var record events.Record
	if err := json.Unmarshal(rows[0].Payload, &record); err != nil {
		t.Fatal(err)
	}
	if record.Type != events.KeyFenced || record.Object.Kind != "KeyFence" || record.Object.ID != "key-fence/concurrent" || record.Object.Owner != h.subject() || record.Object.Labels["tenant"] != "one" || record.Subject != h.subject() || record.RequestID == "" || record.Reason != events.ReasonRequest {
		t.Fatal(record)
	}
	if err := h.st.Journal().Acknowledge(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Journal().Prune(t.Context(), h.clock().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := h.request(http.MethodPost, endpoint, assertion); got.Code != http.StatusOK {
		t.Fatal(got.Code, got.Body)
	}
	if rows, err := h.st.Journal().Since(t.Context(), 0, 100); err != nil || len(rows) != 0 {
		t.Fatal("replay recreated pruned event", rows, err)
	}

	broken := newHarness(t, func(o *Options) { o.Store = fenceJournalFailure{o.Store} })
	got := broken.request(http.MethodPost, "/v1/keys/rollback/fence", fmt.Sprintf(`{"owner":%q}`, broken.subject()))
	wantCode(t, got, CodeStoreUnavailable)
	if _, err := broken.st.KeyFences().Get(t.Context(), "rollback"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("fence survived journal failure", err)
	}
}

type fenceJournalFailure struct{ store.Store }

func (f fenceJournalFailure) Transact(ctx context.Context, fn func(store.Store) error) error {
	return f.Store.Transact(ctx, func(tx store.Store) error { return fn(fenceJournalFailure{tx}) })
}
func (f fenceJournalFailure) Journal() store.Journal { return failedFenceJournal{f.Store.Journal()} }

type failedFenceJournal struct{ store.Journal }

func (f failedFenceJournal) Append(context.Context, store.Event) (int64, error) { return 0, errDown }

func TestKeyFenceRejectsAlreadyAuthorizedMutation(t *testing.T) {
	for _, operation := range []string{"create", "update", "rotate"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t, nil)
			h.seed()
			if operation != "create" {
				if got := h.request(http.MethodPut, "/v1/keys/delayed", keyJSON); got.Code != http.StatusCreated {
					t.Fatal(got.Body)
				}
			}
			paused := &fencePausedTransaction{Store: h.st, started: make(chan struct{}), release: make(chan struct{})}
			h.h.o.Store = paused
			method, path, payload := http.MethodPut, "/v1/keys/delayed", keyJSON
			if operation == "rotate" {
				method, path, payload = http.MethodPost, path+"/rotate", ""
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- h.request(method, path, payload) }()
			<-paused.started
			fenced := h.request(http.MethodPost, "/v1/keys/delayed/fence", fmt.Sprintf(`{"owner":%q}`, h.subject()))
			close(paused.release)
			if fenced.Code != http.StatusOK {
				t.Fatal(fenced.Code, fenced.Body)
			}
			wantCode(t, <-done, CodeKeyFenced)
		})
	}
}

type fencePausedTransaction struct {
	store.Store
	started, release chan struct{}
	once             sync.Once
}

func (p *fencePausedTransaction) Transact(ctx context.Context, fn func(store.Store) error) error {
	pause := false
	p.once.Do(func() { pause = true; close(p.started) })
	if pause {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Store.Transact(ctx, fn)
}
