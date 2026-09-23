// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/localissuer"
)

// Policy names who decides permission on the control plane, the value
// spec 011's /v1/self reports as policy.
type Policy string

// The three policies.
const (
	// PolicyAuthorizer is the operator's endpoint at LUX_AUTHORIZER_URL.
	PolicyAuthorizer Policy = "authorizer"
	// PolicyOwner is the built-in owner policy, with LUX_ADMIN_SUBJECTS.
	PolicyOwner Policy = "owner"
	// PolicyFile is the file mode: no issuer, listed or local, no bearer,
	// and nothing to authorize, because the directory is the desired
	// state.
	PolicyFile Policy = "file"
)

// Options is what Startup reads from the configuration, so a test builds
// one by hand.
type Options struct {
	// Issuers, Audiences, AuthorizerURL, AuthorizerToken,
	// AuthorizerTimeout, and AdminSubjects are the configuration rows of
	// spec 006.
	Issuers   []string
	Audiences []string
	// LocalIssuer and LocalKeys are the local issuer of spec 035: its
	// name, LUX_PUBLIC_URL, and the public halves of LUX_LOCAL_ISSUER_KEY
	// and LUX_LOCAL_ISSUER_KEYS, the signing key first. No listed issuer
	// and no local one is the file mode, which the configuration admits
	// only with LUX_MANIFEST_DIR; a manifest directory with an issuer of
	// either kind keeps its verifier, and its read-only control plane
	// answers verified callers.
	LocalIssuer       string
	LocalKeys         []jwt.LocalKey
	AuthorizerURL     string
	AuthorizerToken   string
	AuthorizerTimeout time.Duration
	AdminSubjects     []string
	// HTTP dials the issuers and the authorizer. Optional: the default is
	// an instrumented client with a ten second timeout, which the
	// authorizer client bounds further by its own deadline.
	HTTP *http.Client
	// Now is the decision cache's clock. Optional.
	Now func() time.Time
	// Observe receives every authorizer call's result and duration, for
	// the metric of spec 019. Optional.
	Observe func(result string, seconds float64)
}

// Auth is the identity a running gateway holds: the verifier over the
// issuers, the shared client when an authorizer is configured, the admin
// subjects, and which policy decides.
type Auth struct {
	// Verifier is nil in the file mode.
	Verifier *Verifier
	// Client is the shared authorizer client, nil under the owner policy
	// and in the file mode.
	Client *authz.Client
	// Admins are the rendered subjects of LUX_ADMIN_SUBJECTS; read and
	// unused under an authorizer.
	Admins []string
	Policy Policy
}

// Startup builds the identity from the configuration: the start-up fetch
// of every issuer, and the authorizer client when its URL is set. It is
// the one call serve makes for spec 006, and its error is a start-up
// failure naming the variable and the issuer. observe receives every
// authorizer call's result and duration for spec 019's metric; nil
// records none.
func Startup(ctx context.Context, cfg config.Config, client *http.Client, observe func(result string, seconds float64)) (*Auth, error) {
	o := Options{
		Issuers:           cfg.OIDCIssuers,
		Audiences:         cfg.OIDCAudiences,
		AuthorizerURL:     cfg.AuthorizerURL,
		AuthorizerToken:   cfg.AuthorizerToken,
		AuthorizerTimeout: cfg.AuthorizerTimeout,
		AdminSubjects:     cfg.AdminSubjects,
		HTTP:              client,
		Observe:           observe,
	}
	if cfg.LocalIssuerKey != nil {
		o.LocalIssuer = cfg.PublicURL.String()
		for _, k := range append([]*localissuer.Key{cfg.LocalIssuerKey}, cfg.LocalIssuerKeys...) {
			o.LocalKeys = append(o.LocalKeys, jwt.LocalKey{KeyID: k.ID(), Key: k.Public()})
		}
	}
	return New(ctx, o)
}

// New builds the identity from options.
func New(ctx context.Context, o Options) (*Auth, error) {
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, Transport: otel.Transport(nil)}
	}
	a := &Auth{Admins: o.AdminSubjects, Policy: PolicyFile}
	// The file mode is the absence of every verification path, not the
	// absence of a listed issuer: a local key alone is a server with a
	// control plane to authenticate.
	if len(o.Issuers) == 0 && o.LocalIssuer == "" && len(o.LocalKeys) == 0 {
		return a, nil
	}
	v, err := NewVerifier(ctx, VerifierOptions{Issuers: o.Issuers, LocalIssuer: o.LocalIssuer, LocalKeys: o.LocalKeys, Audiences: o.Audiences, HTTP: client})
	if err != nil {
		return nil, err
	}
	a.Verifier = v
	if o.AuthorizerURL == "" {
		a.Policy = PolicyOwner
		return a, nil
	}
	if o.AuthorizerToken == "" {
		return nil, errors.New("LUX_AUTHORIZER_TOKEN is unset while LUX_AUTHORIZER_URL is set, and the authorizer requires a bearer")
	}
	// The vocabulary is spec 006's table, so an action the gateway does
	// not declare is refused here and never sent: a typo is a mistake in
	// luxd, and the authorizer is not asked to name it.
	c, err := authz.NewClient(authz.Options{
		URL: o.AuthorizerURL, Token: o.AuthorizerToken, HTTP: client,
		Timeout: o.AuthorizerTimeout, Now: o.Now, Observe: o.Observe,
		Vocabulary: authorizer.Vocabulary(),
	})
	if err != nil {
		return nil, err
	}
	a.Client = c
	a.Policy = PolicyAuthorizer
	return a, nil
}

// Authorizer is the decision maker over the store's objects: the shared
// client under an authorizer, the OwnerPolicy over objects otherwise, and
// nil in the file mode, where nothing is authorized.
func (a *Auth) Authorizer(objects ObjectLookup) *Authorizer {
	switch a.Policy {
	case PolicyAuthorizer:
		return NewAuthorizer(a.Client)
	case PolicyOwner:
		return NewAuthorizer(&OwnerPolicy{Admins: a.Admins, Objects: objects})
	}
	return nil
}

// String is the one line the start-up log says about identity: the
// issuers, the local issuer with the algorithm and key id of each key,
// the audience, and who decides; under an authorizer it reports
// LUX_ADMIN_SUBJECTS as read and unused, and never the token or a key.
func (a *Auth) String() string {
	if a.Policy == PolicyFile {
		return "identity: file mode, no issuer and no control plane to authenticate"
	}
	var b strings.Builder
	if issuers := a.Verifier.Issuers(); len(issuers) > 0 {
		b.WriteString("identity: issuers ")
		b.WriteString(strings.Join(issuers, ", "))
	} else {
		b.WriteString("identity: no listed issuer")
	}
	if local := a.Verifier.DescribeLocal(); local != "" {
		b.WriteString("; local issuer ")
		b.WriteString(local)
	}
	b.WriteString("; audience ")
	b.WriteString(strings.Join(a.Verifier.Audiences(), ", "))
	admins := strconv.Itoa(len(a.Admins)) + " admin subject(s)"
	switch a.Policy {
	case PolicyAuthorizer:
		b.WriteString("; authorizer ")
		b.WriteString(a.Client.URL())
		b.WriteString("; LUX_ADMIN_SUBJECTS read and unused (")
		b.WriteString(admins)
		b.WriteString(")")
	default:
		b.WriteString("; owner policy with ")
		b.WriteString(admins)
	}
	return b.String()
}
