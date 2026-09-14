// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/metrics"
)

// TestAuthorizerObserverRenamesError: every call counts by decision with
// the client's error renamed unavailable, the duration is observed, and
// /metrics serves both families in the text format.
func TestAuthorizerObserverRenamesError(t *testing.T) {
	reg := metrics.NewRegistry()
	observe := AuthorizerObserver(reg)
	observe("allow", 0.01)
	observe("deny", 0.02)
	observe("error", 0.5)
	observe("error", 0.6)
	requests := reg.Counter(MetricAuthorizerRequests, "")
	if requests.Value(map[string]string{"decision": "allow"}) != 1 || requests.Value(map[string]string{"decision": "deny"}) != 1 || requests.Value(map[string]string{"decision": "unavailable"}) != 2 || requests.Value(map[string]string{"decision": "error"}) != 0 {
		t.Error("the decisions were counted wrongly")
	}
	if reg.Histogram(MetricAuthorizerDuration, "", nil).Count(map[string]string{"decision": "unavailable"}) != 2 {
		t.Error("the durations were not observed")
	}
	rec := httptest.NewRecorder()
	Metrics(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{"# TYPE lux_authorizer_requests_total counter", `lux_authorizer_requests_total{decision="unavailable"} 2`, "# TYPE lux_authorizer_duration_seconds histogram"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the exposition lacks %q:\n%s", want, rec.Body.String())
		}
	}
}
