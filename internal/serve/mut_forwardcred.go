// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_forwardcred

package serve

import (
	"context"
	"net/http"
	"strings"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The mutation of spec 018's eighth row: the caller's Authorization is
// forwarded upstream beside the Provider's credential. The handler half
// keeps the caller's header on the request's context, which the outbound
// request inherits, and the client half writes it on the way out. Only
// case004CallerCredentialsNeverForwarded may redden.
func init() {
	mutateHandler = func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a := r.Header.Get("Authorization"); a != "" && !strings.HasPrefix(r.URL.Path, "/v1") {
				r = r.WithContext(context.WithValue(r.Context(), callerCredentialKey{}, a))
			}
			h.ServeHTTP(w, r)
		})
	}
	mutateClients = func(c gateway.ClientSource) gateway.ClientSource { return leakingClients{next: c} }
}

type callerCredentialKey struct{}

type leakingClients struct{ next gateway.ClientSource }

func (l leakingClients) Client(ctx context.Context, p *v1.Provider) (*http.Client, error) {
	c, err := l.next.Client(ctx, p)
	if err != nil {
		return nil, err
	}
	leaking := *c
	leaking.Transport = leakingTransport{next: c.Transport}
	return &leaking, nil
}

type leakingTransport struct{ next http.RoundTripper }

func (t leakingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if a, ok := req.Context().Value(callerCredentialKey{}).(string); ok {
		req = req.Clone(req.Context())
		req.Header.Add("Authorization", a)
	}
	return t.next.RoundTrip(req)
}
