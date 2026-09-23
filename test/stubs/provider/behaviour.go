// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strconv"
	"strings"
	"time"
)

// Kind is one row of the failure injection table.
type Kind string

// The rows of the table, by the upstream model name that selects each.
const (
	// KindNormal is the deterministic answer, for a name in no row.
	KindNormal Kind = ""
	// KindFail500 is 500 with the dialect's error body.
	KindFail500 Kind = "fail-500"
	// KindFail429 is 429 with Retry-After: 1.
	KindFail429 Kind = "fail-429"
	// KindFail401 is 401, the shape a wrong credential produces.
	KindFail401 Kind = "fail-401"
	// KindSlow waits the Go duration after the dash, then answers.
	KindSlow Kind = "slow"
	// KindHang writes the status and the headers and then nothing, until
	// the caller cancels.
	KindHang Kind = "hang"
	// KindFailStreamMid streams two content events, then closes the
	// connection with no usage event.
	KindFailStreamMid Kind = "fail-stream-mid"
	// KindFailBody is 200 with a body that is not the dialect's shape.
	KindFailBody Kind = "fail-body"
	// KindRedirect is 302 to another host.
	KindRedirect Kind = "redirect"
	// KindFail400 is 400 with the dialect's error body.
	KindFail400 Kind = "fail-400"
	// KindFail529 is 529 with an overloaded body.
	KindFail529 Kind = "fail-529"
	// KindFailHTML is 200 with Content-Type: text/html and an HTML body.
	KindFailHTML Kind = "fail-html"
	// KindTokens is the deterministic answer with the two usage numbers
	// after the dash.
	KindTokens Kind = "tokens"
	// KindEvents is a streamed answer of n content events instead of
	// five.
	KindEvents Kind = "events"
)

// Behavior is what one upstream model name asks of the stub: the row,
// and the figures the row's name carried.
type Behaviour struct {
	Kind Kind
	// Wait is slow-<duration>'s duration.
	Wait time.Duration
	// InputTokens and OutputTokens are the usage the answer reports.
	InputTokens, OutputTokens int64
	// Events is how many content events a streamed answer carries.
	Events int
}

// Behaviors lists the names of the table, one per row, as a test that
// walks the table reads them.
func Behaviours() []string {
	return []string{
		string(KindFail500), string(KindFail429), string(KindFail401), "slow-10ms", string(KindHang),
		string(KindFailStreamMid), string(KindFailBody), string(KindRedirect), string(KindFail400),
		string(KindFail529), string(KindFailHTML), "tokens-1000-500", "events-3",
	}
}

// ParseBehaviour reads an upstream model name against the table: an exact
// row name, a slow-, tokens-, or events- name with its figures, and the
// deterministic answer for anything else, a malformed figure included,
// because a Model named tokens-x is a model and not a request to fail.
func ParseBehaviour(name string) Behaviour {
	b := Behaviour{InputTokens: DefaultInputTokens, OutputTokens: DefaultOutputTokens, Events: DefaultEvents}
	switch Kind(name) {
	case KindFail500, KindFail429, KindFail401, KindHang, KindFailStreamMid, KindFailBody, KindRedirect, KindFail400, KindFail529, KindFailHTML:
		b.Kind = Kind(name)
		return b
	case KindNormal, KindSlow, KindTokens, KindEvents:
	}
	if rest, ok := strings.CutPrefix(name, string(KindSlow)+"-"); ok {
		if d, err := time.ParseDuration(rest); err == nil && d >= 0 {
			b.Kind, b.Wait = KindSlow, d
		}
		return b
	}
	if rest, ok := strings.CutPrefix(name, string(KindTokens)+"-"); ok {
		in, out, found := strings.Cut(rest, "-")
		i, ierr := strconv.ParseInt(in, 10, 64)
		o, oerr := strconv.ParseInt(out, 10, 64)
		if found && ierr == nil && oerr == nil && i >= 0 && o >= 0 {
			b.Kind, b.InputTokens, b.OutputTokens = KindTokens, i, o
		}
		return b
	}
	if rest, ok := strings.CutPrefix(name, string(KindEvents)+"-"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 0 {
			b.Kind, b.Events = KindEvents, n
		}
		return b
	}
	return b
}
