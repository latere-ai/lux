// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults and bounds for the control plane variables of spec 011 and
// the two request path variables of spec 004 the API's Resolve and the
// doors' body cap read. A manifest below a kilobyte cannot carry a Key
// with its selectors; one above sixteen mebibytes is not a manifest. A
// data plane body below four kilobytes cannot carry a prompt; the
// ceiling is what one replica could defensibly buffer for a replay. An
// upstream request is at least a second and at most an hour, the bounds
// spec 003 puts on a Provider's own timeout.
const (
	DefaultRequestsPerMinute                      = 600
	DefaultUnauthenticatedRequestsPerMinute       = 60
	DefaultMaxManifestBytes                 int64 = 64 << 10
	MinMaxManifestBytes                     int64 = 1 << 10
	MaxMaxManifestBytes                     int64 = 16 << 20
	DefaultMaxBodyBytes                     int64 = 64 << 20
	MinMaxBodyBytes                         int64 = 4 << 10
	MaxMaxBodyBytes                         int64 = 1 << 30
	DefaultUpstreamTimeout                        = 10 * time.Minute
	MinUpstreamTimeout                            = time.Second
	MaxUpstreamTimeout                            = time.Hour
)

// loadAPI reads the variables of spec 011 into c, and the two rows of
// spec 004 that reach the API's Resolve defaults and the doors' body
// cap, returning every problem found, each naming its variable.
// LUX_PUBLIC_URL is required in every mode, because every URL in a
// response and the loop check of Resolve are built from it.
func (c *Config) loadAPI(getenv Getenv) []string {
	var problems []string
	c.PublicURL, problems = publicURL(getenv("LUX_PUBLIC_URL"), problems)
	c.RequestsPerMinute, problems = countOr("LUX_REQUESTS_PER_MINUTE", getenv("LUX_REQUESTS_PER_MINUTE"), DefaultRequestsPerMinute, problems)
	c.UnauthenticatedRequestsPerMinute, problems = countOr("LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE", getenv("LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE"), DefaultUnauthenticatedRequestsPerMinute, problems)
	c.TrustedProxies, problems = prefixes("LUX_TRUSTED_PROXIES", getenv("LUX_TRUSTED_PROXIES"), problems)
	c.MaxManifestBytes, problems = byteSize("LUX_MAX_MANIFEST_BYTES", getenv("LUX_MAX_MANIFEST_BYTES"), DefaultMaxManifestBytes, MinMaxManifestBytes, MaxMaxManifestBytes, problems)
	c.MaxBodyBytes, problems = byteSize("LUX_MAX_BODY_BYTES", getenv("LUX_MAX_BODY_BYTES"), DefaultMaxBodyBytes, MinMaxBodyBytes, MaxMaxBodyBytes, problems)
	c.UpstreamTimeout, problems = interval("LUX_UPSTREAM_TIMEOUT", getenv("LUX_UPSTREAM_TIMEOUT"), DefaultUpstreamTimeout, MinUpstreamTimeout, MaxUpstreamTimeout, problems)
	return problems
}

// publicURL reads LUX_PUBLIC_URL: an absolute http:// or https:// URL
// with a host and no query or fragment, its trailing slash dropped so
// a path joins it with one slash. Unset is a problem.
func publicURL(raw string, problems []string) (*url.URL, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, append(problems, "LUX_PUBLIC_URL is unset, and every URL in a response is built from the address callers reach the gateway at")
	}
	u, ok := endpoint(raw)
	switch {
	case !ok:
		return nil, append(problems, "LUX_PUBLIC_URL is "+strconv.Quote(raw)+", not an http:// or https:// URL with a host")
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return nil, append(problems, "LUX_PUBLIC_URL is "+strconv.Quote(raw)+", and the address carries no query, fragment, or user information")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, problems
}

// countOr reads a whole number of zero or more, or def when blank.
func countOr(name, raw string, def int, problems []string) (int, []string) {
	if strings.TrimSpace(raw) == "" {
		return def, problems
	}
	return count(name, raw, problems)
}

// prefixes reads a comma separated list of CIDR ranges; an entry that is
// not one, a bare address included, is a problem, because a bare
// address left as a range would trust one host by accident when the
// operator meant a network or the reverse.
func prefixes(name, raw string, problems []string) ([]netip.Prefix, []string) {
	var out []netip.Prefix
	for _, s := range splitList(raw) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			problems = append(problems, name+" entry "+strconv.Quote(s)+" is not a CIDR range such as 10.0.0.0/8")
			continue
		}
		out = append(out, p.Masked())
	}
	return out, problems
}

// byteSize reads a byte count within bounds: a whole number, or one with
// a Ki, Mi, or Gi suffix as spec 002's table spells 64Mi; def when
// blank.
func byteSize(name, raw string, def, minimum, maximum int64, problems []string) (int64, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, problems
	}
	digits, unit := raw, int64(1)
	for _, s := range []struct {
		suffix string
		unit   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}} {
		if strings.HasSuffix(raw, s.suffix) {
			digits, unit = strings.TrimSuffix(raw, s.suffix), s.unit
			break
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	switch {
	case err != nil || n < 0 || n > math.MaxInt64/unit:
		return def, append(problems, name+" is "+strconv.Quote(raw)+", not a byte count such as "+formatBytes(def))
	case n*unit < minimum || n*unit > maximum:
		return def, append(problems, name+" is "+raw+", not between "+formatBytes(minimum)+" and "+formatBytes(maximum))
	default:
		return n * unit, problems
	}
}

// formatBytes renders a byte count with the largest binary suffix that
// divides it, so a bound reads as the table writes it.
func formatBytes(n int64) string {
	for _, s := range []struct {
		suffix string
		unit   int64
	}{{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if n >= s.unit && n%s.unit == 0 {
			return strconv.FormatInt(n/s.unit, 10) + s.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}
