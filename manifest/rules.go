// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The syntax rules of the field tables.
var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	modelSegment = regexp.MustCompile(`^[a-z0-9]([a-z0-9._:-]*[a-z0-9])?$`)
	labelName    = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	labelValue   = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	headerToken  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	posixName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)
	ulidBody     = `[0-9A-Z]{26}`
	providerRef  = regexp.MustCompile(`^` + v1.PrefixProvider + ulidBody + `$`)
	budgetRef    = regexp.MustCompile(`^` + v1.PrefixBudget + ulidBody + `$`)
)

// The size limits of the tables.
const (
	maxLabelKey        = 63
	maxLabelPrefix     = 253
	maxDNSLabel        = 63
	maxModelName       = 128
	maxAnnotationValue = 4096
	maxAnnotations     = 65536
	maxHeaders         = 16
	maxHeaderValue     = 4096
	maxUpstreamName    = 256
	maxSelectors       = 64
	maxCredential      = 4096
	minSuppliedValue   = 32
	maxSuppliedValue   = 4096
)

// hasKindPrefix reports whether a name begins with an id prefix.
func hasKindPrefix(name string) bool {
	for _, p := range v1.KindPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// validDNSLabel is the name rule of a Provider, Key, or Budget.
func validDNSLabel(s string) bool {
	return len(s) <= maxDNSLabel && dnsLabel.MatchString(s)
}

// validModelName is the name rule of a Model: one or more segments joined
// by /, each of [a-z0-9._:-] beginning and ending alphanumeric, at most
// 128 characters in all, because a discovered Model is named
// <provider>/<upstream name> and an upstream name may carry dots, a colon
// tag, or slashes of its own.
func validModelName(s string) bool {
	if s == "" || len(s) > maxModelName {
		return false
	}
	for seg := range strings.SplitSeq(s, "/") {
		if !modelSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// validName applies the kind's name rule.
func validName(kind, name string) bool {
	if kind == v1.KindModel {
		return validModelName(name)
	}
	return validDNSLabel(name)
}

// nameRule describes the kind's name rule for a developer detail.
func nameRule(kind string) string {
	if kind == v1.KindModel {
		return "a Model name is one or more segments of [a-z0-9._:-] joined by /, each beginning and ending alphanumeric, at most 128 characters"
	}
	return "a " + kind + " name is a DNS-1123 label: [a-z0-9-], beginning and ending alphanumeric, at most 63 characters"
}

// checkLabelKey holds a label or annotation key to Kubernetes label
// syntax: an optional DNS subdomain prefix and a slash, then a name of at
// most 63 characters. The reserved prefix is the caller's question.
func checkLabelKey(k string) error {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		name = k
	} else if len(prefix) > maxLabelPrefix || !dnsSubdomain.MatchString(prefix) {
		return errString("the prefix before / is not a DNS subdomain")
	}
	if strings.Contains(name, "/") {
		return errString("a key has at most one /")
	}
	if len(name) > maxLabelKey || !labelName.MatchString(name) {
		return errString("the name is not [A-Za-z0-9._-] of at most 63 characters beginning and ending alphanumeric")
	}
	return nil
}

// checkLabelValue holds a label value to Kubernetes label syntax.
func checkLabelValue(v string) error {
	if len(v) > maxLabelKey || !labelValue.MatchString(v) {
		return errString("a label value is empty or [A-Za-z0-9._-] of at most 63 characters beginning and ending alphanumeric")
	}
	return nil
}

// checkHeaderName holds a header name to the token grammar of RFC 9110.
func checkHeaderName(h string) error {
	if !headerToken.MatchString(h) {
		return errString("a header name is a token of [!#$%&'*+.^_`|~0-9A-Za-z-]")
	}
	return nil
}

// checkHeaderValue holds a header value to visible ASCII and space, at
// most 4 KiB, no CR or LF.
func checkHeaderValue(v string) error {
	if len(v) > maxHeaderValue {
		return errString("a header value is at most 4096 bytes")
	}
	for i := range len(v) {
		if v[i] < 0x20 || v[i] > 0x7E {
			return errString("a header value is visible ASCII and space, with no control character, CR, or LF")
		}
	}
	return nil
}

// reservedHeaders are the names a Provider's static headers may not carry
// beside its own credential header: the framing headers and the
// hop-by-hop set of RFC 9110.
var reservedHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true, "keep-alive": true,
	"proxy-authenticate": true, "proxy-authorization": true, "te": true,
	"trailer": true, "transfer-encoding": true, "upgrade": true,
}

// checkEnvName holds a valueFrom.env to a POSIX variable name.
func checkEnvName(s string) error {
	if !posixName.MatchString(s) {
		return errString("a POSIX variable name is [A-Za-z_][A-Za-z0-9_]*")
	}
	return nil
}

// checkCurrency holds a currency to an upper-case ISO 4217 code.
func checkCurrency(s string) error {
	if !currencyCode.MatchString(s) {
		return errString("a currency is an upper-case ISO 4217 code of three letters")
	}
	return nil
}

// checkUpstreamName holds a target's model to 1 to 256 printable
// characters, the provider's own string and nothing more.
func checkUpstreamName(s string) error {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > maxUpstreamName {
		return errString("an upstream name is 1 to 256 characters")
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return errString("an upstream name is printable characters")
		}
	}
	return nil
}

