// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"time"
)

// Key is the credential a workload holds: which Models it may name, its
// rate and spend limits, the Budget it draws from, and when it expires.
type Key struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     KeySpec    `json:"spec"`
	Status   KeyStatus  `json:"status"`
}

// Kind implements Object.
func (k *Key) Kind() string { return KindKey }

// ID implements Object.
func (k *Key) ID() string { return k.Status.ID }

// Owner implements Object.
func (k *Key) Owner() string { return k.Status.Owner }

// Name implements Object.
func (k *Key) Name() string { return k.Metadata.Name }

// MarshalJSON writes the envelope before the object.
func (k Key) MarshalJSON() ([]byte, error) {
	type plain Key
	return json.Marshal(struct {
		envelope
		plain
	}{envelope{APIVersion, KindKey}, plain(k)})
}

// KeySpec is the desired state of a Key. The value a caller supplies
// instead of a minted one is write-only, an unexported member the
// encoders skip, reached through Value.
type KeySpec struct {
	Models        []string   `json:"models,omitempty"`
	value         writeOnly  `writeonly:"value"`
	ValueFrom     *ValueFrom `json:"valueFrom,omitempty"`
	Limits        KeyLimits  `json:"limits,omitzero"`
	Budget        string     `json:"budget,omitempty"`
	TTL           Duration   `json:"ttl,omitempty"`
	ExpiresAt     time.Time  `json:"expiresAt,omitzero"`
	AllowUnpriced bool       `json:"allowUnpriced"`
	Passthrough   bool       `json:"passthrough"`
	Disabled      bool       `json:"disabled"`
}

// Value returns the supplied value the manifest carried and whether it
// carried one. The API reads it once, hashes it, and takes it out.
func (s *KeySpec) Value() (string, bool) { return s.value.get() }

// SetValue records a value the decoder read, or one an importer supplies.
func (s *KeySpec) SetValue(v string) { s.value.put(v) }

// ClearValue drops the value, which the API does after hashing it.
func (s *KeySpec) ClearValue() { s.value.clear() }

// KeyLimits are the Key's own limits. The two rates are pointers because an
// explicit 0, no limit, differs from an absent rate, which takes the
// operator's default.
type KeyLimits struct {
	RequestsPerMinute *int   `json:"requestsPerMinute,omitempty"`
	TokensPerMinute   *int   `json:"tokensPerMinute,omitempty"`
	Spend             *Spend `json:"spend,omitempty"`
}

// Spend is a spend limit on the Key itself: an amount per window.
type Spend struct {
	Amount   *Money `json:"amount,omitempty"`
	Currency string `json:"currency,omitempty"`
	Window   Window `json:"window,omitempty"`
}

// KeyStatus is written by the server and ignored on apply. Resolve fills
// Selectors, ExpiresAt, and Warnings; the API fills the rest.
type KeyStatus struct {
	ID         string           `json:"id,omitempty"`
	Version    int64            `json:"version,omitempty"`
	Owner      string           `json:"owner,omitempty"`
	Prefix     string           `json:"prefix,omitempty"`
	Value      string           `json:"value,omitempty"`
	State      KeyState         `json:"state,omitempty"`
	Selectors  []SelectorStatus `json:"selectors,omitempty"`
	Budget     *BudgetRef       `json:"budget,omitempty"`
	Usage      *KeyUsage        `json:"usage,omitempty"`
	LastUsedAt time.Time        `json:"lastUsedAt,omitzero"`
	ExpiresAt  time.Time        `json:"expiresAt,omitzero"`
	CreatedAt  time.Time        `json:"createdAt,omitzero"`
	UpdatedAt  time.Time        `json:"updatedAt,omitzero"`
	Warnings   []string         `json:"warnings"`
}

// SelectorStatus is what one selector matched at resolve. Matched is
// never nil, so a selector that matched nothing reads as an empty list.
type SelectorStatus struct {
	Selector string   `json:"selector"`
	Matched  []string `json:"matched"`
}

// BudgetRef names the Budget the Key draws from.
type BudgetRef struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// KeyUsage is the Key's spending, rendered at read time.
type KeyUsage struct {
	Window *UsageWindow `json:"window,omitempty"`
	Total  UsageTotal   `json:"total"`
}

// UsageWindow is the current spend window's counters.
type UsageWindow struct {
	Requests int64     `json:"requests"`
	Tokens   int64     `json:"tokens"`
	Spend    Money     `json:"spend"`
	ResetsAt time.Time `json:"resetsAt,omitzero"`
}

// UsageTotal is the Key's lifetime counters.
type UsageTotal struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
	Spend    Money `json:"spend"`
}

// KeyState is the Key's rendered state.
type KeyState string

// The key states.
const (
	KeyActive    KeyState = "Active"
	KeyDisabled  KeyState = "Disabled"
	KeyExpired   KeyState = "Expired"
	KeyExhausted KeyState = "Exhausted"
)

// Valid reports whether s is one of the states.
func (s KeyState) Valid() bool {
	switch s {
	case KeyActive, KeyDisabled, KeyExpired, KeyExhausted:
		return true
	default:
		return false
	}
}
