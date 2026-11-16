// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle holds the lifecycles Lux stores or keeps in memory, each
// as a statemachine table. Every writer of a lifecycle consults its table: a
// write that would take a record along a step the table lacks changes
// nothing and is refused with an error that wraps
// statemachine.ErrIllegalTransition. The tests drive the real writers
// through each table, and formal/ renders the tables into the Lean and TLA+
// models that the lean and tla gates check.
package lifecycle

import (
	"latere.ai/x/pkg/statemachine"

	v1 "latere.ai/x/lux/manifest/v1"
)

// ObjectState is the state of a stored object row.
type ObjectState string

// The states of an object row. Pruned is not a stored value: pruning removes
// the row, so the state is the absence of the row.
const (
	ObjectLive    ObjectState = "live"
	ObjectDeleted ObjectState = "deleted"
	ObjectPruned  ObjectState = "pruned"
)

// ObjectTable is the lifecycle of a row of the objects table, derived from
// objects.deleted_at. Put creates a live row; Objects.Delete is the one
// writer of deleted_at and moves a live row to deleted, which frees its name
// at once and never frees its id; Objects.Prune removes deleted rows whose
// journal rows are all settled. Nothing makes a deleted row live again: a
// create over a used id is a version conflict.
var ObjectTable = statemachine.Must(statemachine.New("Object", []ObjectState{ObjectLive}, map[ObjectState][]ObjectState{
	ObjectLive:    {ObjectDeleted},
	ObjectDeleted: {ObjectPruned},
	ObjectPruned:  nil,
}))

// EventState is the state of a journal row.
type EventState string

// The states of a journal row. Removed is not a stored value: Drop and
// Prune delete the row, so the state is the absence of the row.
const (
	EventPending      EventState = "pending"
	EventAcknowledged EventState = "acknowledged"
	EventRemoved      EventState = "removed"
)

// EventTable is the lifecycle of a row of the journal, derived from
// journal.acked_at. Append creates a pending row; Journal.Acknowledge is the
// one writer of acked_at and moves a pending row to acknowledged; Journal.Drop
// removes a row in either state, which is how a row that failed for the whole
// retry window leaves; Journal.Prune removes acknowledged rows past their
// age. Defer changes a pending row's attempt count and due time and is no
// step.
var EventTable = statemachine.Must(statemachine.New("Event", []EventState{EventPending}, map[EventState][]EventState{
	EventPending:      {EventAcknowledged, EventRemoved},
	EventAcknowledged: {EventRemoved},
	EventRemoved:      nil,
}))

// HealthPhase is a Provider's published reachability in the lower-case
// spelling a table state takes. The wire value is v1.HealthState, which
// keeps its capitals; HealthPhaseOf and HealthStateOf convert.
type HealthPhase string

// The phases of a Provider's health, one for each v1.HealthState.
const (
	HealthUnknown     HealthPhase = "unknown"
	HealthHealthy     HealthPhase = "healthy"
	HealthDegraded    HealthPhase = "degraded"
	HealthUnreachable HealthPhase = "unreachable"
)

// HealthTable is the lifecycle of a Provider's health as the health job
// holds it in memory and publishes it as status.health.state. The writers are
// the machine's step (a probe or data-plane outcome: a failure moves unknown
// or healthy to degraded and the third consecutive failure moves degraded to
// unreachable, a success moves any state to healthy), its force (the tunnel
// registry's verdict, unreachable from any state), and the reset the job
// writes once when a Provider's mode becomes none (unknown from any other
// state). A failure never moves unreachable back to degraded.
var HealthTable = statemachine.Must(statemachine.New("Health", []HealthPhase{HealthUnknown}, map[HealthPhase][]HealthPhase{
	HealthUnknown:     {HealthHealthy, HealthDegraded, HealthUnreachable},
	HealthHealthy:     {HealthDegraded, HealthUnreachable, HealthUnknown},
	HealthDegraded:    {HealthHealthy, HealthUnreachable, HealthUnknown},
	HealthUnreachable: {HealthHealthy, HealthUnknown},
}))

// HealthPhaseOf is the phase of a published state. A state that was never
// written, the empty one, is unknown.
func HealthPhaseOf(s v1.HealthState) HealthPhase {
	switch s {
	case v1.HealthHealthy:
		return HealthHealthy
	case v1.HealthDegraded:
		return HealthDegraded
	case v1.HealthUnreachable:
		return HealthUnreachable
	case v1.HealthUnknown:
		return HealthUnknown
	default:
		return HealthUnknown
	}
}

// HealthStateOf is the published state of a phase.
func HealthStateOf(p HealthPhase) v1.HealthState {
	switch p {
	case HealthHealthy:
		return v1.HealthHealthy
	case HealthDegraded:
		return v1.HealthDegraded
	case HealthUnreachable:
		return v1.HealthUnreachable
	case HealthUnknown:
		return v1.HealthUnknown
	default:
		return v1.HealthUnknown
	}
}

// TunnelPhase is whether a tunneled Provider has a live session, in the
// lower-case spelling a table state takes; v1.TunnelState is the wire value.
type TunnelPhase string

// The phases of a Provider's tunnel.
const (
	TunnelConnected    TunnelPhase = "connected"
	TunnelDisconnected TunnelPhase = "disconnected"
)

// TunnelTable is the lifecycle of status.tunnel.state. The health lease
// holder is its one writer: it reads the tunnel registry each tick and
// writes connected while a live row exists and disconnected while none does.
// A Provider starts in either, by whether its first tick finds a row, and
// each moves to the other.
var TunnelTable = statemachine.Must(statemachine.New("Tunnel", []TunnelPhase{TunnelConnected, TunnelDisconnected}, map[TunnelPhase][]TunnelPhase{
	TunnelConnected:    {TunnelDisconnected},
	TunnelDisconnected: {TunnelConnected},
}))

// TunnelPhaseOf is the phase of a published tunnel state.
func TunnelPhaseOf(s v1.TunnelState) TunnelPhase {
	switch s {
	case v1.TunnelConnected:
		return TunnelConnected
	case v1.TunnelDisconnected:
		return TunnelDisconnected
	default:
		return TunnelDisconnected
	}
}

// SessionState is the state of a tunnel session on the replica that accepted
// it.
type SessionState string

// The states of a tunnel session.
const (
	SessionLive   SessionState = "live"
	SessionClosed SessionState = "closed"
)

// SessionTable is the lifecycle of one accepted tunnel session. A session
// starts live and its close is the one writer of the state, reached from the
// agent's stream ending, the bearer's expiry, a superseding session, the
// Provider's deletion, and the replica's drain; whichever comes first closes
// it, and later ones find it closed.
var SessionTable = statemachine.Must(statemachine.New("Session", []SessionState{SessionLive}, map[SessionState][]SessionState{
	SessionLive:   {SessionClosed},
	SessionClosed: nil,
}))
