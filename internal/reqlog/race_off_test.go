// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !race

package reqlog

// raceEnabled says whether the suite runs under the race detector, which
// slows every lock and copy by an order of magnitude, so the hot path
// bound is loosened rather than skipped there.
const raceEnabled = false
