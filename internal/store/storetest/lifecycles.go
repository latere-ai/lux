// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lux/internal/lifecycle"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

var lifecycleSeq atomic.Int64

// noWriter is the refusal for a state no writer moves a record to.
func noWriter(what string) error {
	return fmt.Errorf("%s: no writer takes a record there: %w", what, statemachine.ErrIllegalTransition)
}

// errRollback ends a probe's transaction without committing it.
var errRollback = errors.New("storetest: roll the probe back")

// objectLifecycle drives the writers of an object row through ObjectTable:
// Delete moves a live row to deleted, and Prune moves a deleted row to
// pruned. Every Enter first prunes what earlier records left behind, so the
// count Prune returns is the count of the record under test.
func objectLifecycle(t *testing.T, s store.Store) {
	ctx := t.Context()
	// idUsed reports whether the store still holds the id, which is how a
	// deleted row differs from a pruned one: neither is readable, and a
	// create over the id of the first is a conflict. The probe's create
	// rolls back.
	idUsed := func(t *testing.T, id string) bool {
		probe := provider(fmt.Sprintf("probe-%d", lifecycleSeq.Add(1)))
		probe.Status.ID = id
		err := s.Transact(ctx, func(tx store.Store) error {
			if _, err := tx.Objects().Put(ctx, probe, 0); err != nil {
				return err
			}
			return errRollback
		})
		switch {
		case errors.Is(err, errRollback):
			return false
		case errors.Is(err, store.ErrVersionConflict):
			return true
		default:
			t.Fatalf("probe the id %s: %v", id, err)
			return false
		}
	}
	d := statemachinetest.Driver[lifecycle.ObjectState]{
		Enter: func(t *testing.T, state lifecycle.ObjectState) string {
			_, err := s.Objects().Prune(ctx, time.Now().Add(time.Hour))
			noErr(t, err, "prune before a record")
			p := provider(fmt.Sprintf("object-%d", lifecycleSeq.Add(1)))
			_, err = s.Objects().Put(ctx, p, 0)
			noErr(t, err, "create")
			if state == lifecycle.ObjectLive {
				return p.Status.ID
			}
			noErr(t, s.Objects().Delete(ctx, v1.KindProvider, p.Status.ID), "delete")
			if state == lifecycle.ObjectPruned {
				n, err := s.Objects().Prune(ctx, time.Now().Add(time.Hour))
				noErr(t, err, "prune")
				equal(t, n, 1, "rows pruned")
			}
			return p.Status.ID
		},
		Move: func(id string, to lifecycle.ObjectState) error {
			switch to {
			case lifecycle.ObjectDeleted:
				return s.Objects().Delete(ctx, v1.KindProvider, id)
			case lifecycle.ObjectPruned:
				n, err := s.Objects().Prune(ctx, time.Now().Add(time.Hour))
				if err != nil {
					return err
				}
				if n == 0 {
					return fmt.Errorf("prune %s: the row was not deleted: %w", id, statemachine.ErrIllegalTransition)
				}
				return nil
			case lifecycle.ObjectLive:
				return noWriter("object live")
			}
			return noWriter("object " + string(to))
		},
		State: func(t *testing.T, id string) lifecycle.ObjectState {
			if _, _, err := s.Objects().Get(ctx, v1.KindProvider, id); err == nil {
				return lifecycle.ObjectLive
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("read %s: %v", id, err)
			}
			if idUsed(t, id) {
				return lifecycle.ObjectDeleted
			}
			return lifecycle.ObjectPruned
		},
	}
	statemachinetest.Exhaustive(t, lifecycle.ObjectTable, d)
	statemachinetest.Race(t, lifecycle.ObjectTable, d)
}

