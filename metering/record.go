// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Status is a request's outcome as the record carries it: served,
// refused before any byte reached a provider, or failed at or after
// one. The gateway's Status has the same three values; the record
// carries its own type so this package imports the kinds and nothing
// that dials.
type Status string

// The three outcomes.
const (
	StatusOK      Status = "ok"
	StatusRefused Status = "refused"
	StatusFailed  Status = "failed"
)

// Tokens is the record's token block: what the upstream reported, or
// the estimate when it reported nothing, every count an int64. Input
// is the count billed at the input price and excludes CachedInput on
// every dialect; Reasoning is inside Output already and is carried for
// reporting, never as a term in the cost; Estimated marks the whole
// block as the estimator's rather than a tokenizer's.
type Tokens struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CachedInput int64 `json:"cachedInput"`
	CacheWrite  int64 `json:"cacheWrite"`
	Reasoning   int64 `json:"reasoning"`
	Estimated   bool  `json:"estimated"`
}

// Charge is the record's cost block: Amount is an integer count of
// micro-units of Currency, so 1250000 is 1.25, and never the money
// string a manifest writes; Priced is false for a Model with no
// pricing, a count route, and an opaque route, and Amount is then 0.
type Charge struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Priced   bool   `json:"priced"`
}

// Ref names one object of the catalog by name and id, the record's
// model and provider blocks; both members are empty when the request
// resolved no Model or reached no Provider.
type Ref struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// KeyRef is the record's key block: the Key's key_ id and its
// twelve-character prefix, which is how a person recognizes the Key
// without its value.
type KeyRef struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
}

// Attempt is one target tried, in order: the Provider's name, the
// upstream's own model name, the outcome, the upstream's status when
// one arrived, the failure's code, and how long the attempt took.
type Attempt struct {
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstreamModel"`
	Status        Status `json:"status"`
	HTTPStatus    int    `json:"httpStatus"`
	Error         string `json:"error"`
	DurationMs    int64  `json:"durationMs"`
}

// Record is one data plane request when it is done: the gateway's
// record with the cost added. It is a struct of scalars, slices of
// scalars, and two string maps, with no member of interface type and
// none that could carry a body, so invariant 7's second half, never a
// prompt, a completion, a header, a credential, a Key value, or the
// caller's address, is checked by reflection rather than promised.
type Record struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	EndedAt time.Time `json:"endedAt"`

	Key   KeyRef `json:"key"`
	Owner string `json:"owner"`

	Model         Ref    `json:"model"`
	Provider      Ref    `json:"provider"`
	UpstreamModel string `json:"upstreamModel"`

	Door          v1.Dialect `json:"door"`
	TargetDialect v1.Dialect `json:"targetDialect"`
	Route         string     `json:"route"`
	Translated    bool       `json:"translated"`
	Loss          []string   `json:"loss"`

	Attempts []Attempt `json:"attempts"`

	Status         Status `json:"status"`
	Error          string `json:"error"`
	UpstreamStatus int    `json:"upstreamStatus"`
	LatencyMs      int64  `json:"latencyMs"`
	TTFBMs         int64  `json:"ttfbMs"`

	Tokens Tokens `json:"tokens"`
	Cost   Charge `json:"cost"`
	Stream bool   `json:"stream"`

	Labels        map[string]string `json:"labels"`
	RequestLabels map[string]string `json:"requestLabels"`
}

// RecordsPerKey is the size of the ring of records a replica keeps per
// Key for GET /v1/requests when no archive is configured.
const RecordsPerKey = 1000
