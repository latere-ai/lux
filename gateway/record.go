// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Status is a request's outcome.
type Status string

// The outcomes: served, refused before any byte reached a provider, or
// failed at or after one.
const (
	StatusOK      Status = "ok"
	StatusRefused Status = "refused"
	StatusFailed  Status = "failed"
)

// Tokens is what the upstream reported, or the estimate when it reported
// nothing. Input excludes CachedInput on every dialect; Reasoning is
// carried for reporting and is inside Output already. Estimated marks
// the whole block as the estimator's rather than a tokenizer's.
type Tokens struct {
	Input       int64
	Output      int64
	CachedInput int64
	CacheWrite  int64
	Reasoning   int64
	Estimated   bool
}

// Attempt is one target tried, in order.
type Attempt struct {
	Provider      string // the Provider's name
	ProviderID    string
	UpstreamModel string
	Status        Status // StatusOK or StatusFailed
	HTTPStatus    int    // the upstream's status, 0 when none arrived
	Error         Code   // the failure's code, empty on success
	Duration      time.Duration
}

// Record is what the pipeline knows about one request when it is done,
// and what the Recorder receives: never a body, a header value, a
// credential, a Key value, or the caller's address. Spec 009's
// metering.Record is this with the cost added.
type Record struct {
	ID      string    // req_ and a ULID, the Lux-Request-Id
	At      time.Time // when the request arrived
	EndedAt time.Time // when the response was finished or refused

	KeyID     string
	KeyPrefix string
	Owner     string
	Labels    map[string]string // the Key's metadata.labels

	Model         string // the resolved Model's name; empty when none resolved
	ModelID       string
	Provider      string // the answering Provider's name; empty when none was reached
	ProviderID    string
	UpstreamModel string

	Door          v1.Dialect
	TargetDialect v1.Dialect // empty when no target was chosen
	Route         string     // the route template of the door table
	Class         RouteClass
	Translated    bool
	Loss          []string // ir.Loss.Strings() of the translation

	Attempts []Attempt

	Status         Status
	Error          Code // the code that answered the caller, or ClientClosed; empty when ok
	UpstreamStatus int  // the last upstream status; 0 when none arrived
	Latency        time.Duration
	TTFB           time.Duration // to the first byte written to the caller; 0 when none was

	Tokens Tokens
	Stream bool

	RequestLabels map[string]string // the accepted pairs of Lux-Labels, at most eight
}