// eventLifecycle drives the writers of a journal row through EventTable:
// Acknowledge moves a pending row to acknowledged, and Drop removes a row
// in either state. A writer that finds no row answers ErrNotFound, which
// for a removed row is the refusal.
func eventLifecycle(t *testing.T, s store.Store) {
	ctx := t.Context()
	objectOf := func(id string) string { return "obj_" + id }
	d := statemachinetest.Driver[lifecycle.EventState]{
		Enter: func(t *testing.T, state lifecycle.EventState) string {
			id := fmt.Sprintf("evt_lc%d", lifecycleSeq.Add(1))
			_, err := s.Journal().Append(ctx, store.Event{ID: id, ObjectID: objectOf(id), Type: "key.updated"})
			noErr(t, err, "append")
			switch state {
			case lifecycle.EventAcknowledged:
				noErr(t, s.Journal().Acknowledge(ctx, id), "acknowledge")
			case lifecycle.EventRemoved:
				noErr(t, s.Journal().Drop(ctx, id), "drop")
			case lifecycle.EventPending:
			}
			return id
		},
		Move: func(id string, to lifecycle.EventState) error {
			var err error
			switch to {
			case lifecycle.EventAcknowledged:
				err = s.Journal().Acknowledge(ctx, id)
			case lifecycle.EventRemoved:
				err = s.Journal().Drop(ctx, id)
			case lifecycle.EventPending:
				return noWriter("event pending")
			}
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("event %s is removed: %w", id, statemachine.ErrIllegalTransition)
			}
			return err
		},
		State: func(t *testing.T, id string) lifecycle.EventState {
			rows, _, err := s.Journal().ByObject(ctx, objectOf(id), store.Page{})
			noErr(t, err, "read the row")
			switch {
			case len(rows) == 0:
				return lifecycle.EventRemoved
			case rows[0].AckedAt.IsZero():
				return lifecycle.EventPending
			default:
				return lifecycle.EventAcknowledged
			}
		},
	}
	statemachinetest.Exhaustive(t, lifecycle.EventTable, d)
	statemachinetest.Race(t, lifecycle.EventTable, d)

	// An acknowledged row keeps the time it was acknowledged at.
	id := fmt.Sprintf("evt_lc%d", lifecycleSeq.Add(1))
	_, err := s.Journal().Append(ctx, store.Event{ID: id, ObjectID: objectOf(id), Type: "key.updated"})
	noErr(t, err, "append")
	noErr(t, s.Journal().Acknowledge(ctx, id), "acknowledge")
	rows, _, err := s.Journal().ByObject(ctx, objectOf(id), store.Page{})
	noErr(t, err, "read the acknowledged row")
	first := rows[0].AckedAt
	time.Sleep(5 * time.Millisecond)
	wantErr(t, s.Journal().Acknowledge(ctx, id), statemachine.ErrIllegalTransition, "acknowledge twice")
	rows, _, err = s.Journal().ByObject(ctx, objectOf(id), store.Page{})
	noErr(t, err, "read the row again")
	truth(t, rows[0].AckedAt.Equal(first), "a second acknowledgment leaves AckedAt as it was")
}

// healthLastErrorLeavesState holds PutStatus to the one partial write of a
// health member: with no State it replaces lastError and leaves the state
// and times the health job wrote.
func healthLastErrorLeavesState(t *testing.T, s store.Store) {
	ctx := t.Context()
	p := provider("health-error")
	_, err := s.Objects().Put(ctx, p, 0)
	noErr(t, err, "create")
	read := func() *v1.HealthStatus {
		obj, _, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
		noErr(t, err, "read")
		return as[*v1.Provider](t, obj).Status.Health
	}
	since := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	noErr(t, s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{LastError: "model list: refused"}}), "error before any health")
	h := read()
	truth(t, h != nil && h.State == v1.HealthUnknown && h.LastError == "model list: refused", "a first lastError starts the member Unknown")

	noErr(t, s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthUnreachable, Since: since, LastError: "probe failed"}}), "health job")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{LastError: "model list: timeout"}}), "error after health")
	h = read()
	truth(t, h != nil && h.State == v1.HealthUnreachable && h.Since.Equal(since) && h.LastError == "model list: timeout", "lastError replaced, state and since kept")

	noErr(t, s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy}}), "health job recovers")
	h = read()
	truth(t, h != nil && h.State == v1.HealthHealthy && h.LastError == "", "a write with a state replaces the member")
}
