// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package token is the token role of luxd, the fourth role of the server
// binary (spec 035): it signs one control plane token with the local
// issuer's key and returns it for the caller to print. It reads the four
// variables config.LoadToken reads and writes nothing: no store is
// opened, no journal row and no event is written, and nothing is dialed,
// so it is safe beside a serving installation, the promise the check role
// makes. The key encryption key and the database are neither read nor
// required, so the role runs wherever the key is.
//
// The token is the one kind the verifier already reads, a JWS with iss,
// sub, aud, iat, exp, and jti, so nothing on the request path learns that
// the installation minted its own credential.
package token

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/localissuer"
)

// The lifetime bounds of spec 035. The cap keeps a minted token from
// becoming a standing credential: the key can mint one for any subject,
// so what it mints expires within a day. It is also the verifier's
// default age bound, so a token never outlives its own iat there either.
const (
	DefaultTTL = time.Hour
	MaxTTL     = 24 * time.Hour
)

// Options is one run of the role: the environment and the three flags.
type Options struct {
	// Getenv is the environment the configuration is read through.
	Getenv config.Getenv
	// Subject is --subject, the token's sub; empty takes the sub of the
	// single LUX_ADMIN_SUBJECTS entry whose issuer is LUX_PUBLIC_URL.
	Subject string
	// Audience is --audience, one name of LUX_OIDC_AUDIENCE; empty is the
	// primary.
	Audience string
	// TTL is --ttl, above zero and at most MaxTTL. The flag's default is
	// DefaultTTL; zero here is a TTL of zero and refused.
	TTL time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// UsageError is a refusal of the invocation rather than of the
// configuration: a flag out of its range, a subject with no default, an
// audience the installation does not accept, or the local issuer off.
// luxd answers it with exit code 2, and a configuration problem with 1.
type UsageError struct {
	msg string
}

func (e *UsageError) Error() string { return e.msg }

func usage(msg string) error { return &UsageError{msg: msg} }

// IsUsage reports whether err is a UsageError.
func IsUsage(err error) bool {
	var u *UsageError
	return errors.As(err, &u)
}

// Mint reads the configuration and signs one token for the options. The
// TTL is checked first, since it needs no configuration; then the
// configuration loads or its one error is returned; then the key, the
// subject, and the audience are resolved, each refusal a UsageError
// naming the flag or the variable to set.
func Mint(o Options) (string, error) {
	if o.TTL <= 0 || o.TTL > MaxTTL {
		return "", usage("--ttl is " + o.TTL.String() + ", and a token lives above zero and at most " + MaxTTL.String() + ", so a minted token is never a standing credential")
	}
	cfg, err := config.LoadToken(o.Getenv)
	if err != nil {
		return "", err
	}
	if cfg.LocalIssuerKey == nil {
		return "", usage("LUX_LOCAL_ISSUER_KEY is unset, and luxd token signs with the local issuer's key")
	}
	issuer := cfg.PublicURL.String()
	subject, err := resolveSubject(o.Subject, issuer, cfg.AdminSubjects)
	if err != nil {
		return "", err
	}
	audience := cfg.OIDCAudiences[0]
	if o.Audience != "" {
		if !slices.Contains(cfg.OIDCAudiences, o.Audience) {
			return "", usage("--audience is " + strconv.Quote(o.Audience) + ", and a token is minted for a name of LUX_OIDC_AUDIENCE: " + strings.Join(cfg.OIDCAudiences, ", "))
		}
		audience = o.Audience
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	return cfg.LocalIssuerKey.Mint(localissuer.Claims{Issuer: issuer, Subject: subject, Audience: audience, IssuedAt: now(), TTL: o.TTL})
}

// resolveSubject is the token's sub: the flag when given, and otherwise
// the sub half of the one LUX_ADMIN_SUBJECTS entry whose issuer half is
// the local issuer, so a token minted with no flag is the installation's
// administrator. None or several such entries leave no default. A sub
// carrying the separator is refused, since the rendered subject
// <issuer>|<sub> would split at it into another issuer and another sub.
func resolveSubject(flag, issuer string, admins []string) (string, error) {
	if flag != "" {
		if strings.Contains(flag, "|") || strings.TrimSpace(flag) != flag {
			return "", usage("--subject is " + strconv.Quote(flag) + ", and a sub carries no | and no surrounding space, since the caller renders as <issuer>|<sub>")
		}
		return flag, nil
	}
	var subs []string
	for _, s := range admins {
		if iss, sub, ok := authz.SplitSubject(s); ok && strings.TrimRight(iss, "/") == issuer && sub != "" {
			subs = append(subs, sub)
		}
	}
	if len(subs) != 1 {
		return "", usage(fmt.Sprintf("--subject is required, since LUX_ADMIN_SUBJECTS names %d subject(s) of the local issuer %s and the default is the one such subject", len(subs), issuer))
	}
	return subs[0], nil
}
