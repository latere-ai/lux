// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"time"
)

// Budget is a spend window several Keys draw from.
type Budget struct {
	Metadata ObjectMeta   `json:"metadata"`
	Spec     BudgetSpec   `json:"spec"`
	Status   BudgetStatus `json:"status"`
}

// Kind implements Object.
func (b *Budget) Kind() string { return KindBudget }

// ID implements Object.
func (b *Budget) ID() string { return b.Status.ID }

// Owner implements Object.
func (b *Budget) Owner() string { return b.Status.Owner }

// Name implements Object.
func (b *Budget) Name() string { return b.Metadata.Name }

// MarshalJSON writes the envelope before the object.
func (b Budget) MarshalJSON() ([]byte, error) {
	type plain Budget
	return json.Marshal(struct {
		envelope
		plain
	}{envelope{APIVersion, KindBudget}, plain(b)})
}

// BudgetSpec is the desired state of a Budget. Hard is a pointer because an
// explicit false differs from an absent field, which defaults to true.
//
// Anchor is the instant a duration or month window is aligned to in
// place of the epoch or the first of the month, immutable like the
// window. RestartedAt, when it lies inside the current window and not
// after now, is where that window starts instead, so an operator can
// restart a window without waiting for its reset.
type BudgetSpec struct {
	Amount      *Money    `json:"amount,omitempty"`
	Currency    string    `json:"currency,omitempty"`
	Window      Window    `json:"window,omitempty"`
	Anchor      time.Time `json:"anchor,omitzero"`
	RestartedAt time.Time `json:"restartedAt,omitzero"`
	Hard        *bool     `json:"hard,omitempty"`
}

// BudgetStatus is written by the server and ignored on apply. Resolve
// fills Warnings; the API renders the rest at read time.
type BudgetStatus struct {
	ID        string      `json:"id,omitempty"`
	Version   int64       `json:"version,omitempty"`
	Owner     string      `json:"owner,omitempty"`
	State     BudgetState `json:"state,omitempty"`
	Spent     *Money      `json:"spent,omitempty"`
	Remaining *Money      `json:"remaining,omitempty"`
	ResetsAt  time.Time   `json:"resetsAt,omitzero"`
	Keys      *int        `json:"keys,omitempty"`
	CreatedAt time.Time   `json:"createdAt,omitzero"`
	UpdatedAt time.Time   `json:"updatedAt,omitzero"`
	Warnings  []string    `json:"warnings"`
}

// BudgetState is the Budget's rendered state.
type BudgetState string

// The budget states.
const (
	BudgetOpen      BudgetState = "Open"
	BudgetExhausted BudgetState = "Exhausted"
)

// Valid reports whether s is one of the states.
func (s BudgetState) Valid() bool {
	switch s {
	case BudgetOpen, BudgetExhausted:
		return true
	default:
		return false
	}
}
