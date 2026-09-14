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

// The boundaries in seconds, thirteen and ten.
var (
	DurationBuckets        = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	TimeToFirstByteBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 60}
)

// requestMetrics holds the three families on one registry.
type requestMetrics struct {
	requests *metrics.Counter
	duration *metrics.Histogram
	ttfb     *metrics.Histogram
}

func newRequestMetrics(reg *metrics.Registry) *requestMetrics {
	if reg == nil {
		return nil
	}
	return &requestMetrics{
		requests: reg.Counter(MetricRequests, "Data plane requests by door, model, provider, status, and code."),
		duration: reg.Histogram(MetricRequestDuration, "Data plane request duration in seconds by door, model, and status.", DurationBuckets),
		ttfb:     reg.Histogram(MetricTimeToFirstByte, "Seconds to the first response byte by door and model.", TimeToFirstByteBuckets),
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
