// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package check is the check role of luxd, the second role of the server
// binary in spec 017: it reads the whole configuration, prints one line
// per requirement of the installation in a fixed order, and exits 1 when
// any line failed. A line is ok, warn, or fail, the requirement's name,
// and one developer sentence; a warn never changes the exit code, and a
// requirement that cannot be checked because an earlier one failed is a
// warn saying so, so the failure is attributed once.
//
// The role dials the operator's endpoints and the providers and changes
// nothing: no object, no counter, no journal row, and no cache entry. The
// two things it puts anywhere are the empty archive object it deletes
// again and the check.ping the sink's type table tells a sink to ignore
// (spec 012). It is safe against a serving installation, and it runs as
// its own process, so over the memory store it sees none of the objects
// the serving replica holds and says so.
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/otel"
	"latere.ai/x/pkg/s3"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/tunnel"
	"latere.ai/x/lux/internal/version"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// State is a line's verdict.
type State string

// The three states. Only Fail changes the exit code.
const (
	OK   State = "ok"
	Warn State = "warn"
	Fail State = "fail"
)

// Line is one requirement's verdict: the state, the requirement's name,
// and one developer sentence.
type Line struct {
	State  State
	Name   string
	Detail string
}

// String renders the line as it prints: the state padded to four
// columns, the name, a colon, and the detail.
func (l Line) String() string { return fmt.Sprintf("%-4s %s: %s", l.State, l.Name, l.Detail) }

// Names lists every requirement in the order the lines print: the rows
// spec 017 owns, then the rows of specs 010, 005, and 013, as that spec
// orders them.
var Names = []string{
	"version", "configuration", "public url", "issuers", "authorizer", "events", "requestlog",
	"store", "migrations", "manifest dir", "db conns",
	"secrets kek", "credentials", "providers", "dialects",
	"tunnels",
}

// DefaultReplicas is the base Deployment's replica count of spec 017,
// which the db conns arithmetic multiplies by.
const DefaultReplicas = 2

// dialTimeout bounds one call to an operator's endpoint when Options
// names no client.
const dialTimeout = 10 * time.Second

// Options is what Run reads beside the environment. Every field but
// Getenv is optional and exists so a test can hold the dials.
type Options struct {
	// Getenv is the environment the configuration is read through.
	Getenv config.Getenv
	// HTTP dials the issuers, the authorizer, the sink, and the archive;
	// nil is an instrumented client with a ten second timeout.
	HTTP *http.Client
	// Replicas is the Deployment's replica count for the db conns
	// arithmetic; zero is DefaultReplicas.
	Replicas int
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// open replaces how the store is opened; the package's own tests seed
	// one. Nil is the store the configuration selects.
	open func(ctx context.Context, cfg config.Config, getenv config.Getenv) (opened, error)
}

// database is what the three rows over the Postgres store read: the
// readiness, the schema, and the cluster's connection limits, with the
// endpoint a line may print. *postgres.Store satisfies it, and the
// package's tests stand a fake in for it.
type database interface {
	Ready(ctx context.Context) error
	Schema(ctx context.Context) (postgres.Schema, error)
	ConnectionLimits(ctx context.Context) (maxConnections, reserved int, err error)
	Endpoint() string
}

// opened is what open answers: the store, the file mode's store when
// that is the mode, and the database when the Postgres store is.
type opened struct {
	st    store.Store
	files *filemode.Store
	db    database
}

// Run prints one line per requirement to out and returns 1 when any line
// failed and 0 otherwise.
func Run(ctx context.Context, o Options, out io.Writer) int {
	code := 0
	for _, l := range Lines(ctx, o) {
		_, _ = fmt.Fprintln(out, l)
		if l.State == Fail {
			code = 1
		}
	}
	return code
}

// run is one check's state: the configuration, the store it opened, and
// the Providers it read, shared by the rows.
type run struct {
	o     Options
	cfg   config.Config
	st    store.Store
	files *filemode.Store
	db    database
	// openErr is why the store did not open; storeErr is why the
	// Providers could not be read, the open failure included, and why is
	// the sentence the rows that needed them print.
	openErr   error
	storeErr  error
	why       string
	schema    postgres.Schema
	schemaErr error
	providers []*v1.Provider
}

