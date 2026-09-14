// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_unpriced

package serve

import (
	"context"
	"errors"

	"latere.ai/x/lux/gateway"
)

// The mutation of spec 018's fifth row: an unpriced Model under a hard
// Budget is served, the Limiter's model_unpriced turned into an
// admission that settles nothing. Only case007UnpricedUnderABudget may
// redden.
func init() {
	mutateLimiter = func(l gateway.Limiter) gateway.Limiter { return pricelessLimiter{next: l} }
}

type pricelessLimiter struct{ next gateway.Limiter }

func (l pricelessLimiter) Reserve(ctx context.Context, r gateway.Reservation) (gateway.Lease, error) {
	lease, err := l.next.Reserve(ctx, r)
	var refusal *gateway.Refusal
	if errors.As(err, &refusal) && refusal.Code == gateway.CodeModelUnpriced {
		return idleLease{}, nil
	}
	return lease, err
}

type idleLease struct{}

func (idleLease) Settle(context.Context, gateway.Tokens) {}
