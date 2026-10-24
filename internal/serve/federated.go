// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/federation"
	v1 "latere.ai/x/lux/manifest/v1"
)

// ErrNoWorkloadIdentity is a federated Provider's credential asked of a
// gateway with neither LUX_WORKLOAD_IDENTITY_FILE nor
// LUX_WORKLOAD_IDENTITY_KUBERNETES: a Provider stored before the
// variables were taken away.
var ErrNoWorkloadIdentity = errors.New("the Provider's credential is federated, and this gateway has no workload identity: set LUX_WORKLOAD_IDENTITY_FILE or LUX_WORKLOAD_IDENTITY_KUBERNETES")

// FederatedCredentials answers a federated Provider (spec 051) with the
// access token Tokens holds for it, and every other Provider with Base's
// value. Tokens is nil on a gateway without a workload identity.
type FederatedCredentials struct {
	Base   gateway.CredentialSource
	Tokens *federation.Exchanger
}

// Credential implements gateway.CredentialSource for a caller that knows
// the Provider's id alone: Base's value.
func (f FederatedCredentials) Credential(ctx context.Context, providerID string) ([]byte, error) {
	return f.Base.Credential(ctx, providerID)
}

// ProviderCredential implements gateway.ProviderCredentialSource.
func (f FederatedCredentials) ProviderCredential(ctx context.Context, p *v1.Provider) ([]byte, error) {
	if p.Spec.Federated() == nil {
		return gateway.OpenCredential(ctx, f.Base, p)
	}
	if f.Tokens == nil {
		return nil, ErrNoWorkloadIdentity
	}
	return f.Tokens.Token(ctx, p)
}

// Refused implements gateway.CredentialRefusals: an exchanged token the
// upstream answered 401 to is dropped.
func (f FederatedCredentials) Refused(p *v1.Provider, value []byte) {
	if f.Tokens != nil && p.Spec.Federated() != nil {
		f.Tokens.Refused(p, value)
	}
}

var (
	_ gateway.ProviderCredentialSource = FederatedCredentials{}
	_ gateway.CredentialRefusals       = FederatedCredentials{}
)
