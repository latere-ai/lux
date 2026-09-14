// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package config reads the typed configuration of luxd from the
// environment. Every variable spec 002 names is read once at start-up, and
// a start-up with anything missing or malformed fails with one message that
// lists every problem, so an operator fixes a deployment in one round.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/lux/internal/secrets"
)

// Defaults for the optional variables.
const (
	DefaultPublicAddr   = ":8080"
	DefaultInternalAddr = ":8081"
	DefaultDBMaxConns   = 8
)

// The bounds of LUX_DB_MAX_CONNS. A managed cluster caps its connections
// in the low tens and a replica set multiplies whatever one process
// opens, so the ceiling is what one process could defensibly hold.
const (
	MinDBMaxConns = 1
	MaxDBMaxConns = 100
)

// Getenv is the environment lookup Load reads through, so a test passes a
// map and never touches the process environment.
type Getenv func(string) string

// Config is the resolved configuration. Field names follow the variable
// names in spec 002 without the LUX_ prefix.
type Config struct {
	// PublicAddr is where the /v1 API, the model traffic, and the public
	// probes listen.
	PublicAddr string
	// InternalAddr is where the four probes listen for the cluster.
	InternalAddr string
	// ManifestDir is the directory of manifests that is desired state in
	// the file mode of spec 010; empty is not the file mode.
	ManifestDir string
	// DBURL is the postgres:// URL of the Postgres store of spec 010;
	// empty is the memory store. It is never echoed, because it may carry
	// a password.
	DBURL string
	// DBMaxConns is the pool size with DBURL, DefaultDBMaxConns without.
	DBMaxConns int

	// The identity variables of spec 006.

	// OIDCIssuers are the issuer URLs whose tokens the control plane
	// accepts, each without its trailing slash. Empty in the file mode.
	OIDCIssuers []string
	// OIDCAudience is the one audience a caller token must contain.
	OIDCAudience string
	// OIDCInsecureIssuers are the issuers from the list that may use
	// http:// on a host other than loopback.
	OIDCInsecureIssuers []string
	// AuthorizerURL and AuthorizerToken are the operator's authorization
	// endpoint and its bearer; an empty URL selects the owner policy.
	AuthorizerURL   string
	AuthorizerToken string
	// AuthorizerTimeout bounds one decision, the retry included.
	AuthorizerTimeout time.Duration
	// AdminSubjects are the rendered subjects the owner policy lets act on
	// every object; read and unused when an authorizer is set.
	AdminSubjects []string

	// The provider variables of spec 005.

	// SecretsKEK is the parsed LUX_SECRETS_KEK: the first key wraps every
	// new data key and every key is tried to open one. Nil in the file
	// mode when the variable is unset, since nothing is sealed there.
	SecretsKEK *secrets.Keyring
	// UpstreamAllowPrivate admits a Provider base URL, and the address it
	// resolves to, on a loopback, link-local, or private network.
	UpstreamAllowPrivate bool
	// DiscoveryInterval is how often a Provider's model list is read.
	DiscoveryInterval time.Duration
	// HealthInterval is how often a Provider is probed.
	HealthInterval time.Duration

	// The key variables of spec 007.

	// KeyCache is how long a Key lookup, positive or negative, is cached
	// per replica on the data plane.
	KeyCache time.Duration
	// DefaultRequestsPerMinute and DefaultTokensPerMinute are the rates a
	// Key without limits gets at resolve; 0 is no limit.
	DefaultRequestsPerMinute int
	DefaultTokensPerMinute   int

	// The metering variable of spec 009.

	// MeteringFlush is how often a replica writes its spend deltas and
	// its usage aggregates to the store.
	MeteringFlush time.Duration

	// The control plane variables of spec 011, and the two of spec 004
	// its Resolve defaults and the doors' body cap read.

	// PublicURL is LUX_PUBLIC_URL: the absolute address callers reach the
	// public listener at, without a trailing slash; every URL in a
	// response and the loop check of Resolve are built from it.
	PublicURL *url.URL
	// RequestsPerMinute is the control plane rate per subject per
	// replica, which an authorizer's limits.requests_per_minute overrides
	// for that subject; 0 is no limit.
	RequestsPerMinute int
	// UnauthenticatedRequestsPerMinute is the rate per client address
	// before authentication, on both planes; 0 is no limit.
	UnauthenticatedRequestsPerMinute int
	// TrustedProxies are the ranges whose X-Forwarded-For names the
	// client; nil trusts no header.
	TrustedProxies []netip.Prefix
	// MaxManifestBytes is the largest control plane request body.
	MaxManifestBytes int64
	// MaxBodyBytes is LUX_MAX_BODY_BYTES, the doors' body cap.
	MaxBodyBytes int64
	// UpstreamTimeout is LUX_UPSTREAM_TIMEOUT, the Defaults.Timeout a
	// Provider without spec.timeout gets at resolve.
	UpstreamTimeout time.Duration

	// The event and request log variables of spec 012.

	// EventsURL is LUX_EVENTS_URL, the operator's event sink; empty is
	// events off. EventsSecret is LUX_EVENTS_SECRET, the HMAC-SHA256 key
	// of every delivery's Lux-Signature, required with the URL and never
	// echoed.
	EventsURL    string
	EventsSecret string
	// RequestLogExporter is LUX_REQUESTLOG_EXPORTER: none or s3.
	RequestLogExporter string
	// S3Endpoint, S3Region, S3Bucket, S3AccessKey, S3SecretKey, and
	// S3Prefix are the archive's LUX_S3_* rows; the endpoint, the bucket,
	// the access key, and the secret key are required with s3, the region
	// is us-east-1 and the prefix lux/ by default. The keys are never
	// echoed.
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Prefix    string

	// The tunnel variables of spec 013.

	// TunnelEnabled serves the tunnel routes and admits spec.tunnel.
	TunnelEnabled bool
	// TunnelRegistryTTL is the liveness window of a registry row; the
	// agent heartbeats at a third of it.
	TunnelRegistryTTL time.Duration
	// TunnelForwardAddr is the host:port other replicas reach this one's
	// internal listener at; empty serves a tunnelled Provider on the
	// holding replica only.
	TunnelForwardAddr string
	// TunnelForwardSecrets are the bearers of the forward route: the
	// first is sent, every one is accepted. Never echoed.
	TunnelForwardSecrets []string
}

