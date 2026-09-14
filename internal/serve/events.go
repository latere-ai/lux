// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The event record of spec 012, as the jobs of this package and the API
// of spec 011 write it to the journal: the envelope every event carries
// and the object block a sink keys on. Spec 012's own package takes this
// shape over when it lands; until then the reasons below are the only
// writers.

// The reasons an event carries: request for a mutation through the API,
// and the three the server raises on its own.
const (
	ReasonRequest   = "request"
	reasonDiscovery = "discovery"
	reasonProbe     = "probe"
	reasonLimit     = "limit"
)

// The event types this package raises.
const (
	eventProviderUnreachable = "provider.unreachable"
	eventProviderHealthy     = "provider.healthy"
	eventModelDiscovered     = "model.discovered"
	eventModelRemoved        = "model.removed"
	eventKeyExhausted        = "key.exhausted"
	eventBudgetExhausted     = "budget.exhausted"
)

// Event is one event to journal: the type of spec 012's table, the
// clock, the subject that caused it and the request it came in on, both
// empty for one the server raised, the reason, the object it names, and
// the data of the type's row. ID is minted when empty.
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

type eventRecord struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Time      time.Time   `json:"time"`
	Subject   string      `json:"subject"`
	Reason    string      `json:"reason"`
	RequestID string      `json:"request_id"`
	Object    eventObject `json:"object"`
	Data      any         `json:"data"`
}

type eventObject struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Owner  string            `json:"owner"`
	Labels map[string]string `json:"labels"`
}

// AppendEvent journals e: the payload is the record exactly as a sink
// receives it, so a retry and a replay send the same bytes. An event
// with no object or no type is a plain error, because a row a sink
// cannot key on is a bug in the caller. Inside a Transact the journal
// is the transaction's, so the row commits with the object it names.
func AppendEvent(ctx context.Context, j store.Journal, e Event) error {
	if e.Object == nil || e.Type == "" {
		return errors.New("event: no object or no type")
	}
	if e.ID == "" {
		e.ID = v1.NewID(v1.PrefixEvent, e.At, nil)
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	rec := eventRecord{
		ID: e.ID, Type: e.Type, Time: e.At.UTC(), Subject: e.Subject, Reason: e.Reason, RequestID: e.RequestID,
		Object: eventObject{Kind: e.Object.Kind(), ID: e.Object.ID(), Name: e.Object.Name(), Owner: e.Object.Owner(), Labels: labelsOf(e.Object)},
		Data:   e.Data,
	}
	if rec.Object.Labels == nil {
		rec.Object.Labels = map[string]string{}
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding the %s event of %s: %w", e.Type, e.Object.ID(), err)
	}
	if _, err := j.Append(ctx, store.Event{ID: rec.ID, ObjectID: e.Object.ID(), Type: e.Type, At: e.At, Payload: payload}); err != nil {
		return fmt.Errorf("journalling the %s event of %s: %w", e.Type, e.Object.ID(), err)
	}
	return nil
}

// appendEvent journals one server-raised event about obj with data.
func appendEvent(ctx context.Context, j store.Journal, typ, reason string, obj v1.Object, data any, now time.Time, newID func() string) error {
	return AppendEvent(ctx, j, Event{ID: newID(), Type: typ, At: now, Reason: reason, Object: obj, Data: data})
}

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
