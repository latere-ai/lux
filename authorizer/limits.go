// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"fmt"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Limits are what an allow granted the subject, decoded from the answer's
// limits object one wire name to one figure. Zero is the configured value
// or no limit: an absent field grants nothing and takes nothing away.
type Limits struct {
	// RequestsPerMinute overrides the control plane rate for this subject
	// (spec 011); 0 keeps the configured rate.
	RequestsPerMinute int
	// Key are the four ceilings a Key this subject applies is held to at
	// resolve, max_key_requests_per_minute, max_key_tokens_per_minute,
	// max_key_spend, and max_key_ttl (spec 003).
	Key manifest.Limits
	// MaxKeys caps the subject's live Keys, checked by the API at
	// key.create; 0 is no cap.
	MaxKeys int
}

// WireLimits is the limits object as an answer carries it, exported so
// an authorizer renders its answer through the type luxd decodes rather
// than through six string literals. Every member is optional; a member
// the object does not name stays nil, and omitempty leaves it out, so an
// authorizer that sets two ceilings sends two and the other four stay
// absent, which grants nothing and takes nothing away. Five are
// pointers, so a member deliberately set to zero is still sent and
// decodes to zero, the same grant as absence; MaxKeyTTL is a duration
// string, whose absent case is the empty string. A money value is a
// string, as a manifest writes one, and a duration is Go syntax.
type WireLimits struct {
	RequestsPerMinute       *int        `json:"requests_per_minute,omitempty"`
	MaxKeyRequestsPerMinute *int        `json:"max_key_requests_per_minute,omitempty"`
	MaxKeyTokensPerMinute   *int        `json:"max_key_tokens_per_minute,omitempty"`
	MaxKeySpend             *v1.Money   `json:"max_key_spend,omitempty"`
	MaxKeyTTL               v1.Duration `json:"max_key_ttl,omitempty"`
	MaxKeys                 *int        `json:"max_keys,omitempty"`
}

// DecodeLimits reads a decision's limits object. A decision with no
// limits is the zero Limits. An object that does not parse, a negative
// figure, a spend that is not a money string, or a ttl that is not a
// duration is an error, and the caller treats the answer as no decision:
// a ceiling the gateway cannot read is not a ceiling it can hold.
func DecodeLimits(d authz.Decision) (Limits, error) {
	var w WireLimits
	if err := d.DecodeLimits(&w); err != nil {
		return Limits{}, err
	}
	var l Limits
	for _, f := range []struct {
		name string
		src  *int
		dst  *int
	}{
		{"requests_per_minute", w.RequestsPerMinute, &l.RequestsPerMinute},
		{"max_key_requests_per_minute", w.MaxKeyRequestsPerMinute, &l.Key.MaxRequestsPerMinute},
		{"max_key_tokens_per_minute", w.MaxKeyTokensPerMinute, &l.Key.MaxTokensPerMinute},
		{"max_keys", w.MaxKeys, &l.MaxKeys},
	} {
		if f.src == nil {
			continue
		}
		if *f.src < 0 {
			return Limits{}, fmt.Errorf("limits.%s is %d, below zero", f.name, *f.src)
		}
		*f.dst = *f.src
	}
	if w.MaxKeySpend != nil {
		// A money string carries no sign, so the figure is never below zero.
		l.Key.MaxSpend = *w.MaxKeySpend
	}
	if w.MaxKeyTTL != "" {
		ttl, err := w.MaxKeyTTL.Parse()
		if err != nil {
			return Limits{}, fmt.Errorf("limits.max_key_ttl: %w", err)
		}
		if ttl < 0 {
			return Limits{}, fmt.Errorf("limits.max_key_ttl is %s, below zero", w.MaxKeyTTL)
		}
		l.Key.MaxTTL = ttl
	}
	return l, nil
}
