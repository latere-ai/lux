// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
)

// Defaults for the identity variables of spec 006.
const (
	DefaultOIDCAudience      = "lux"
	DefaultAuthorizerTimeout = 5 * time.Second
)

// loadIdentity reads the variables of spec 006 into c and returns every
// problem found, each naming its variable. The issuers are required
// unless LUX_MANIFEST_DIR selects the file mode, which has no control
// plane to authenticate; the variable itself is spec 010's and is read
// here for that one rule.
func (c *Config) loadIdentity(getenv Getenv) []string {
	var problems []string
	c.OIDCIssuers = c.issuers(getenv("LUX_OIDC_ISSUERS"), &problems)
	if len(c.OIDCIssuers) == 0 && strings.TrimSpace(getenv("LUX_MANIFEST_DIR")) == "" {
		problems = append(problems, "LUX_OIDC_ISSUERS is unset, and the control plane needs at least one issuer unless LUX_MANIFEST_DIR selects the file mode")
	}
	c.OIDCAudience = strings.TrimSpace(withDefault(getenv("LUX_OIDC_AUDIENCE"), DefaultOIDCAudience))
	if strings.Contains(c.OIDCAudience, ",") {
		problems = append(problems, "LUX_OIDC_AUDIENCE is "+strconv.Quote(c.OIDCAudience)+", a list, and one audience is accepted")
	}
	c.OIDCInsecureIssuers = c.insecureIssuers(getenv("LUX_OIDC_INSECURE_ISSUERS"), &problems)
	for _, iss := range c.OIDCIssuers {
		if u, _ := endpoint(iss); !isHTTPS(u) && !isLoopback(u) && !slices.Contains(c.OIDCInsecureIssuers, iss) {
			problems = append(problems, "LUX_OIDC_ISSUERS "+iss+" is http:// on a host other than loopback, so list it in LUX_OIDC_INSECURE_ISSUERS to accept it")
		}
	}

	c.AuthorizerURL = strings.TrimSpace(getenv("LUX_AUTHORIZER_URL"))
	c.AuthorizerToken = strings.TrimSpace(getenv("LUX_AUTHORIZER_TOKEN"))
	if c.AuthorizerURL != "" {
		u, ok := endpoint(c.AuthorizerURL)
		switch {
		case !ok:
			problems = append(problems, "LUX_AUTHORIZER_URL is "+strconv.Quote(c.AuthorizerURL)+", not an http:// or https:// URL with a host")
		case !isHTTPS(u) && !isLoopback(u):
			problems = append(problems, "LUX_AUTHORIZER_URL "+c.AuthorizerURL+" is http:// on a host other than loopback, and the authorizer is reached over https:// or on loopback")
		}
		if c.AuthorizerToken == "" {
			problems = append(problems, "LUX_AUTHORIZER_TOKEN is unset while LUX_AUTHORIZER_URL is set, and the authorizer requires a bearer")
		}
	}

	c.AuthorizerTimeout = DefaultAuthorizerTimeout
	if raw := strings.TrimSpace(getenv("LUX_AUTHORIZER_TIMEOUT")); raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			problems = append(problems, "LUX_AUTHORIZER_TIMEOUT is "+strconv.Quote(raw)+", not a duration such as 5s")
		case d <= 0:
			problems = append(problems, "LUX_AUTHORIZER_TIMEOUT is "+raw+", and one decision's deadline is above zero")
		default:
			c.AuthorizerTimeout = d
		}
	}

	c.AdminSubjects = authz.ParseSubjects(getenv("LUX_ADMIN_SUBJECTS"))
	for _, s := range c.AdminSubjects {
		if _, _, ok := authz.SplitSubject(s); !ok {
			problems = append(problems, "LUX_ADMIN_SUBJECTS entry "+strconv.Quote(s)+" is not a rendered subject of the form <issuer>|<sub>")
		}
	}
	return problems
}

// issuers reads a comma separated list of issuer URLs, each trimmed and
// rendered without its trailing slash, so two spellings of one issuer are
// one entry. An entry that is not an http:// or https:// URL with a host,
// and an issuer listed twice, are problems.
func (c *Config) issuers(raw string, problems *[]string) []string {
	var out []string
	for _, s := range splitList(raw) {
		iss := strings.TrimRight(s, "/")
		if _, ok := endpoint(iss); !ok {
			*problems = append(*problems, "LUX_OIDC_ISSUERS entry "+strconv.Quote(s)+" is not an http:// or https:// URL with a host")
			continue
		}
		if slices.Contains(out, iss) {
			*problems = append(*problems, "LUX_OIDC_ISSUERS lists "+iss+" twice")
			continue
		}
		out = append(out, iss)
	}
	return out
}

// insecureIssuers reads the issuers allowed to use http:// off loopback.
// Each is rendered as the issuers are and must be one of them, since an
// entry that names no listed issuer permits nothing and is a typo.
func (c *Config) insecureIssuers(raw string, problems *[]string) []string {
	var out []string
	for _, s := range splitList(raw) {
		iss := strings.TrimRight(s, "/")
		if !slices.Contains(c.OIDCIssuers, iss) {
			*problems = append(*problems, "LUX_OIDC_INSECURE_ISSUERS names "+iss+", which is not in LUX_OIDC_ISSUERS")
			continue
		}
		out = append(out, iss)
	}
	return out
}

// splitList reads a comma separated list, trimming each entry and dropping
// the empty ones.
func splitList(raw string) []string {
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// endpoint parses s as an http:// or https:// URL with a host; ok is
// false for anything else.
func endpoint(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, false
	}
	return u, true
}

// isHTTPS reports whether the endpoint is an https:// URL.
func isHTTPS(u *url.URL) bool { return u.Scheme == "https" }

// isLoopback reports whether the endpoint's host is localhost or a
// loopback address, the one place http:// is accepted without being
// listed.
func isLoopback(u *url.URL) bool {
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
