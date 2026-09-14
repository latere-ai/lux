// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"math"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// DefaultOutputTokens is the output half of a reservation when the
// request names no maximum: the door reserves this many, and the settle
// replaces it with the measured count.
const DefaultOutputTokens int64 = 1024

// Reserved is the token reservation of one request: the estimate of its
// input plus the requested maximum output, which the door has already
// defaulted to DefaultOutputTokens where the request named none, and
// which is zero on a count and an opaque route.
func Reserved(estimatedInput, requestedOutput int64) int64 {
	return max(estimatedInput, 0) + max(requestedOutput, 0)
}

// Settled is the count a reservation settles to: the measured input plus
// the measured output. Zero is a request that was refused after the
// reservation or failed before it was sent, whose reservation is
// refunded whole.
func Settled(measuredInput, measuredOutput int64) int64 {
	return max(measuredInput, 0) + max(measuredOutput, 0)
}

// Adjustment is what the settle gives back to or takes further from the
// bucket: the reservation less the settled count, a refund when
// positive and a further debit when negative.
func Adjustment(reserved, settled int64) int64 {
	return reserved - settled
}

// Projected is what a spend window would hold if this request ran: the
// store's total as of the last flush, this replica's unflushed delta,
// and the estimated cost of the request.
func Projected(known, pending, estimate int64) int64 {
	return known + pending + estimate
}

// Exceeds reports whether a projected spend is over the amount, which is
// the refusal of a hard window; a window is refused when it would go
// over, so a request that lands exactly on the amount runs.
func Exceeds(projected, amount int64) bool {
	return projected > amount
}

// Overshoot is the bound on how far a hard spend limit can be exceeded
// by replicas that flush on an interval:
//
//	overshoot <= (R - 1) × F × T × C + C
//
// with R replicas, a flush interval F, T requests per second per replica
// against the counter, and C the greatest cost of one request. The
// first term is what the other replicas spent inside one flush interval
// that this one has not yet seen; the last is this replica's own request
// that crossed. With one replica the bound is one request.
func Overshoot(replicas int, flush time.Duration, requestsPerSecond float64, cost v1.Money) v1.Money {
	others := math.Max(float64(replicas-1), 0) * flush.Seconds() * requestsPerSecond * float64(cost)
	return v1.Money(math.Round(others)) + cost
}
