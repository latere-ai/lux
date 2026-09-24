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
// name. A none window renders the word none in place of a start: it is
// the lifetime counter, and a duration window that happens to begin at
// the object's creation instant must not share its key.
func CounterKey(s Scope, id string, w v1.Window, at, createdAt time.Time) string {
	if w == v1.WindowNone {
		return TotalKey(s, id)
	}
	start, _ := Window(w, at, createdAt)
	return keyOf(s, id, strconv.FormatInt(start.Unix(), 10))
}

// BudgetWindow is the Budget's current window at at: spec.window aligned
// to spec.anchor, started instead at spec.restartedAt when that instant
// lies inside the natural window and is not after at. The reset is the
// natural window's, zero under none.
func BudgetWindow(b *v1.Budget, at time.Time) (start, resetsAt time.Time) {
	start, resetsAt = b.Spec.Window.AnchoredBounds(at, b.Status.CreatedAt, b.Spec.Anchor)
	if r := b.Spec.RestartedAt; !r.IsZero() && !r.Before(start) && !r.After(at) && (resetsAt.IsZero() || r.Before(resetsAt)) {
		start = r.UTC()
	}
	return start, resetsAt
}

// BudgetCounterKey is the key of the Budget's counter of scope s in the
// window BudgetWindow finds at at. Without an anchor or a restart it is
// CounterKey's key, so a Budget's counters keep their rows across an
// upgrade; a restart under none renders the lifetime key with the
// instant, none@<unix>, so the lifetime counts from the restart.
func BudgetCounterKey(s Scope, b *v1.Budget, at time.Time) string {
	start, _ := BudgetWindow(b, at)
	if b.Spec.Window == v1.WindowNone {
		if start.Equal(b.Status.CreatedAt.UTC()) {
			return TotalKey(s, b.Status.ID)
		}
		return keyOf(s, b.Status.ID, "none@"+strconv.FormatInt(start.Unix(), 10))
	}
	return keyOf(s, b.Status.ID, strconv.FormatInt(start.Unix(), 10))
}

// MarkerKey is the exhaustion marker of a window claimed at an amount:
// the window's key and the amount in micro-units, so an amount raised
// inside a window re-arms its announcement.
func MarkerKey(window string, amount v1.Money) string {
	return window + ":" + strconv.FormatInt(int64(amount), 10)
}

// TotalKey is the key of the lifetime counter for scope s of the object
// id, which is CounterKey under window none.
func TotalKey(s Scope, id string) string { return keyOf(s, id, "none") }

func keyOf(s Scope, id, start string) string {
	kind, counter, _ := strings.Cut(string(s), ":")
	return kind + ":" + id + ":" + counter + ":" + start
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
