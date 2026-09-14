// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The event record of spec 012, as the jobs of this package write it to
// the journal: the envelope every event carries and the object block a
// sink keys on. Spec 012's own package takes this shape over when it
// lands; until then the two server-raised reasons below are the only
// writers.

// The reasons a server-raised event carries.
const (
	reasonDiscovery = "discovery"
	reasonProbe     = "probe"
)

// The event types this package raises.
const (
	eventProviderUnreachable = "provider.unreachable"
	eventProviderHealthy     = "provider.healthy"
	eventModelDiscovered     = "model.discovered"
	eventModelRemoved        = "model.removed"
)

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

// appendEvent journals one server-raised event about obj with data.
func appendEvent(ctx context.Context, j store.Journal, typ, reason string, obj v1.Object, data any, now time.Time, newID func() string) error {
	rec := eventRecord{
		ID:     newID(),
		Type:   typ,
		Time:   now.UTC(),
		Reason: reason,
		Object: eventObject{Kind: obj.Kind(), ID: obj.ID(), Name: obj.Name(), Owner: obj.Owner(), Labels: labelsOf(obj)},
		Data:   data,
	}
	if rec.Object.Labels == nil {
		rec.Object.Labels = map[string]string{}
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding the %s event of %s: %w", typ, obj.ID(), err)
	}
	if _, err := j.Append(ctx, store.Event{ID: rec.ID, ObjectID: obj.ID(), Type: typ, At: now, Payload: payload}); err != nil {
		return fmt.Errorf("journalling the %s event of %s: %w", typ, obj.ID(), err)
	}
	return nil
}

func labelsOf(obj v1.Object) map[string]string {
	switch x := obj.(type) {
	case *v1.Provider:
		return x.Metadata.Labels
	case *v1.Model:
		return x.Metadata.Labels
	}
	return nil
}
