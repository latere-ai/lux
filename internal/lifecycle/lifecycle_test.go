// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"flag"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/statemachine/statemachinetest"

	v1 "latere.ai/x/lux/manifest/v1"
)

var update = flag.Bool("update", false, "rewrite the formal models under formal/")

// TestFormalModels holds formal/, which the lean and tla gates check, to the
// lifecycle tables. Rerun with -update after changing one.
func TestFormalModels(t *testing.T) {
	statemachinetest.Models(t, "../../formal", *update,
		ObjectTable.Graph(), EventTable.Graph(), HealthTable.Graph(),
		TunnelTable.Graph(), SessionTable.Graph())
}

// TestEveryStatusIsInItsTable keeps each status type and its table to one
// set of states, so a state added to one is added to the other.
func TestEveryStatusIsInItsTable(t *testing.T) {
	check(t, "Object", ObjectTable.States(), ObjectLive, ObjectDeleted, ObjectPruned)
	check(t, "Event", EventTable.States(), EventPending, EventAcknowledged, EventRemoved)
	check(t, "Health", HealthTable.States(), HealthUnknown, HealthHealthy, HealthDegraded, HealthUnreachable)
	check(t, "Tunnel", TunnelTable.States(), TunnelConnected, TunnelDisconnected)
	check(t, "Session", SessionTable.States(), SessionLive, SessionClosed)
}

func check[S ~string](t *testing.T, name string, states []S, consts ...S) {
	t.Helper()
	want := slices.Sorted(slices.Values(consts))
	if !slices.Equal(states, want) {
		t.Errorf("%s table states %v, constants %v", name, states, want)
	}
}

// TestPhasesMatchTheWireStates: each v1 state converts to a phase of its
// table and back, and the state a record never had is unknown.
func TestPhasesMatchTheWireStates(t *testing.T) {
	for _, s := range []v1.HealthState{v1.HealthUnknown, v1.HealthHealthy, v1.HealthDegraded, v1.HealthUnreachable} {
		p := HealthPhaseOf(s)
		if !slices.Contains(HealthTable.States(), p) || HealthStateOf(p) != s {
			t.Errorf("health state %q: phase %q, back %q", s, p, HealthStateOf(p))
		}
	}
	if HealthPhaseOf("") != HealthUnknown {
		t.Errorf("the empty health state is phase %q, want unknown", HealthPhaseOf(""))
	}
	if HealthPhaseOf("Bogus") != HealthUnknown || HealthStateOf("bogus") != v1.HealthUnknown || TunnelPhaseOf("Bogus") != TunnelDisconnected {
		t.Error("a value no state names must read as unknown, and an unknown tunnel as disconnected")
	}
	for _, s := range []v1.TunnelState{v1.TunnelConnected, v1.TunnelDisconnected} {
		if p := TunnelPhaseOf(s); !slices.Contains(TunnelTable.States(), p) || string(p) != strings.ToLower(string(s)) {
			t.Errorf("tunnel state %q: phase %q", s, p)
		}
	}
}
