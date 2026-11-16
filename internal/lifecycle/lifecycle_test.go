// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"flag"
	"slices"
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
	check(t, "Health", HealthTable.States(), v1.HealthUnknown, v1.HealthHealthy, v1.HealthDegraded, v1.HealthUnreachable)
	check(t, "Tunnel", TunnelTable.States(), v1.TunnelConnected, v1.TunnelDisconnected)
	check(t, "Session", SessionTable.States(), SessionLive, SessionClosed)
}

func check[S ~string](t *testing.T, name string, states []S, consts ...S) {
	t.Helper()
	want := slices.Sorted(slices.Values(consts))
	if !slices.Equal(states, want) {
		t.Errorf("%s table states %v, constants %v", name, states, want)
	}
}