// checkReference holds a reference to a Provider or a Budget to its
// shape: the kind's name rule or its prefixed id.
func checkReference(s string, id *regexp.Regexp, kind string) error {
	if id.MatchString(s) || validDNSLabel(s) {
		return nil
	}
	return errString("a " + kind + " is named by its name or its id")
}

// hostClass is what the upstream host rule makes of a host.
type hostClass int

const (
	hostPublic  hostClass = iota
	hostPrivate           // admitted only with AllowPrivateUpstreams, with a warning
	hostInvalid           // never admitted
)

// classifyHost applies the upstream host rule to a lower-case host: an IP
// literal or a fully qualified name is public; a loopback, link-local, or
// private address by syntax, a single-label name, and the .local and
// .internal suffixes are private; anything else is not a host.
func classifyHost(host string) (hostClass, string) {
	if ip, err := netip.ParseAddr(host); err == nil {
		switch {
		case ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
			return hostInvalid, "not a unicast address"
		case ip.IsLoopback():
			return hostPrivate, "a loopback address"
		case ip.IsLinkLocalUnicast():
			return hostPrivate, "a link-local address"
		case ip.IsPrivate():
			return hostPrivate, "a private address"
		default:
			return hostPublic, ""
		}
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) > maxLabelPrefix || !dnsSubdomain.MatchString(host) {
		return hostInvalid, "not a host name or an IP literal"
	}
	labels := strings.Split(host, ".")
	if len(labels) == 1 {
		return hostPrivate, "a single-label host"
	}
	switch tld := labels[len(labels)-1]; {
	case tld == "local" || tld == "internal":
		return hostPrivate, "a ." + tld + " name"
	case digits(tld):
		return hostInvalid, "not a host name or an IP literal"
	}
	return hostPublic, ""
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// checkBaseURL applies the baseURL rules: https, a host under the upstream
// host rule, an optional path, no userinfo, query, or fragment; http and a
// private host only with AllowPrivateUpstreams and then with a warning;
// the gateway's own host refused in both modes. It returns the warning
// text, if any, and the refusal.
func checkBaseURL(raw string, o Options) (string, *Error) {
	const path = "spec.baseURL"
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+" is not an absolute URL with a host", path)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+": the scheme is not https", path)
	}
	if u.User != nil {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+" carries userinfo", path)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+" carries a query", path)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+" carries a fragment", path)
	}
	host := strings.ToLower(u.Hostname())
	if o.PublicURL != nil && host == strings.ToLower(o.PublicURL.Hostname()) {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+" names this gateway's own host; a provider that is this gateway is a loop", path)
	}
	class, why := classifyHost(host)
	if class == hostInvalid {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+": "+why, path)
	}
	if scheme == "http" {
		class, why = hostPrivate, "a plaintext http:// address"+plus(class == hostPrivate, " on "+why)
	}
	if class == hostPublic {
		return "", nil
	}
	if !o.AllowPrivateUpstreams {
		return "", refuse(CodeInvalidField, strconv.Quote(raw)+" is "+why+", which needs LUX_UPSTREAM_ALLOW_PRIVATE", path)
	}
	return "The base URL is " + why + ", admitted because this gateway allows private upstreams.", nil
}

func plus(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}
