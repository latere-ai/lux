// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/localissuer"
)

// Defaults for the identity variables of spec 006.
const (
	DefaultOIDCAudience      = "lux"
	DefaultAuthorizerTimeout = 5 * time.Second
)

// loadIdentity reads the variables of spec 006 into c and returns every
// problem found, each naming its variable. The issuers are required
// unless LUX_LOCAL_ISSUER_KEY configures the local issuer of spec 035 or
// LUX_MANIFEST_DIR selects the file mode, which has no control plane to
// authenticate; that variable is spec 010's, read by Load before this
// runs. A local key that is set and unusable is its own problem, so the
// issuer rule reads whether the variable is set, not whether it parsed.
func (c *Config) loadIdentity(getenv Getenv) []string {
	var problems []string
	c.OIDCIssuers = c.issuers(getenv("LUX_OIDC_ISSUERS"), &problems)
	var local []string
	c.LocalIssuerKey, c.LocalIssuerKeys, local = LocalIssuerKeys(getenv)
	problems = append(problems, local...)
	if len(c.OIDCIssuers) == 0 && strings.TrimSpace(getenv("LUX_LOCAL_ISSUER_KEY")) == "" && c.ManifestDir == "" {
		problems = append(problems, "LUX_OIDC_ISSUERS is unset, and the control plane needs an issuer to verify a bearer against unless LUX_LOCAL_ISSUER_KEY sets a local one or LUX_MANIFEST_DIR selects the file mode")
	}
	c.OIDCAudiences = audiences(getenv("LUX_OIDC_AUDIENCE"), &problems)
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

	c.AuthorizeListItems, problems = flag("LUX_AUTHORIZE_LIST_ITEMS", getenv("LUX_AUTHORIZE_LIST_ITEMS"), problems)
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

	c.AdminSubjects = adminSubjects(getenv("LUX_ADMIN_SUBJECTS"), &problems)
	return problems
}

// adminSubjects reads LUX_ADMIN_SUBJECTS: a comma separated list of
// rendered subjects, an entry without the separator a problem.
func adminSubjects(raw string, problems *[]string) []string {
	out := authz.ParseSubjects(raw)
	for _, s := range out {
		if _, _, ok := authz.SplitSubject(s); !ok {
			*problems = append(*problems, "LUX_ADMIN_SUBJECTS entry "+strconv.Quote(s)+" is not a rendered subject of the form <issuer>|<sub>")
		}
	}
	return out
}

// LocalIssuerKeys reads the two variables of spec 035's local issuer:
// LUX_LOCAL_ISSUER_KEY, the key it signs with and verifies against, nil
// when unset, and LUX_LOCAL_ISSUER_KEYS, further keys that verify and
// never sign, for a rotation. It returns every problem found, each naming
// its variable and none echoing a value. Load reads the pair through it,
// and so does luxd check, whose local issuer row reports the keys whether
// or not the rest of the configuration loaded.
//
// Two keys with one key id are a problem, the same key listed twice
// included: a token's kid then names two keys and the verifier refuses
// it rather than choose, so leaving the signing key in the rotation list
// would refuse every token the installation mints.
func LocalIssuerKeys(getenv Getenv) (*localissuer.Key, []*localissuer.Key, []string) {
	key, problems := localIssuerKey(getenv("LUX_LOCAL_ISSUER_KEY"), nil)
	raw := getenv("LUX_LOCAL_ISSUER_KEYS")
	if strings.TrimSpace(raw) == "" {
		return key, nil, problems
	}
	if strings.TrimSpace(getenv("LUX_LOCAL_ISSUER_KEY")) == "" {
		return nil, nil, append(problems, "LUX_LOCAL_ISSUER_KEYS is set while LUX_LOCAL_ISSUER_KEY is unset, and the further keys verify beside the key the local issuer signs with")
	}
	more, err := localissuer.ParseList(raw)
	if err != nil {
		return key, nil, append(problems, "LUX_LOCAL_ISSUER_KEYS "+err.Error())
	}
	seen := map[string]bool{}
	if key != nil {
		seen[key.ID()] = true
	}
	for i, k := range more {
		switch {
		case key != nil && k.ID() == key.ID():
			problems = append(problems, fmt.Sprintf("LUX_LOCAL_ISSUER_KEYS entry %d is the key LUX_LOCAL_ISSUER_KEY holds, key id %s, and a token naming a key id held twice verifies against neither", i+1, k.ID()))
		case seen[k.ID()]:
			problems = append(problems, "LUX_LOCAL_ISSUER_KEYS lists key id "+k.ID()+" twice, and a token naming a key id held twice verifies against neither")
		}
		seen[k.ID()] = true
	}
	return key, more, problems
}

// localIssuerKey reads LUX_LOCAL_ISSUER_KEY: blank is the local issuer
// off, and a set value is one PEM encoded PKCS #8 private key or a
// problem that does not echo it.
func localIssuerKey(raw string, problems []string) (*localissuer.Key, []string) {
	if strings.TrimSpace(raw) == "" {
		return nil, problems
	}
	k, err := localissuer.Parse(raw)
	if err != nil {
		return nil, append(problems, "LUX_LOCAL_ISSUER_KEY "+err.Error())
	}
	return k, problems
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

// audiences reads LUX_OIDC_AUDIENCE: a comma separated list of distinct
// names, each trimmed, the first the primary. Unset is DefaultOIDCAudience
// alone, so an installation on its own hostname is unchanged. An empty
// entry is a problem, because `a,,b` and `lux,` are typing mistakes and an
// empty audience would verify nothing or everything depending on where it
// landed; a name listed twice is a problem, because it permits nothing new
// and hides a typo, the rule LUX_OIDC_ISSUERS runs.
func audiences(raw string, problems *[]string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{DefaultOIDCAudience}
	}
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
			*problems = append(*problems, "LUX_OIDC_AUDIENCE is "+strconv.Quote(raw)+", and one of its entries is empty")
			return out
		case slices.Contains(out, s):
			*problems = append(*problems, "LUX_OIDC_AUDIENCE lists "+s+" twice")
		default:
			out = append(out, s)
		}
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