// Lines runs every requirement in Names' order and returns the verdicts.
func Lines(ctx context.Context, o Options) []Line {
	if o.Replicas <= 0 {
		o.Replicas = DefaultReplicas
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: dialTimeout, Transport: otel.Transport(nil)}
	}
	if o.open == nil {
		o.open = openStore
	}
	lines := []Line{{OK, "version", version.String()}}
	cfg, err := config.Load(o.Getenv)
	if err != nil {
		lines = append(lines, Line{Fail, "configuration", strings.TrimPrefix(err.Error(), "configuration: ")})
		for _, name := range Names[2:] {
			lines = append(lines, notChecked(name, "the configuration did not load"))
		}
		return lines
	}
	lines = append(lines, Line{OK, "configuration", "every variable read; " + mode(cfg)})
	r := &run{o: o, cfg: cfg}
	got, err := o.open(ctx, cfg, o.Getenv)
	r.st, r.files, r.db, r.openErr, r.storeErr = got.st, got.files, got.db, err, err
	if r.st != nil {
		defer func() { _ = r.st.Close() }()
		if r.db != nil {
			// The schema decides whether the tables are there to read:
			// a database that does not answer, a schema not yet applied,
			// or one luxd serve would refuse leaves the rows over the
			// objects unchecked, and the store and migrations rows say
			// which it was.
			r.schema, r.schemaErr = r.db.Schema(ctx)
			r.why = schemaWhy(r.schema, r.schemaErr)
		}
		if r.why != "" {
			r.storeErr = errors.New(r.why)
		} else {
			r.providers, r.storeErr = listProviders(ctx, r.st)
		}
	}
	for _, row := range []func(context.Context) Line{
		r.publicURL, r.issuers, r.authorizer, r.events, r.requestLog,
		r.store, r.migrations, r.manifestDir, r.dbConns,
		r.secretsKEK, r.credentials, r.providerRow, r.dialects,
		r.tunnels,
	} {
		lines = append(lines, row(ctx))
	}
	return lines
}

// mode names where desired state comes from, for the configuration line.
func mode(cfg config.Config) string {
	switch {
	case cfg.ManifestDir != "":
		return "the file mode over " + cfg.ManifestDir
	case cfg.DBURL != "":
		return "the Postgres store"
	default:
		return "the memory store"
	}
}

// notChecked is the line of a requirement an earlier failure kept from
// running: a warn, so the failure counts once.
func notChecked(name, why string) Line {
	return Line{Warn, name, "not checked; " + why}
}

// openStore opens the store the configuration selects, as serve does
// but for the migrations, which check applies never: the Postgres store's
// pool over LUX_DB_URL, the file mode over LUX_MANIFEST_DIR, the memory
// store otherwise (spec 010).
func openStore(ctx context.Context, cfg config.Config, getenv config.Getenv) (opened, error) {
	switch {
	case cfg.DBURL != "":
		pg, err := postgres.Open(ctx, postgres.Options{URL: cfg.DBURL, MaxConns: cfg.DBMaxConns})
		if err != nil {
			return opened{}, err
		}
		return opened{st: pg, db: pg}, nil
	case cfg.ManifestDir != "":
		defaults := manifest.Defaults{RequestsPerMinute: cfg.DefaultRequestsPerMinute, TokensPerMinute: cfg.DefaultTokensPerMinute, Timeout: cfg.UpstreamTimeout}
		files, err := filemode.Load(ctx, filemode.Options{Dir: cfg.ManifestDir, Getenv: getenv, Defaults: defaults, AllowPrivateUpstreams: cfg.UpstreamAllowPrivate, PublicURL: cfg.PublicURL})
		if err != nil {
			return opened{}, err
		}
		return opened{st: files, files: files}, nil
	default:
		return opened{st: memory.New()}, nil
	}
}

