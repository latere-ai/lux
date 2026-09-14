// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/metering"
)

// sink is the operator's endpoint: it verifies every signature under
// the secret and keeps the bodies.
type sink struct {
	secret []byte
	mu     sync.Mutex
	got    [][]byte
	bad    int
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := events.Verify(s.secret, r.Header.Get(events.Header), body, time.Now(), 5*time.Minute); err != nil {
		s.bad++
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.got = append(s.got, body)
	w.WriteHeader(http.StatusNoContent)
}

func (s *sink) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.got {
		var rec events.Record
		_ = json.Unmarshal(b, &rec)
		out = append(out, rec.Type)
	}
	return out
}

// TestServeDeliversEventsToTheSink is spec 012's wiring through run: with
// LUX_EVENTS_URL and LUX_EVENTS_SECRET set, the start-up line names the
// sink and says a restart loses unacknowledged rows without a store, and
// a Budget applied through /v1 arrives at the sink as one signed
// budget.created within the worker's poll.
func TestServeDeliversEventsToTheSink(t *testing.T) {
	secret := []byte("whsec-9f8e7d")
	s := &sink{secret: secret}
	target := httptest.NewServer(s)
	defer target.Close()
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	srv := startServe(t, map[string]string{"LUX_OIDC_ISSUERS": iss.URL(), "LUX_EVENTS_URL": target.URL, "LUX_EVENTS_SECRET": string(secret)})
	defer srv.stop()
	waitFor(t, srv.out, "luxd: events: delivered to "+target.URL+"; the journal is this process's memory")
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}
	if resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/budgets/team", `{"spec": {"amount": "50"}}`, bearer...); resp.StatusCode != 201 {
		t.Fatalf("apply: %d %s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(s.types()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no delivery within ten seconds; stderr:\n%s", srv.errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.types(); got[0] != events.BudgetCreated || s.bad != 0 {
		t.Fatalf("delivered %v, %d unverified", got, s.bad)
	}
	if resp, body := do(t, http.MethodGet, srv.internalURL+"/metrics", ""); resp.StatusCode != 200 || !strings.Contains(body, "lux_events_pending 0") {
		t.Fatalf("/metrics: %d\n%s", resp.StatusCode, body)
	}
}

// TestServeArchivesTheRequestLog is the exporter's wiring through run:
// with LUX_REQUESTLOG_EXPORTER=s3 the start-up line names the bucket, a
// request through a door produces a record, and the stop drains the
// ring into one NDJSON object under the prefix; with the default nothing
// is archived and the line says so.
func TestServeArchivesTheRequestLog(t *testing.T) {
	bucket := s3test.New(t, "archive")
	srv := startServe(t, map[string]string{
		"LUX_REQUESTLOG_EXPORTER": "s3", "LUX_S3_ENDPOINT": bucket.URL(), "LUX_S3_BUCKET": "archive",
		"LUX_S3_ACCESS_KEY": s3test.Key, "LUX_S3_SECRET_KEY": s3test.Secret, "LUX_S3_PREFIX": "gateways/a/",
	})
	waitFor(t, srv.out, "luxd: request log: archived to bucket archive at "+bucket.URL()+" under gateways/a/, in batches of 5000 records or every 30s")
	if resp, _ := do(t, http.MethodGet, srv.publicURL+"/openai/v1/models", "", "Authorization", "Bearer lux_nosuchkey"); resp.StatusCode != 401 {
		t.Fatalf("the door answered %d", resp.StatusCode)
	}
	if code := srv.stop(); code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, srv.errOut.String())
	}
	keys := bucket.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "gateways/a/") || !strings.HasSuffix(keys[0], ".ndjson") {
		t.Fatalf("keys %v; stderr:\n%s", keys, srv.errOut.String())
	}
	data, _ := bucket.Get(keys[0])
	var rec metering.Record
	if err := json.Unmarshal([]byte(strings.TrimSuffix(string(data), "\n")), &rec); err != nil || rec.Status != metering.StatusRefused || rec.Error != "unauthenticated" {
		t.Fatalf("the archived record: %+v, %v\n%s", rec, err, data)
	}
	if ct, _ := bucket.ContentType(keys[0]); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type %q", ct)
	}

	plain := startServe(t, nil)
	defer plain.stop()
	waitFor(t, plain.out, "luxd: request log: not archived; GET /v1/requests reads this replica's memory")
	waitFor(t, plain.out, "luxd: events: off; set LUX_EVENTS_URL and LUX_EVENTS_SECRET to deliver them")
}
