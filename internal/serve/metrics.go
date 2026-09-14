// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"net/http"

	"latere.ai/x/pkg/metrics"
)

// The two authorizer metrics of spec 006 in spec 019's table, recorded
// by the serve wiring from the shared client's Observe, and the one
// /metrics handler over the process's registry.
const (
	MetricAuthorizerRequests = "lux_authorizer_requests_total"
	MetricAuthorizerDuration = "lux_authorizer_duration_seconds"
)

// AuthorizerObserver is the Observe the shared authorizer client takes:
// every call counts by decision and its duration is observed, with the
// client's error renamed unavailable, because that is the code the
// caller sees and an operator reading the alert should not translate.
func AuthorizerObserver(reg *metrics.Registry) func(result string, seconds float64) {
	requests := reg.Counter(MetricAuthorizerRequests, "Authorizer calls by decision: allow, deny, or unavailable.")
	duration := reg.Histogram(MetricAuthorizerDuration, "Authorizer call duration in seconds by decision.", metrics.DefaultDurationBuckets)
	return func(result string, seconds float64) {
		if result == "error" {
			result = "unavailable"
		}
		labels := map[string]string{"decision": result}
		requests.Inc(labels)
		duration.Observe(labels, seconds)
	}
}

// Metrics serves the registry in the Prometheus text format, the
// /metrics of the internal listener.
func Metrics(reg *metrics.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		reg.WritePrometheus(w)
	})
}
