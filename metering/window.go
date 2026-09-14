// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"strconv"
	"strings"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Scope names one counter of one kind of object: the kind before the
// colon and the counter after it. CounterKey renders a Scope, an object
// id, and a window start into the store's key.
type Scope string

// The scopes, one per row of spec 009's counter table. The three key
// counters exist under the Key's spend window and, for the lifetime
// totals, under window none; the two markers are claimed once per
// window by the replica that announces its exhaustion.
const (
	ScopeKeyRequests     Scope = "key:requests"
	ScopeKeyTokens       Scope = "key:tokens"
	ScopeKeySpend        Scope = "key:spend"
	ScopeKeyExhausted    Scope = "key:exhausted"
	ScopeBudgetSpend     Scope = "budget:spend"
	ScopeBudgetExhausted Scope = "budget:exhausted"
)

// Window returns the bounds of w containing at: the start, which is
// what a counter key carries, and the instant the window resets, which
// is the Retry-After of a refusal and the expiry of the counter's row.
// A duration window is aligned to the Unix epoch in UTC, so every
// replica finds the same boundary from the clock alone; month starts on
// the first of at's month at 00:00:00Z; none starts at createdAt and
// never resets, which a zero resetsAt says.
func Window(w v1.Window, at, createdAt time.Time) (start, resetsAt time.Time) {
	return w.Bounds(at, createdAt)
}

// CounterKey renders the key of the counter table for scope s of the
// object id under window w at instant at: <kind>:<id>:<counter>:<start>
// with start the window's start as Unix seconds, so one key names
// exactly one window and a finished window's row is prunable by its own
// name. For the totals w is none and start is createdAt.
func CounterKey(s Scope, id string, w v1.Window, at, createdAt time.Time) string {
	start, _ := Window(w, at, createdAt)
	return KeyAt(s, id, start)
}

// KeyAt is CounterKey for a window whose start is already known.
func KeyAt(s Scope, id string, start time.Time) string {
	kind, counter, _ := strings.Cut(string(s), ":")
	return kind + ":" + id + ":" + counter + ":" + strconv.FormatInt(start.Unix(), 10)
}

// RetryAfter is the wait a window refusal names: the time from now to
// resetsAt, and zero for a window that never resets or has already
// reset, which the door renders as no Retry-After header.
func RetryAfter(now, resetsAt time.Time) time.Duration {
	if resetsAt.IsZero() || !resetsAt.After(now) {
		return 0
	}
	return resetsAt.Sub(now)
}
