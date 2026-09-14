// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_retryafter

package serve

import (
	"context"
	"errors"

	"latere.ai/x/lux/gateway"
)

// The mutation of spec 018's sixth row: a rate refusal carries no
// Retry-After. Only case007RateLimited may redden.
func init() {
	mutateLimiter = func(l gateway.Limiter) gateway.Limiter { return impatientLimiter{next: l} }
}

type impatientLimiter struct{ next gateway.Limiter }

func (l impatientLimiter) Reserve(ctx context.Context, r gateway.Reservation) (gateway.Lease, error) {
	lease, err := l.next.Reserve(ctx, r)
	var refusal *gateway.Refusal
	if errors.As(err, &refusal) && refusal.Code == gateway.CodeRateLimited {
		return nil, &gateway.Refusal{Code: refusal.Code, Detail: refusal.Detail}
	}
	return lease, err
}
