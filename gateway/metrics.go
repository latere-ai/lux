// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"latere.ai/x/pkg/metrics"
)

// The three request metrics of spec 019 this package owns, with the
// bucket boundaries that spec declares: model latency is not web
// latency, and a stream can run for minutes.
const (
	MetricRequests        = "lux_requests_total"
	MetricRequestDuration = "lux_request_duration_seconds"
	MetricTimeToFirstByte = "lux_time_to_first_byte_seconds"
)

// The two upstream metrics of spec 005's row in spec 019's table, one
// observation per target tried, recorded here because the attempt is
// the one place that knows the Provider, the duration, and the outcome.
const (
	MetricUpstreamRequests = "lux_upstream_requests_total"
	MetricUpstreamDuration = "lux_upstream_duration_seconds"
)

// The boundaries in seconds, thirteen and ten. DurationBuckets serves
// the request duration and the upstream duration alike.
var (
	DurationBuckets        = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	TimeToFirstByteBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 60}
)

// The upstream status label's closed set: a response the caller gets,
// the request's own refusal by the provider included; the provider
// failing, a transport error, a 5xx, a refused credential, or a redirect;
// and the Provider's timeout.
const (
	UpstreamOK      = "ok"
	UpstreamError   = "error"
	UpstreamTimeout = "timeout"
)

// requestMetrics holds the five families on one registry.
type requestMetrics struct {
	requests         *metrics.Counter
	duration         *metrics.Histogram
	ttfb             *metrics.Histogram
	upstream         *metrics.Counter
	upstreamDuration *metrics.Histogram
}

func newRequestMetrics(reg *metrics.Registry) *requestMetrics {
	if reg == nil {
		return nil
	}
	return &requestMetrics{
		requests:         reg.Counter(MetricRequests, "Data plane requests by door, model, provider, status, and code."),
		duration:         reg.Histogram(MetricRequestDuration, "Data plane request duration in seconds by door, model, and status.", DurationBuckets),
		ttfb:             reg.Histogram(MetricTimeToFirstByte, "Seconds to the first response byte by door and model.", TimeToFirstByteBuckets),
		upstream:         reg.Counter(MetricUpstreamRequests, "Upstream attempts by provider and status: ok, error, or timeout."),
		upstreamDuration: reg.Histogram(MetricUpstreamDuration, "Upstream attempt duration in seconds by provider.", DurationBuckets),
	}
}

// observe records one finished request. Every label value is from a
// closed set or is the name of an object an operator declared: the
// model is the resolved Model's name and empty when the name resolved
// to nothing, so a flood of invented names adds no series.
func (m *requestMetrics) observe(rec Record) {
	if m == nil {
		return
	}
	code := ""
	if rec.Status != StatusOK {
		code = string(rec.Error)
	}
	m.requests.Inc(map[string]string{
		"door": string(rec.Door), "model": rec.Model, "provider": rec.Provider, "status": string(rec.Status), "code": code,
	})
	m.duration.Observe(map[string]string{"door": string(rec.Door), "model": rec.Model, "status": string(rec.Status)}, rec.Latency.Seconds())
	if rec.TTFB > 0 && rec.Status != StatusRefused {
		m.ttfb.Observe(map[string]string{"door": string(rec.Door), "model": rec.Model}, rec.TTFB.Seconds())
	}
}

// observeAttempt records one target tried.
func (m *requestMetrics) observeAttempt(at Attempt) {
	if m == nil {
		return
	}
	m.upstream.Inc(map[string]string{"provider": at.Provider, "status": upstreamStatus(at.Error)})
	m.upstreamDuration.Observe(map[string]string{"provider": at.Provider}, at.Duration.Seconds())
}

// upstreamStatus classifies an attempt's outcome for the provider's
// series. upstream_rejected is the request's own refusal and a caller
// that left is nobody's failure: both read ok, so the alert on the
// ratio names a provider that is failing and not a caller that is.
func upstreamStatus(code Code) string {
	switch code {
	case CodeUpstreamTimeout:
		return UpstreamTimeout
	case CodeUpstreamError, CodeProviderUnavailable:
		return UpstreamError
	default:
		return UpstreamOK
	}
}
