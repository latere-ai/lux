// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"net/http"
	"time"

	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Options is what New builds a Handler from. Keys, Catalog, Credentials,
// Router, and Clients are required, because a door cannot answer without
// them; Limiter, Recorder, Health, and Metrics may be nil, and a nil one
// is a handler that reserves no window, writes no record, reports no
// outcome, or emits no metric.
type Options struct {
	Keys        KeyLookup        // the Key by the SHA-256 of its value, resolved, cached (007)
	Catalog     Catalog          // Models and Providers by name, without credential values
	Credentials CredentialSource // a Provider's credential value, opened for this request (005)
	Router      Router           // target selection and the per-target circuits (008)
	Limiter     Limiter          // the windows: reserve before, settle after (007)
	Recorder    Recorder         // one record per request (009)
	Clients     ClientSource     // the upstream client per Provider, built by NewClientSource (005)
	Health      HealthObserver   // outcomes per Provider for passive health (005)
	// Metrics is the registry the three request metrics of spec 019 are
	// recorded in: lux_requests_total, lux_request_duration_seconds, and
	// lux_time_to_first_byte_seconds. Nil records none.
	Metrics *metrics.Registry
	// Version is the build's, written as the User-Agent luxd/<Version>
	// toward every provider. Empty is dev.
	Version string
	// MaxBodyBytes is LUX_MAX_BODY_BYTES: the largest request body a door
	// accepts and the cap on an upstream body read whole. Zero is 64Mi.
	MaxBodyBytes int64
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints a request id, req_ and a ULID; nil uses v1.NewID.
	NewID func() string
}

// KeyLookup is the Key on a door: the resolved Key whose value's SHA-256
// is hash, 64 lower-case hex characters. Spec 007's cache satisfies it
// from the store's hash index and the journal tail. A hash no Key has is
// nil and no error, which the door answers unauthenticated; an error is
// the store's own failure, answered store_unavailable.
type KeyLookup interface {
	ByHash(ctx context.Context, hash string) (*v1.Key, error)
}

// Catalog is desired state as the door reads it: a Model by its exact
// name, declared or discovered, every Model for the list a Key may see,
// and a Provider by name or prv_ id, without its credential value. A
// name that names nothing is nil and no error; an error is the store's
// own failure.
type Catalog interface {
	Model(ctx context.Context, name string) (*v1.Model, error)
	Models(ctx context.Context) ([]*v1.Model, error)
	Provider(ctx context.Context, nameOrID string) (*v1.Provider, error)
}

// Target is one entry of a Model's attempt order: the Provider, without
// its credential value, and the upstream's own name for the model,
// targets[].model.
type Target struct {
	Provider *v1.Provider
	Model    string
}

// Router is target selection and the circuit per target of spec 008.
// Targets is the attempt order for one request, computed from the
// Model's targets, each Provider's health, and each target's circuit
// through the breaker's side-effect-free Admits; empty is
// provider_unavailable. Allow takes the half-open probe slot immediately
// before an attempt and answers false when another request took it, in
// which case the target is skipped. RecordSuccess is every complete
// response the retry table does not retry; RecordFailure is every
// retryable failure; an encode refusal and a caller cancellation report
// neither.
type Router interface {
	Targets(ctx context.Context, m *v1.Model) ([]Target, error)
	Allow(t Target) bool
	RecordSuccess(t Target)
	RecordFailure(t Target)
}

// Reservation is what stage 7 of the pipeline asks the Limiter to admit:
// the Key, the Model the request resolved to, or nil with Opaque set on
// an opaque route, and the token reservation of spec 007: InputTokens is
// the estimate over the decoded request on a translation, or the body's
// length in bytes divided by four on a passthrough; OutputTokens is the
// requested maximum output, or 1024 when the request names none. A count
// and an opaque route reserve zero tokens and one request.
type Reservation struct {
	Key          *v1.Key
	Model        *v1.Model
	Opaque       bool
	InputTokens  int64
	OutputTokens int64
}

// Limiter is the windows of spec 007: the Key's rate windows, the
// Model's pricing against the Key's allowUnpriced, the Budget's
// currency, the Key's spend window, and the Budget's window, in that
// order. Reserve admits the request and returns the Lease its settle
// runs through, or a *Refusal carrying the code, the Retry-After, and
// the developer detail; any other error is the store's failure.
type Limiter interface {
	Reserve(ctx context.Context, r Reservation) (Lease, error)
}

// Lease is one admitted reservation. Settle replaces the reservation
// with the measured tokens once the response is finished, refused, or
// failed; zero measured tokens is a whole refund.
type Lease interface {
	Settle(ctx context.Context, t Tokens)
}

// Refusal is the Limiter's answer when a window is full or a price
// cannot be counted: one of rate_limited, model_unpriced,
// currency_mismatch, spend_exceeded, or budget_exhausted. RetryAfter is
// the time until the window resets, zero when there is no reset to
// name.
type Refusal struct {
	Code       Code
	RetryAfter time.Duration
	Detail     string
}

// Error renders the developer's line.
func (r *Refusal) Error() string {
	if r.Detail == "" {
		return string(r.Code)
	}
	return string(r.Code) + ": " + r.Detail
}

// Recorder takes the one Record of every request after its response is
// finished or refused. Spec 009 builds metering.Record from it, adding
// the cost the Model's pricing gives the tokens; the gateway computes no
// price. Record is called once per request and never blocks on a store.
type Recorder interface {
	Record(r Record)
}

// ClientSource is the upstream client per Provider, built under spec
// 005's rules: pinned to the Provider's base URL, no redirects, no
// proxy, no compression. The gateway dials through nothing else.
type ClientSource interface {
	Client(ctx context.Context, p *v1.Provider) (*http.Client, error)
}

// CredentialSource opens a Provider's credential value for one outbound
// request and nothing longer. An empty value is a Provider with no
// credential, toward which the gateway injects no header.
type CredentialSource interface {
	Credential(ctx context.Context, providerID string) ([]byte, error)
}

// HealthObserver takes each attempt's outcome for the passive health of
// spec 005: failed is a transport error, a timeout, or a 5xx; any other
// complete response is a success.
type HealthObserver interface {
	Observe(providerID string, failed bool)
}
