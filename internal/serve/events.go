// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"time"

	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The event record is internal/events's, spec 012's package. The names
// here are the ones the jobs of this package and the API of spec 011
// were written against, kept so every writer reads as it did; each is
// the events package's own.

// ReasonRequest is the reason of a mutation through the API.
const ReasonRequest = events.ReasonRequest

// The three reasons the server raises on its own.
const (
	reasonDiscovery = events.ReasonDiscovery
	reasonProbe     = events.ReasonProbe
	reasonLimit     = events.ReasonLimit
)

// The event types this package raises.
const (
	eventProviderUnreachable = events.ProviderUnreachable
	eventProviderHealthy     = events.ProviderHealthy
	eventModelDiscovered     = events.ModelDiscovered
	eventModelRemoved        = events.ModelRemoved
	eventKeyExhausted        = events.KeyExhausted
	eventBudgetExhausted     = events.BudgetExhausted
)

// Event is one event to journal, events.Event.
type Event = events.Event

// eventRecord is the body a sink receives, events.Record.
type eventRecord = events.Record

// AppendEvent journals e through events.Append.
func AppendEvent(ctx context.Context, j store.Journal, e Event) error {
	return events.Append(ctx, j, e)
}

// appendEvent journals one server-raised event about obj with data.
func appendEvent(ctx context.Context, j store.Journal, typ, reason string, obj v1.Object, data any, now time.Time, newID func() string) error {
	return events.Append(ctx, j, Event{ID: newID(), Type: typ, At: now, Reason: reason, Object: obj, Data: data})
}
