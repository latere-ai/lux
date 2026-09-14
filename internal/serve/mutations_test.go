// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !mut_ifmatch && !mut_default && !mut_loss && !mut_unknownfield && !mut_unpriced && !mut_retryafter && !mut_keyvalue && !mut_forwardcred

package serve

import (
	"context"
	"net/http"
	"testing"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
)

type identityLimiter struct{}

func (identityLimiter) Reserve(context.Context, gateway.Reservation) (gateway.Lease, error) {
	return nil, nil
}

type identityClients struct{}

func (identityClients) Client(context.Context, *v1.Provider) (*http.Client, error) { return nil, nil }

// TestMutationsAreTheIdentityWithoutATag: spec 018's seams return what
// they are given in a build without a mutation tag, so a released
// binary that called them would change nothing, and luxd calls none.
func TestMutationsAreTheIdentityWithoutATag(t *testing.T) {
	var h http.Handler = http.NewServeMux()
	if Mutate(h) != h {
		t.Error("Mutate changed the handler")
	}
	var l gateway.Limiter = identityLimiter{}
	if MutateLimiter(l) != l {
		t.Error("MutateLimiter changed the Limiter")
	}
	var c gateway.ClientSource = identityClients{}
	if MutateClients(c) != c {
		t.Error("MutateClients changed the clients")
	}
	if mutateHandler != nil || mutateLimiter != nil || mutateClients != nil {
		t.Error("a hook is set in a build without a mutation tag")
	}
}