// schemaWhy is the sentence the rows over the objects print when the
// Postgres schema keeps them from reading, and "" when the tables are
// there to read: the database did not answer, the schema is not applied
// yet, or luxd serve would refuse it.
func schemaWhy(s postgres.Schema, err error) string {
	if err != nil {
		return "the store did not answer"
	}
	action, why := postgres.Guard(s)
	switch {
	case action == postgres.Refuse:
		return "luxd serve would refuse the schema: " + why
	case s.Version == 0:
		return "the schema is not applied yet; luxd serve applies it at its first start"
	}
	return ""
}

// listProviders reads every Provider of the store, in name order.
func listProviders(ctx context.Context, st store.Store) ([]*v1.Provider, error) {
	var out []*v1.Provider
	cursor := ""
	for {
		objs, next, err := st.Objects().List(ctx, v1.KindProvider, store.Filter{}, store.Page{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("listing the Providers: %w", err)
		}
		for _, o := range objs {
			if p, ok := o.(*v1.Provider); ok {
				out = append(out, p)
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// storeWhy is the reason the rows over the store are not checked.
func (r *run) storeWhy() string {
	switch {
	case r.cfg.DBURL != "" && r.openErr != nil:
		return "the store did not open"
	case r.cfg.DBURL != "" && r.why != "":
		return r.why
	case r.cfg.ManifestDir != "":
		return "the manifest directory did not load"
	default:
		return "the Providers could not be read"
	}
}

// endpoint is the scheme and host of a URL, the part of an operator's
// endpoint a line may print: no path, no query, no userinfo.
func endpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// publicURL: LUX_PUBLIC_URL is absolute, and no Provider's baseURL names
// its host, which would loop a request back into the gateway (spec 003
// compares hostnames alone).
func (r *run) publicURL(context.Context) Line {
	const name = "public url"
	u := r.cfg.PublicURL
	if u == nil || !u.IsAbs() || u.Hostname() == "" {
		return Line{Fail, name, "LUX_PUBLIC_URL is not an absolute URL with a host"}
	}
	if r.storeErr != nil {
		return Line{Warn, name, u.String() + " is absolute; the Providers were not compared with it because " + r.storeWhy()}
	}
	var loops []string
	for _, p := range r.providers {
		if p.Spec.Tunnel {
			continue
		}
		if b, err := url.Parse(p.Spec.BaseURL); err == nil && strings.EqualFold(b.Hostname(), u.Hostname()) {
			loops = append(loops, p.Metadata.Name)
		}
	}
	if len(loops) > 0 {
		return Line{Fail, name, fmt.Sprintf("%s is the host of the baseURL of Provider %s too, so a request there would loop back into the gateway", u.Hostname(), strings.Join(loops, ", "))}
	}
	return Line{OK, name, fmt.Sprintf("%s is absolute and none of %d Provider(s) names its host", u, len(r.providers))}
}

// issuers: every LUX_OIDC_ISSUERS entry answers discovery and a key set
// with an RS256 or ES256 key, through the verifier serve starts with.
func (r *run) issuers(ctx context.Context) Line {
	const name = "issuers"
	if len(r.cfg.OIDCIssuers) == 0 {
		return Line{OK, name, "none; the file mode has no control plane bearer to verify"}
	}
	v, err := auth.NewVerifier(ctx, auth.VerifierOptions{Issuers: r.cfg.OIDCIssuers, Audience: r.cfg.OIDCAudience, HTTP: r.o.HTTP})
	if err != nil {
		return Line{Fail, name, strings.TrimPrefix(err.Error(), "LUX_OIDC_ISSUERS: ")}
	}
	return Line{OK, name, fmt.Sprintf("%d issuer(s) answer discovery and a key set with an RS256 or ES256 key: %s", len(v.Issuers()), strings.Join(v.Issuers(), ", "))}
}

// authorizerClient builds the client serve would, over the check's own
// HTTP client, so the probe travels as any decision does.
func (r *run) authorizerClient() (*authz.Client, error) {
	return authz.NewClient(authz.Options{
		URL: r.cfg.AuthorizerURL, Token: r.cfg.AuthorizerToken, HTTP: r.o.HTTP,
		Timeout: r.cfg.AuthorizerTimeout, Now: r.o.Now,
	})
}

// probe sends the probe every authorizer denies, authz.Probe of the
// action provider.read on the kind Provider carrying authz.ProbeID, and
// reads an allow as an endpoint that does not read the request. The
// client caches the deny for five seconds as it caches any deny, under
// the anonymous subject and the reserved id no object shares.
func probe(ctx context.Context, a authz.Authorizer, where string) Line {
	const name = "authorizer"
	err := auth.NewAuthorizer(a).Check(ctx)
	switch {
	case errors.Is(err, authz.ErrProbeAllowed):
		return Line{Fail, name, where + ": " + err.Error()}
	case err != nil:
		return Line{Fail, name, where + ": " + err.Error()}
	}
	return Line{OK, name, where + " denies the probe"}
}

// authorizer: the endpoint answers the probe inside LUX_AUTHORIZER_TIMEOUT
// with a decision, and denies it; the built-in owner policy is probed
// the same way when no endpoint is set.
func (r *run) authorizer(ctx context.Context) Line {
	const name = "authorizer"
	switch {
	case len(r.cfg.OIDCIssuers) == 0:
		return Line{OK, name, "none; the file mode authorizes nothing, since the directory is the desired state"}
	case r.cfg.AuthorizerURL == "":
		l := probe(ctx, &auth.OwnerPolicy{Admins: r.cfg.AdminSubjects}, fmt.Sprintf("the built-in owner policy with %d admin subject(s)", len(r.cfg.AdminSubjects)))
		return l
	}
	client, err := r.authorizerClient()
	if err != nil {
		return Line{Fail, name, err.Error()}
	}
	l := probe(ctx, client, endpoint(r.cfg.AuthorizerURL))
	if l.State == OK {
		l.Detail += " inside " + r.cfg.AuthorizerTimeout.String()
	}
	return l
}

// events: with LUX_EVENTS_URL set, the sink answers 2xx to one signed
// check.ping, computed as spec 012 says and never journalled.
func (r *run) events(ctx context.Context) Line {
	const name = "events"
	if r.cfg.EventsURL == "" {
		return Line{OK, name, "off; set LUX_EVENTS_URL and LUX_EVENTS_SECRET to deliver them"}
	}
	sink := events.NewSink(events.SinkOptions{URL: r.cfg.EventsURL, Secret: []byte(r.cfg.EventsSecret), Client: r.o.HTTP, Now: r.o.Now})
	if err := sink.Ping(ctx); err != nil {
		return Line{Fail, name, endpoint(r.cfg.EventsURL) + ": " + err.Error()}
	}
	return Line{OK, name, endpoint(r.cfg.EventsURL) + " acknowledged one signed check.ping"}
}

// requestLog: with the exporter s3, the bucket accepts and then deletes
// one empty object under LUX_S3_PREFIX, through the same client the
// exporter writes with.
func (r *run) requestLog(ctx context.Context) Line {
	const name = "requestlog"
	if r.cfg.RequestLogExporter != config.ExporterS3 {
		return Line{OK, name, "not archived; LUX_REQUESTLOG_EXPORTER is " + r.cfg.RequestLogExporter}
	}
	bucket, err := s3.New(r.cfg.S3Endpoint, r.cfg.S3Region, r.cfg.S3Bucket, r.cfg.S3AccessKey, r.cfg.S3SecretKey, s3.WithPathStyle(), s3.WithHTTPClient(r.o.HTTP))
	if err != nil {
		return Line{Fail, name, err.Error()}
	}
	where := fmt.Sprintf("bucket %s at %s", r.cfg.S3Bucket, endpoint(r.cfg.S3Endpoint))
	key := fmt.Sprintf("%scheck/%d", r.cfg.S3Prefix, r.o.Now().UnixNano())
	if _, err := bucket.PutObject(ctx, key, s3.BytesBody(nil)); err != nil {
		return Line{Fail, name, where + " refused an empty object at " + key + ": " + err.Error()}
	}
	if err := bucket.DeleteObject(ctx, key); err != nil {
		return Line{Fail, name, where + " accepted " + key + " and refused to delete it, so the object is left behind: " + err.Error()}
	}
	return Line{OK, name, where + " accepted and deleted one empty object under " + r.cfg.S3Prefix}
}

// store (spec 010): the mode is named; with Postgres the pool opens and
// SELECT 1 answers inside the readiness budget.
func (r *run) store(ctx context.Context) Line {
	const name = "store"
	switch {
	case r.cfg.DBURL != "" && r.db == nil:
		return Line{Fail, name, r.openErr.Error()}
	case r.cfg.DBURL != "":
		started := r.o.Now()
		if err := r.db.Ready(ctx); err != nil {
			return Line{Fail, name, err.Error()}
		}
		return Line{OK, name, fmt.Sprintf("the Postgres store at %s answered SELECT 1 in %s; desired state, spend windows, leases, and the journal are shared by every replica and survive a restart", r.db.Endpoint(), r.o.Now().Sub(started).Round(time.Millisecond))}
	case r.cfg.ManifestDir != "":
		return Line{OK, name, "the file mode: desired state is the directory " + r.cfg.ManifestDir + ", read-only through the API; spend windows, budgets, and leases are per replica"}
	default:
		return Line{Warn, name, memory.Notice + "; this check is its own process and reads none of the objects a serving replica holds"}
	}
}

// migrations (spec 010): the schema is not dirty and is of the binary's
// major; the line names the stored and the embedded highest version, ok
// when they agree or the stored one is behind, warn when it is ahead
// within the major, fail when dirty or of another major. Nothing is
// applied here.
func (r *run) migrations(context.Context) Line {
	const name = "migrations"
	switch {
	case r.cfg.DBURL == "":
		return Line{OK, name, "none; only the Postgres store has a schema to migrate"}
	case r.db == nil:
		return notChecked(name, r.storeWhy())
	case r.schemaErr != nil:
		return notChecked(name, "the store did not answer")
	}
	action, why := postgres.Guard(r.schema)
	switch action {
	case postgres.Refuse:
		return Line{Fail, name, why}
	case postgres.Serve:
		return Line{Warn, name, why}
	default:
		return Line{OK, name, why}
	}
}

// manifestDir (spec 010): in the file mode the directory is readable,
// every file resolves, and the line counts what was read per kind.
func (r *run) manifestDir(context.Context) Line {
	const name = "manifest dir"
	switch {
	case r.cfg.ManifestDir == "":
		return Line{OK, name, "unset; desired state comes from the API"}
	case r.files == nil:
		return Line{Fail, name, r.storeErr.Error()}
	}
	s := r.files.Summary()
	return Line{OK, name, fmt.Sprintf("%s: %d files read and resolved: %d providers, %d budgets, %d models, %d keys",
		s.Dir, s.Files, s.Kinds[v1.KindProvider], s.Kinds[v1.KindBudget], s.Kinds[v1.KindModel], s.Kinds[v1.KindKey])}
}

// dbConns (spec 010): the arithmetic spec 017's rollout rule rests on,
// LUX_DB_MAX_CONNS plus one per replica, times the Deployment's replicas,
// beside the cluster's max_connections less its superuser_reserved_connections:
// a warn when the replicas together exceed what is left, a fail when
// one replica alone does.
func (r *run) dbConns(ctx context.Context) Line {
	const name = "db conns"
	per := r.cfg.DBMaxConns + 1
	detail := fmt.Sprintf("LUX_DB_MAX_CONNS is %d, so each replica opens up to %d connections and the Deployment's %d replicas %d; a rollout surges no replica, so the store never sees more",
		r.cfg.DBMaxConns, per, r.o.Replicas, per*r.o.Replicas)
	switch {
	case r.cfg.DBURL == "":
		return Line{OK, name, detail + "; without LUX_DB_URL none is opened"}
	case r.db == nil:
		return notChecked(name, r.storeWhy())
	case r.schemaErr != nil:
		return notChecked(name, "the store did not answer")
	}
	maxConnections, reserved, err := r.db.ConnectionLimits(ctx)
	if err != nil {
		return Line{Fail, name, err.Error()}
	}
	usable := maxConnections - reserved
	detail += fmt.Sprintf("; the cluster's max_connections is %d with %d reserved for superusers, leaving %d", maxConnections, reserved, usable)
	switch {
	case per > usable:
		return Line{Fail, name, detail + "; one replica alone exceeds it, so no replica can open its pool"}
	case per*r.o.Replicas > usable:
		return Line{Warn, name, detail + "; the replicas together exceed it, so the last to start finds no slot: lower LUX_DB_MAX_CONNS or raise max_connections"}
	default:
		return Line{OK, name, detail}
	}
}

// secretsKEK (spec 005): LUX_SECRETS_KEK is set and every key decodes to
// 32 bytes, which the configuration already refused otherwise; the line
// counts the keys.
func (r *run) secretsKEK(context.Context) Line {
	const name = "secrets kek"
	switch {
	case r.cfg.SecretsKEK == nil:
		return Line{OK, name, "unset; the file mode seals nothing"}
	case r.cfg.ManifestDir != "":
		return Line{OK, name, fmt.Sprintf("%d key(s) of 32 bytes, read and unused in the file mode", r.cfg.SecretsKEK.Len())}
	default:
		return Line{OK, name, fmt.Sprintf("%d key(s) of 32 bytes; the first wraps every new credential and every one is tried to open a stored one", r.cfg.SecretsKEK.Len())}
	}
}

// credentials (spec 005): every stored credential's wrapped data key
// opens under a listed key; the line names the count and never a value.
func (r *run) credentials(ctx context.Context) Line {
	const name = "credentials"
	switch {
	case r.cfg.ManifestDir != "" && r.files != nil:
		return Line{OK, name, "read from the environment by the file mode; nothing is sealed"}
	case r.st == nil || r.storeErr != nil:
		return notChecked(name, r.storeWhy())
	}
	n, err := secrets.Check(ctx, r.st.Credentials(), r.cfg.SecretsKEK)
	if err != nil {
		return Line{Fail, name, err.Error()}
	}
	return Line{OK, name, fmt.Sprintf("%d stored row(s) open under the %d listed key(s)", n, r.cfg.SecretsKEK.Len())}
}

// credentialSource is the seam the probe reads a Provider's credential
// through: the file mode's values, or the sealed rows under the keys.
func (r *run) credentialSource() interface {
	Credential(ctx context.Context, providerID string) ([]byte, error)
} {
	if r.files != nil {
		return &serve.FileCredentials{Files: r.files}
	}
	return &serve.StoreCredentials{Credentials: r.st.Credentials(), Keys: r.cfg.SecretsKEK}
}

// providerRow (spec 005): every Provider's baseURL resolves, its address
// is admitted by the private-address rule, and its models route answers
// inside the probe budget, through the health job's own Probe over the
// gateway's client; a refused credential is a warn, a failure a fail.
func (r *run) providerRow(ctx context.Context) Line {
	const name = "providers"
	if r.st == nil || r.storeErr != nil {
		return notChecked(name, r.storeWhy())
	}
	var probed []*v1.Provider
	for _, p := range r.providers {
		if !p.Spec.Tunnel {
			probed = append(probed, p)
		}
	}
	if len(probed) == 0 {
		detail := "none declared"
		if r.cfg.ManifestDir == "" && r.cfg.DBURL == "" {
			detail += "; over the memory store a check process cannot read the serving replica's objects"
		}
		return Line{OK, name, detail}
	}
	health := serve.NewHealth(serve.HealthOptions{
		Clients:     gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: r.cfg.UpstreamAllowPrivate, Version: version.Version}),
		Credentials: r.credentialSource(),
		Logger:      slog.New(slog.DiscardHandler),
	})
	state := OK
	var parts []string
	for _, p := range probed {
		failed, lastError := health.Probe(ctx, p)
		switch {
		case failed:
			state = Fail
			parts = append(parts, p.Metadata.Name+": "+lastError)
		case lastError != "":
			if state == OK {
				state = Warn
			}
			parts = append(parts, p.Metadata.Name+": "+lastError)
		default:
			parts = append(parts, p.Metadata.Name+" reachable")
		}
	}
	return Line{state, name, strings.Join(parts, "; ")}
}

// dialects (spec 005): every Provider's dialect has the codec the doors
// translate through, or its Models are reached through its own door
// alone, which is the gemini case: llmdialect has no Gemini codec, so a
// gemini Provider answers the /gemini door and no other (spec 004).
func (r *run) dialects(context.Context) Line {
	const name = "dialects"
	if r.st == nil || r.storeErr != nil {
		return notChecked(name, r.storeWhy())
	}
	if len(r.providers) == 0 {
		return Line{OK, name, "no Providers to check"}
	}
	var gemini, translated []string
	for _, p := range r.providers {
		if p.Spec.Dialect == v1.DialectGemini {
			gemini = append(gemini, p.Metadata.Name)
		} else {
			translated = append(translated, p.Metadata.Name)
		}
	}
	detail := fmt.Sprintf("%d Provider(s) translate from every door through llmdialect", len(translated))
	if len(gemini) == 0 {
		return Line{OK, name, detail}
	}
	return Line{Warn, name, fmt.Sprintf("%s; gemini Provider(s) %s are reached through the /gemini door alone, since llmdialect has no Gemini codec", detail, strings.Join(gemini, ", "))}
}

// tunnels (spec 013): the tunnel is off, or on and every tunnelled
// Provider has a live registry row; with LUX_TUNNEL_FORWARD_ADDR set the
// forward route on that address accepts the configured secret; with
// LUX_DB_URL set and the address unset, tunnelled Providers serve on one
// replica, which is a warn.
func (r *run) tunnels(ctx context.Context) Line {
	const name = "tunnels"
	switch {
	case !r.cfg.TunnelEnabled:
		return Line{OK, name, "off; LUX_TUNNEL_ENABLED=1 serves the tunnel routes and admits spec.tunnel"}
	case r.cfg.ManifestDir != "":
		return Line{OK, name, "off in the file mode, which has no issuer to verify a session's bearer"}
	case r.st == nil || r.storeErr != nil:
		return notChecked(name, r.storeWhy())
	}
	var missing []string
	live := 0
	for _, p := range r.providers {
		if !p.Spec.Tunnel {
			continue
		}
		_, err := r.st.Tunnels().Get(ctx, p.Status.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			missing = append(missing, p.Metadata.Name)
		case err != nil:
			return Line{Fail, name, "reading the tunnel registry: " + err.Error()}
		default:
			live++
		}
	}
	if len(missing) > 0 {
		return Line{Fail, name, "no live session for tunnelled Provider " + strings.Join(missing, ", ") + "; its agent is not connected"}
	}
	detail := fmt.Sprintf("on; %d tunnelled Provider(s), every one with a live session", live)
	if r.cfg.TunnelForwardAddr == "" {
		if r.cfg.DBURL != "" {
			return Line{Warn, name, detail + "; tunnelled Providers serve on the holding replica alone, and an installation with more than one replica sets LUX_TUNNEL_FORWARD_ADDR"}
		}
		return Line{OK, name, detail}
	}
	if err := r.forwardRoute(ctx); err != nil {
		return Line{Fail, name, "LUX_TUNNEL_FORWARD_ADDR " + r.cfg.TunnelForwardAddr + ": " + err.Error()}
	}
	return Line{OK, name, detail + "; the forward route at " + r.cfg.TunnelForwardAddr + " accepts the configured secret"}
}

// forwardRoute asks this replica's own forward route, over plaintext
// HTTP/2 as another replica would, for a Provider id no object has, with
// the first configured secret. The expected answer is a refusal of the
// unknown Provider; a refused bearer or no answer at all is the failure.
func (r *run) forwardRoute(ctx context.Context) error {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Timeout: dialTimeout, Transport: otel.Transport(&http.Transport{Protocols: &protocols})}
	target := "http://" + r.cfg.TunnelForwardAddr + strings.Replace(tunnel.ForwardPattern, "{id}", v1.PrefixProvider+"00000000000000000000000000", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.TunnelForwardSecrets[0])
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the forward route answered %d, so the first entry of LUX_TUNNEL_FORWARD_SECRET is not one that replica accepts", resp.StatusCode)
	}
	return nil
}
