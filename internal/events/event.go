// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The reasons an event carries: which source raised it.
const (
	// ReasonRequest is a mutation through the API, with the caller's
	// subject and the request's id.
	ReasonRequest = "request"
	// ReasonDiscovery is the discovery job declaring or removing a Model.
	ReasonDiscovery = "discovery"
	// ReasonProbe is the health job observing a Provider's transition.
	ReasonProbe = "probe"
	// ReasonLimit is a spend window reaching its amount.
	ReasonLimit = "limit"
	// ReasonCheck is luxd check verifying the sink; the one reason that
	// is never journalled.
	ReasonCheck = "check"
)

// The event types, one constant per row of the table.
const (
	ProviderCreated     = "provider.created"
	ProviderUpdated     = "provider.updated"
	ProviderDeleted     = "provider.deleted"
	ProviderUnreachable = "provider.unreachable"
	ProviderHealthy     = "provider.healthy"
	ModelCreated        = "model.created"
	ModelUpdated        = "model.updated"
	ModelDeleted        = "model.deleted"
	ModelDiscovered     = "model.discovered"
	ModelRemoved        = "model.removed"
	KeyCreated          = "key.created"
	KeyUpdated          = "key.updated"
	KeyRotated          = "key.rotated"
	KeyDeleted          = "key.deleted"
	KeyExhausted        = "key.exhausted"
	BudgetCreated       = "budget.created"
	BudgetUpdated       = "budget.updated"
	BudgetDeleted       = "budget.deleted"
	BudgetExhausted     = "budget.exhausted"
	CheckPing           = "check.ping"
)

// Event is one event to journal: the type of the table, the clock, the
// subject that caused it and the request it came in on, both empty for
// one the server raised, the reason, the object it names, and the data
// of the type's row. ID is minted when empty.
type Event struct {
	ID        string
	Type      string
	At        time.Time
	Subject   string
	Reason    string
	RequestID string
	Object    v1.Object
	Data      any
}

// Record is the body a sink receives, the JSON of spec 012's example:
// the envelope, the object block, and the row's data.
type Record struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Time      time.Time `json:"time"`
	Subject   string    `json:"subject"`
	Reason    string    `json:"reason"`
	RequestID string    `json:"request_id"`
	Object    ObjectRef `json:"object"`
	Data      any       `json:"data"`
}

// ObjectRef is the object block: the kind, the id, the name, the owner,
// and the labels, and nothing else, so a sink keys on it without
// parsing data. Labels is never null.
type ObjectRef struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Owner  string            `json:"owner"`
	Labels map[string]string `json:"labels"`
}

// Append journals e: the payload is the record exactly as a sink
// receives it, so a retry and a replay send the same bytes. An event
// with no object or no type is a plain error, because a row a sink
// cannot key on is a bug in the caller. Inside a Transact the journal
// is the transaction's, so the row commits with the object it names.
func Append(ctx context.Context, j store.Journal, e Event) error {
	if e.Object == nil || e.Type == "" {
		return errors.New("event: no object or no type")
	}
	if e.ID == "" {
		e.ID = v1.NewID(v1.PrefixEvent, e.At, nil)
	}
	rec := Record{
		ID: e.ID, Type: e.Type, Time: e.At.UTC(), Subject: e.Subject, Reason: e.Reason, RequestID: e.RequestID,
		Object: ObjectRef{Kind: e.Object.Kind(), ID: e.Object.ID(), Name: e.Object.Name(), Owner: e.Object.Owner(), Labels: labelsOf(e.Object)},
		Data:   e.Data,
	}
	payload, err := encode(rec)
	if err != nil {
		return fmt.Errorf("encoding the %s event of %s: %w", e.Type, e.Object.ID(), err)
	}
	if _, err := j.Append(ctx, store.Event{ID: rec.ID, ObjectID: e.Object.ID(), Type: e.Type, At: e.At, Payload: payload}); err != nil {
		return fmt.Errorf("journalling the %s event of %s: %w", e.Type, e.Object.ID(), err)
	}
	return nil
}

// encode renders the record as the body a sink receives, with data and
// labels never null.
func encode(rec Record) ([]byte, error) {
	if rec.Data == nil {
		rec.Data = map[string]any{}
	}
	if rec.Object.Labels == nil {
		rec.Object.Labels = map[string]string{}
	}
	return json.Marshal(rec)
}

// labelsOf is the object's metadata labels, for the four kinds.
func labelsOf(obj v1.Object) map[string]string {
	switch x := obj.(type) {
	case *v1.Provider:
		return x.Metadata.Labels
	case *v1.Model:
		return x.Metadata.Labels
	case *v1.Key:
		return x.Metadata.Labels
	case *v1.Budget:
		return x.Metadata.Labels
	}
	return nil
}