// Load reads every variable through getenv and returns the configuration,
// or one error naming every problem found, sorted by variable name.
func Load(getenv Getenv) (Config, error) {
	c := Config{
		PublicAddr:   withDefault(getenv("LUX_PUBLIC_ADDR"), DefaultPublicAddr),
		InternalAddr: withDefault(getenv("LUX_INTERNAL_ADDR"), DefaultInternalAddr),
		ManifestDir:  strings.TrimSpace(getenv("LUX_MANIFEST_DIR")),
		DBURL:        strings.TrimSpace(getenv("LUX_DB_URL")),
		DBMaxConns:   DefaultDBMaxConns,
	}
	var problems []string
	if err := checkAddr(c.PublicAddr); err != nil {
		problems = append(problems, "LUX_PUBLIC_ADDR "+err.Error())
	}
	if err := checkAddr(c.InternalAddr); err != nil {
		problems = append(problems, "LUX_INTERNAL_ADDR "+err.Error())
	}
	if sameEndpoint(c.PublicAddr, c.InternalAddr) {
		problems = append(problems, "LUX_INTERNAL_ADDR must differ from LUX_PUBLIC_ADDR; both are "+c.PublicAddr)
	}
	if c.ManifestDir != "" {
		if err := checkDir(c.ManifestDir); err != nil {
			problems = append(problems, "LUX_MANIFEST_DIR "+err.Error())
		}
	}
	if c.DBURL != "" {
		if err := checkDBURL(c.DBURL); err != nil {
			problems = append(problems, "LUX_DB_URL "+err.Error())
		}
		// The pool size is read only with a database to size it for.
		if raw := strings.TrimSpace(getenv("LUX_DB_MAX_CONNS")); raw != "" {
			n, err := strconv.Atoi(raw)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("LUX_DB_MAX_CONNS is %q, not an integer", raw))
			case n < MinDBMaxConns || n > MaxDBMaxConns:
				problems = append(problems, fmt.Sprintf("LUX_DB_MAX_CONNS is %d, not between %d and %d", n, MinDBMaxConns, MaxDBMaxConns))
			default:
				c.DBMaxConns = n
			}
		}
	}
	// The two answer the same question, where desired state comes from,
	// and a precedence rule between them would be a silent decision
	// about whose manifests win.
	if c.ManifestDir != "" && c.DBURL != "" {
		problems = append(problems, "LUX_DB_URL and LUX_MANIFEST_DIR are both set; desired state comes from the directory or the database, not both")
	}
	problems = append(problems, c.loadIdentity(getenv)...)
	problems = append(problems, c.loadProviders(getenv)...)
	problems = append(problems, c.loadKeys(getenv)...)
	problems = append(problems, c.loadMetering(getenv)...)
	problems = append(problems, c.loadAPI(getenv)...)
	problems = append(problems, c.loadEvents(getenv)...)
	problems = append(problems, c.loadTunnel(getenv)...)
	if len(problems) > 0 {
		sort.Strings(problems)
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func withDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// sameEndpoint reports whether two valid addresses name one socket. Port
// 0 asks the kernel for any free port, so two ":0" addresses are two
// sockets and a test that binds both on loopback is not refused.
func sameEndpoint(a, b string) bool {
	if a != b {
		return false
	}
	_, port, err := net.SplitHostPort(a)
	return err == nil && port != "0"
}

// checkAddr accepts what net.Listen accepts for a TCP address: host:port
// with the host optional.
func checkAddr(addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("is %q, not a host:port address", addr)
	}
	return nil
}

// checkDir accepts a directory this process can list.
func checkDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("is %q, not a readable directory: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("is %q, not a directory", dir)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("is %q, not a readable directory: %w", dir, err)
	}
	return nil
}

// checkDBURL accepts a postgres:// or postgresql:// URL, the two
// spellings the driver takes, and echoes neither the value nor the
// parser's error, both of which would carry the password.
func checkDBURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("does not parse as a URL; the value is not echoed because it may carry a password")
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		return nil
	default:
		return fmt.Errorf("has scheme %q, not postgres", u.Scheme)
	}
}
