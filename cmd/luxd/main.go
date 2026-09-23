// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command luxd is the Lux gateway: it serves every model provider's API at
// one address, resolving a request against the manifest that declares the
// providers, models, keys, and budgets. This file is the entry point and
// holds wiring only: configuration, the listeners, and the run group. The
// behaviour lives in the packages under internal/ and in the exported
// packages at the module root.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/s3"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/api"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/bootstrap"
	"latere.ai/x/lux/internal/check"
	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/reqlog"
	"latere.ai/x/lux/internal/rewrap"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/store/postgres"
	"latere.ai/x/lux/internal/token"
	"latere.ai/x/lux/internal/tunnel"
	"latere.ai/x/lux/internal/version"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// Shutdown timing of spec 002: readiness answers 503 at once, the drain
// delay lets a load balancer notice, then the servers close with the
// grace period.
const (
	drainDelay  = 3 * time.Second
	gracePeriod = 60 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run dispatches the subcommand and returns the process exit code, so
// tests drive it without a subprocess: 0 on a clean stop, 1 on a start-up
// or runtime failure, 2 on a usage error. serve, check, rewrap, and token
// are the four roles of spec 002's table, one package each under
// internal/.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serveCmd(ctx, rest, getenv, stdout, stderr)
	case "check":
		return checkCmd(ctx, rest, getenv, stdout, stderr)
	case "rewrap":
		return rewrapCmd(ctx, rest, getenv, stdout, stderr)
	case "token":
		return tokenCmd(rest, getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "luxd: unknown subcommand %q; serve is the default, and check, rewrap, and token are the others\n", name)
		return 2
	}
}

// subcommand is spec 002's rule: the first argument that does not start
// with a dash names the subcommand, and the arguments around it are the
// subcommand's own.
func subcommand(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			return a, append(rest, args[i+1:]...)
		}
	}
	return "", args
}

// serveCmd is the node: the two listeners and the probes of spec 002,
// the store and the identity, the two jobs of spec 005, the Key cache
// and the Limiter of spec 007, the doors of spec 004 with the routing of
// spec 008, the control plane of spec 011, mounted per mode, and the
// two streams of spec 012 when the configuration names them.
func serveCmd(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("luxd serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		return fail(stderr, err)
	}

	// One metrics registry for the process, spec 019's: the store's, the
	// Key cache's, the router's, the doors', the authorizer's, and the
	// control plane's families land in it, and the internal listener
	// serves it at /metrics.
	reg := metrics.NewRegistry()
	st, files, notices, err := openStore(ctx, cfg, getenv, reg)
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = st.Close() }()
	for _, notice := range notices {
		_, _ = fmt.Fprintf(stdout, "luxd: %s\n", notice)
	}

	// The telemetry of spec 019: traces, metrics, and logs through
	// latere.ai/x/pkg/otel on the standard OTEL_* variables, exporting
	// only with OTEL_EXPORTER_OTLP_ENDPOINT set; the doors and the control
	// plane take their tracer from the provider it installs, and every
	// line is JSON on stderr with service, version, and replica, behind
	// the handler that truncates a Key value to its prefix before any
	// exporter sees it, and the process's default logger is that one. The
	// stop flushes every exporter after the listeners and the jobs have
	// stopped.
	logger, stopTelemetry := serve.Telemetry(ctx, version.Version, stderr)
	slog.SetDefault(logger)
	defer func() { _ = stopTelemetry(context.WithoutCancel(ctx)) }()

	// The Key cache of spec 007: the doors' lookup, invalidated by the
	// journal tail below, and emptied after a file-mode re-read, which
	// swaps the snapshot without a journal row.
	keys := serve.NewKeyCache(serve.KeyCacheOptions{Store: st, TTL: cfg.KeyCache, Metrics: reg, Logger: logger})
	if files != nil {
		stopHUP := reloadOnHUP(ctx, files, stdout, stderr, keys.Reset)
		defer stopHUP()
	}

	// Identity of spec 006: the issuers are fetched and checked once here,
	// so a deployment that cannot reach its issuer fails at start and not
	// at the first request.
	identity, err := auth.Startup(ctx, cfg, nil, serve.AuthorizerObserver(reg))
	if err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "luxd: %s\n", identity.String())

	// The resolver's defaults are spec 007's two rates and spec 004's
	// upstream timeout, shared by bootstrap, the Limiter, and the API's
	// Resolve.
	defaults := manifest.Defaults{RequestsPerMinute: cfg.DefaultRequestsPerMinute, TokensPerMinute: cfg.DefaultTokensPerMinute, Timeout: cfg.UpstreamTimeout}

	// Bootstrap of spec 035: the operator's own desired state, applied into
	// the store before the credentials are opened and before any listener,
	// so a gateway that starts serves what it was told to hold. A refusal
	// names the file, the object, and the code, and stops the start.
	if cfg.BootstrapDir != "" {
		res, err := bootstrap.Apply(ctx, bootstrap.Options{
			Dir: cfg.BootstrapDir, Store: st, Owner: cfg.AdminSubjects[0], Keys: cfg.SecretsKEK,
			Getenv: getenv, Defaults: defaults, AllowPrivateUpstreams: cfg.UpstreamAllowPrivate,
			TunnelEnabled: cfg.TunnelEnabled, PublicURL: cfg.PublicURL,
		})
		if err != nil {
			return fail(stderr, fmt.Errorf("LUX_BOOTSTRAP_DIR: %w", err))
		}
		for _, line := range res.Lines() {
			_, _ = fmt.Fprintf(stdout, "luxd: %s\n", line)
		}
		_, _ = fmt.Fprintf(stdout, "luxd: %s\n", res.Summary())
	}

	// Credential custody of spec 005: in server mode every stored row's
	// wrapped data key is opened here, so a deployment carrying the wrong
	// key fails at start and not at the first request through a door.
	credentials, notice, err := openCredentials(ctx, cfg, st, files)
	if err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "luxd: %s\n", notice)

	// The Limiter of spec 007 and the Recorder of spec 009 are the doors'
	// stage 7 and their record; both flush this replica's deltas to the
	// store every LUX_METERING_FLUSH, and once more at stop.
	limiter := serve.NewLimiter(serve.LimiterOptions{Store: st, Budgets: keys, Defaults: defaults, Flush: cfg.MeteringFlush, Logger: logger})

	// The request log of spec 012: with the exporter s3 every priced
	// record also goes to the archive's ring, written to the bucket in
	// NDJSON batches by the exporter's worker; with none nothing leaves
	// the process and GET /v1/requests reads this replica's memory.
	recorderOptions := serve.RecorderOptions{Store: st, Catalog: &serve.Catalog{Objects: st.Objects()}, Limiter: limiter, Metrics: reg, Flush: cfg.MeteringFlush, Logger: logger}
	var exporter *reqlog.Exporter
	var archive api.RecordLister
	if cfg.RequestLogExporter == config.ExporterS3 {
		bucket, err := s3.New(cfg.S3Endpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3AccessKey, cfg.S3SecretKey, s3.WithPathStyle(), s3.WithRetry(reqlog.WritePolicy))
		if err != nil {
			return fail(stderr, fmt.Errorf("LUX_S3_ENDPOINT: %w", err))
		}
		exporter = reqlog.NewExporter(reqlog.ExporterOptions{Bucket: bucket, Prefix: cfg.S3Prefix, Metrics: reg, Logger: logger})
		recorderOptions.Archive = exporter
		archive = reqlog.NewReader(bucket, cfg.S3Prefix)
		_, _ = fmt.Fprintf(stdout, "luxd: request log: archived to bucket %s at %s under %s, in batches of %d records or every %s\n", cfg.S3Bucket, cfg.S3Endpoint, cfg.S3Prefix, reqlog.FlushSize, reqlog.FlushInterval)
	} else {
		reqlog.RegisterIdle(reg)
		_, _ = fmt.Fprintln(stdout, "luxd: request log: not archived; GET /v1/requests reads this replica's memory")
	}
	recorder := serve.NewRecorder(recorderOptions)
	_, _ = fmt.Fprintf(stdout, "luxd: metering: spend counters and usage aggregates flush every %s\n", cfg.MeteringFlush)

	// The two jobs of spec 005, the Key cache's journal tail, and the two
	// metering flushes run for the life of the process and stop with it,
	// after the listeners have drained.
	clients := gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: cfg.UpstreamAllowPrivate, Version: version.Version})
	// The tunnel of spec 013 stands in front of the clients when it is
	// on: a tunnelled Provider is answered from its session and every
	// other Provider is the clients' own, so the doors and the jobs reach
	// both through one seam. A session that opens lists the Provider's
	// models and ticks health at once, so a laptop's models are callable
	// as soon as it attaches rather than at the next interval.
	var discovery *serve.Discovery
	var healthJob *serve.Health
	clientSource, revoker, tun, notice := composeTunnel(cfg, st, identity, clients, reg, logger, func(ctx context.Context, p *v1.Provider) {
		if p.Spec.Discovery.Mode == v1.DiscoveryAuto {
			discovery.List(ctx, p)
		}
		healthJob.Tick(ctx)
	})
	_, _ = fmt.Fprintf(stdout, "luxd: %s\n", notice)
	discovery = serve.NewDiscovery(serve.DiscoveryOptions{Store: st, Clients: clientSource, Credentials: credentials, Interval: cfg.DiscoveryInterval, Logger: logger})
	healthJob = serve.NewHealth(serve.HealthOptions{Store: st, Clients: clientSource, Credentials: credentials, Interval: cfg.HealthInterval, TunnelTTL: cfg.TunnelRegistryTTL, Metrics: reg, Logger: logger})
	jobsCtx, stopJobs := context.WithCancel(ctx)
	var jobs sync.WaitGroup
	jobs.Go(func() { discovery.Run(jobsCtx) })
	jobs.Go(func() { healthJob.Run(jobsCtx) })
	jobs.Go(func() { keys.Run(jobsCtx) })
	jobs.Go(func() { limiter.Run(jobsCtx) })
	jobs.Go(func() { recorder.Run(jobsCtx) })
	if exporter != nil {
		jobs.Go(func() { exporter.Run(jobsCtx) })
	}
	// The events of spec 012: every mutation and state change is
	// journalled whatever the configuration says; with a sink named the
	// delivery worker on the replica holding the journal lease posts each
	// row signed, at least once and in order per object.
	if cfg.EventsURL != "" {
		worker := events.NewWorker(events.WorkerOptions{
			Store: st, Metrics: reg, Logger: logger,
			Sink: events.NewSink(events.SinkOptions{URL: cfg.EventsURL, Secret: []byte(cfg.EventsSecret)}),
		})
		jobs.Go(func() { worker.Run(jobsCtx) })
		durability := "; the journal is this process's memory, and a restart loses what was not yet acknowledged"
		if cfg.DBURL != "" {
			durability = ""
		}
		_, _ = fmt.Fprintf(stdout, "luxd: events: delivered to %s%s\n", cfg.EventsURL, durability)
	} else {
		events.RegisterIdle(reg)
		_, _ = fmt.Fprintln(stdout, "luxd: events: off; set LUX_EVENTS_URL and LUX_EVENTS_SECRET to deliver them")
	}
	defer func() {
		stopJobs()
		jobs.Wait()
	}()

	// The doors of spec 004 over the seams of specs 005, 007, 008, and
	// 009: the Recorder prices every record and writes it.
	catalog := &serve.Catalog{Objects: st.Objects()}
	doors := gateway.New(gateway.Options{
		Keys:         keys,
		Catalog:      catalog,
		Credentials:  credentials,
		Router:       gateway.NewTargetRouter(gateway.RouterOptions{Catalog: catalog, Health: healthJob.View, Metrics: reg}),
		Limiter:      limiter,
		Recorder:     recorder,
		Clients:      clientSource,
		Health:       healthJob,
		Metrics:      reg,
		Version:      version.Version,
		MaxBodyBytes: cfg.MaxBodyBytes,
	})

	// The control plane of spec 011: on the public listener in server
	// mode, and on the internal listener alone in the file mode, where
	// the public listener answers not_found under /v1.
	control := api.New(api.Options{
		Store:                 st,
		Auth:                  identity,
		Authorizer:            identity.Authorizer(&serve.ObjectOwners{Objects: st.Objects()}),
		AuthorizeListItems:    cfg.AuthorizeListItems,
		PublicURL:             cfg.PublicURL,
		BasePath:              cfg.BasePath,
		Version:               version.Version,
		RequestsPerMinute:     cfg.RequestsPerMinute,
		TrustedProxies:        cfg.TrustedProxies,
		MaxManifestBytes:      cfg.MaxManifestBytes,
		Defaults:              defaults,
		AllowPrivateUpstreams: cfg.UpstreamAllowPrivate,
		TunnelEnabled:         tun != nil,
		Keys:                  cfg.SecretsKEK,
		Clients:               revoker,
		Tunnel:                tunnelRoutes(tun),
		ReadOnlyDir:           cfg.ManifestDir,
		Archive:               archive,
		Metrics:               reg,
		Logger:                logger,
	})

	draining := make(chan struct{})
	probes := health.Handler(health.Options{
		Ready: health.Checks(
			health.Check{Name: "draining", Run: notDraining(draining)},
			health.Check{Name: "store", Run: st.Ready},
		),
		Timeout:   2 * time.Second,
		Version:   version.Version,
		Commit:    version.Commit,
		BuildTime: version.Date,
		Metrics:   serve.Metrics(reg),
	})

	// The public listener: the three probes and the build identity at /,
	// answered whatever a caller's bucket says, and the two planes, the
	// four doors, /.well-known/lux, and /v1 in server mode, behind one
	// per-address bucket of spec 011, one instance so a refused
	// credential on either plane draws from the same bucket.
	planes := http.NewServeMux()
	for _, d := range []string{"openai", "anthropic", "gemini", "lux"} {
		planes.Handle("/"+d, doors)
		planes.Handle("/"+d+"/", doors)
	}
	planes.Handle("/.well-known/lux", control)
	// The internal listener: the four probes, /metrics, and /v1 in the
	// file mode.
	internal := http.NewServeMux()
	internal.Handle("/", probes)
	// The forward route of spec 013 is on the internal listener alone,
	// and only when this replica advertises an address other replicas
	// reach it at; without one, no replica would forward here.
	if tun != nil && cfg.TunnelForwardAddr != "" {
		internal.Handle(tunnel.ForwardPattern, tun.Forward())
	}
	if files == nil {
		planes.Handle("/v1/", control)
		_, _ = fmt.Fprintf(stdout, "luxd: control plane at %s/v1 on the public listener\n", cfg.PublicURL)
	} else {
		planes.Handle("/v1/", control.Unmounted())
		internal.Handle("/v1/", control)
		_, _ = fmt.Fprintln(stdout, "luxd: control plane read-only on the internal listener; the public listener answers not_found under /v1")
	}
	limited := serve.LimitUnauthenticated(planes, serve.AddressLimiterOptions{PerMinute: cfg.UnauthenticatedRequestsPerMinute, Trusted: cfg.TrustedProxies})
	public := http.NewServeMux()
	for _, p := range []string{"/livez", "/readyz", "/version"} {
		public.Handle("GET "+p, probes)
	}
	public.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, version.String())
	})
	for _, prefix := range []string{"/openai", "/anthropic", "/gemini", "/lux", "/v1", "/.well-known/lux"} {
		public.Handle(prefix, limited)
		public.Handle(prefix+"/", limited)
	}

	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", cfg.PublicAddr)
	if err != nil {
		return fail(stderr, fmt.Errorf("LUX_PUBLIC_ADDR: %w", err))
	}
	internalLn, err := lc.Listen(ctx, "tcp", cfg.InternalAddr)
	if err != nil {
		_ = publicLn.Close()
		return fail(stderr, fmt.Errorf("LUX_INTERNAL_ADDR: %w", err))
	}
	_, _ = fmt.Fprintf(stdout, "luxd: %s listening public=%s internal=%s\n",
		version.Version, publicLn.Addr(), internalLn.Addr())
	if cfg.BasePath != "" {
		_, _ = fmt.Fprintf(stdout, "luxd: the public listener answers under %s\n", cfg.BasePath)
	}

	servers := []*http.Server{
		{Handler: mountAt(cfg.BasePath, public), ReadHeaderTimeout: 10 * time.Second},
		{Handler: internal, ReadHeaderTimeout: 10 * time.Second},
	}
	if tun != nil {
		// The tunnel needs HTTP/2, and a plaintext listener behind an
		// ingress that terminates TLS, or on a developer's machine, gets
		// it unencrypted: the session and the carriers on the public
		// listener, the forward hop on the internal one (spec 013).
		for _, s := range servers {
			s.Protocols = new(http.Protocols)
			s.Protocols.SetHTTP1(true)
			s.Protocols.SetUnencryptedHTTP2(true)
		}
	}
	errc := make(chan error, len(servers))
	for i, ln := range []net.Listener{publicLn, internalLn} {
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(servers[i], ln)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fail(stderr, err)
	}
	// The stop signal has fired, so the shutdown runs on a context that
	// keeps the request's values and outlives its cancellation.
	close(draining)
	stopping := context.WithoutCancel(ctx)
	if tun != nil {
		// Every agent is told to reconnect at once and lands on another
		// replica, before the listeners close under it.
		tun.Drain(stopping)
	}
	sleepCtx(stopping, drainDelay)
	shutdownCtx, cancel := context.WithTimeout(stopping, gracePeriod)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return 0
}

// openStore constructs the store the configuration selects, wrapped in
// Instrument on the process's registry, and returns the file mode's
// store beside it when that is the mode, so serve can wire the re-read,
// and the start-up lines that name the mode. With LUX_DB_URL the
// Postgres store of spec 010 opens its pool, holds the schema to the
// guards, applies the migrations that are missing, and adds one WARN
// line when the schema is ahead of this build within its major; a
// database that does not answer, a dirty schema, or one of another
// major is the start-up failure.
func openStore(ctx context.Context, cfg config.Config, getenv config.Getenv, reg *metrics.Registry) (store.Store, *filemode.Store, []string, error) {
	switch {
	case cfg.DBURL != "":
		pg, warning, err := postgres.Connect(ctx, postgres.Options{URL: cfg.DBURL, PoolURL: cfg.DBPoolURL, MaxConns: cfg.DBMaxConns})
		if err != nil {
			return nil, nil, nil, err
		}
		notices := []string{pg.Notice(ctx)}
		if warning != "" {
			notices = append(notices, "WARN "+warning)
		}
		return store.Instrument(pg, reg), nil, notices, nil
	case cfg.ManifestDir != "":
		// The resolver's defaults are spec 007's two rates and spec 004's
		// upstream timeout, so a Key the directory declares without limits
		// gets the operator's and a Provider without a timeout the same;
		// the public URL is the loop check's, as through the API.
		defaults := manifest.Defaults{RequestsPerMinute: cfg.DefaultRequestsPerMinute, TokensPerMinute: cfg.DefaultTokensPerMinute, Timeout: cfg.UpstreamTimeout}
		files, err := filemode.Load(ctx, filemode.Options{Dir: cfg.ManifestDir, Getenv: getenv, Defaults: defaults, AllowPrivateUpstreams: cfg.UpstreamAllowPrivate, PublicURL: cfg.PublicURL})
		if err != nil {
			return nil, nil, nil, err
		}
		return store.Instrument(files, reg), files, []string{files.Notice()}, nil
	default:
		return store.Instrument(memory.New(), reg), nil, []string{memory.Notice}, nil
	}
}

// credentialSource is the seam the jobs and, with spec 004, the doors
// take a Provider's credential through: the two types of internal/serve.
type credentialSource interface {
	Credential(ctx context.Context, providerID string) ([]byte, error)
}

// openCredentials is the credential source of the mode and the start-up
// check of spec 005: in file mode the values read from the environment,
// with LUX_SECRETS_KEK read and unused; otherwise the sealed rows under
// the keys, every one of which is opened here, and the Providers whose
// row no listed key opens are the start-up failure.
func openCredentials(ctx context.Context, cfg config.Config, st store.Store, files *filemode.Store) (credentialSource, string, error) {
	if files != nil {
		return &serve.FileCredentials{Files: files}, "credentials: read from the environment by the file mode; nothing is sealed", nil
	}
	n, err := secrets.Check(ctx, st.Credentials(), cfg.SecretsKEK)
	if err != nil {
		return nil, "", fmt.Errorf("LUX_SECRETS_KEK: %w", err)
	}
	return &serve.StoreCredentials{Credentials: st.Credentials(), Keys: cfg.SecretsKEK},
		fmt.Sprintf("credentials: %d key(s) in LUX_SECRETS_KEK, %d stored row(s) open under them", cfg.SecretsKEK.Len(), n), nil
}

// composeTunnel is the wiring of spec 013. With LUX_TUNNEL_ENABLED in
// server mode the tunnel Gateway stands in front of spec 005's clients
// and is the client source the doors and the jobs dial through, the
// revoker the API tells of a deleted Provider, and the handler behind
// the tunnel routes and the forward listener; otherwise the clients
// serve both seams and there is no Gateway. In the file mode there is
// no issuer to verify a session's bearer, so the tunnel stays off; off
// either way the sessions gauge of spec 019 is registered at zero, so
// the metric is in the registry whether or not the tunnel is on. The
// returned notice is the start-up line: it names the forward address or
// says tunnelled Providers serve on the holding replica alone, which an
// installation past one replica reads as the cause of an intermittent
// provider_unavailable.
func composeTunnel(cfg config.Config, st store.Store, identity *auth.Auth, clients *gateway.Clients, reg *metrics.Registry, logger *slog.Logger, onConnect func(context.Context, *v1.Provider)) (gateway.ClientSource, api.ClientRevoker, *tunnel.Gateway, string) {
	switch {
	case !cfg.TunnelEnabled:
		tunnel.RegisterIdle(reg)
		return clients, clients, nil, "tunnel: off; LUX_TUNNEL_ENABLED=1 serves the tunnel routes and admits spec.tunnel"
	case identity.Verifier == nil:
		tunnel.RegisterIdle(reg)
		return clients, clients, nil, "tunnel: off in the file mode, which has no issuer to verify a session's bearer"
	}
	tun := tunnel.New(tunnel.Options{
		Store: st, Verifier: identity.Verifier, Clients: clients,
		Replica: cfg.TunnelForwardAddr, Secrets: cfg.TunnelForwardSecrets, TTL: cfg.TunnelRegistryTTL,
		Version: version.Version, Metrics: reg, Logger: logger, OnConnect: onConnect,
	})
	notice := fmt.Sprintf("tunnel: on, registry TTL %s", cfg.TunnelRegistryTTL)
	if cfg.TunnelForwardAddr == "" {
		notice += "; tunnelled Providers are served by the holding replica only, and an installation with more than one replica sets LUX_TUNNEL_FORWARD_ADDR"
		if cfg.DBURL != "" {
			notice = "WARN " + notice
		}
		return tun, tun, tun, notice
	}
	return tun, tun, tun, notice + fmt.Sprintf("; other replicas forward to this one at %s with %d secret(s)", cfg.TunnelForwardAddr, len(cfg.TunnelForwardSecrets))
}

// tunnelRoutes is the Gateway as the API's option, or a nil interface
// when there is none, which the API reads as the tunnel off.
func tunnelRoutes(tun *tunnel.Gateway) api.TunnelRoutes {
	if tun == nil {
		return nil
	}
	return tun
}

// checkCmd is the check role of spec 017: it reads the whole configuration,
// prints one line per requirement of the installation on stdout in the
// order internal/check fixes, and exits 1 when any line failed. It dials
// the operator's endpoints and the providers and changes nothing, so it
// is safe against a serving installation.
func checkCmd(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("luxd check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return check.Run(ctx, check.Options{Getenv: getenv}, stdout)
}

// rewrapCmd is the rewrap role of spec 005: it reads LUX_SECRETS_KEK and
// LUX_DB_URL and nothing else of the table, opens the Postgres store
// without applying a migration, refuses a schema that is not at this
// build's highest version, and re-wraps every stored credential's data
// key under the first key, printing one summary line and one line per
// row no listed key opens. Exit 1 when a row could not be re-wrapped;
// without LUX_DB_URL the role has nothing durable to re-wrap.
func rewrapCmd(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("luxd rewrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r, err := config.LoadRewrap(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	pg, err := postgres.Open(ctx, postgres.Options{URL: r.DBURL, PoolURL: r.DBPoolURL, MaxConns: 2})
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = pg.Close() }()
	schema, err := pg.Schema(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("LUX_DB_URL: the database at %s did not answer: %w", pg.Endpoint(), err))
	}
	switch {
	case schema.Dirty:
		return fail(stderr, fmt.Errorf("LUX_DB_URL: the schema is dirty at version %d; rewrap applies no migration, and a dirty schema is an operator's to repair", schema.Version))
	case schema.Version != postgres.Highest:
		return fail(stderr, fmt.Errorf("LUX_DB_URL: the schema is at version %d and this build's is %d; rewrap applies no migration and runs against the schema it was built with, so run luxd serve of this build first, or this build's rewrap against the schema it wrote", schema.Version, postgres.Highest))
	}
	sum, err := rewrap.Run(ctx, pg.Credentials(), r.SecretsKEK, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintf(stdout, "luxd: %s\n", sum)
	if sum.Failed() {
		return 1
	}
	return 0
}

// tokenCmd is the token role of spec 035: it signs one control plane
// token with LUX_LOCAL_ISSUER_KEY and prints it on stdout followed by a
// newline and nothing else, so LUX_TOKEN=$(luxd token) is one line of a
// script. It reads LUX_PUBLIC_URL, LUX_LOCAL_ISSUER_KEY,
// LUX_OIDC_AUDIENCE, and LUX_ADMIN_SUBJECTS and nothing else of the
// table, and writes nothing. A bad flag, a TTL out of its range, a
// missing subject, an unlisted audience, and an unset key are usage
// errors, exit 2; a variable it reads that does not parse is exit 1.
func tokenCmd(args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("luxd token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	subject := fs.String("subject", "", "the token's sub; the default is the sub of the one LUX_ADMIN_SUBJECTS entry whose issuer is LUX_PUBLIC_URL")
	audience := fs.String("audience", "", "a name of LUX_OIDC_AUDIENCE; the default is the first")
	ttl := fs.Duration("ttl", token.DefaultTTL, "the token's lifetime, above zero and at most "+token.MaxTTL.String())
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "luxd token: unexpected argument %q; the token role takes --subject, --audience, and --ttl\n", fs.Arg(0))
		return 2
	}
	minted, err := token.Mint(token.Options{Getenv: getenv, Subject: *subject, Audience: *audience, TTL: *ttl})
	switch {
	case token.IsUsage(err):
		_, _ = fmt.Fprintf(stderr, "luxd token: %v\n", err)
		return 2
	case err != nil:
		return fail(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, minted)
	return 0
}

// reloadOnHUP re-reads the directory on every SIGHUP until ctx ends or
// the returned stop is called, printing what was read, or the file and
// the path that stopped the read while the previous snapshot keeps
// serving. afterReload runs after a successful re-read: the Key cache's
// Reset, because the swap writes no journal row for the tail to see.
func reloadOnHUP(ctx context.Context, files *filemode.Store, stdout, stderr io.Writer, afterReload func()) (stop func()) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-quit:
				return
			case <-hup:
				if err := files.Reload(ctx); err != nil {
					_, _ = fmt.Fprintf(stderr, "luxd: reload failed, the previous snapshot keeps serving: %v\n", err)
					continue
				}
				afterReload()
				_, _ = fmt.Fprintf(stdout, "luxd: reloaded %s\n", files.Notice())
			}
		}
	}()
	return func() {
		signal.Stop(hup)
		close(quit)
		<-done
	}
}

// fail writes the one line an operator reads on a start-up or runtime
// failure and returns exit code 1.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "luxd: %v\n", err)
	return 1
}

// notDraining fails readiness once shutdown has begun, so a load balancer
// stops routing before the servers close. The store's Ready is the other
// readiness check, named store, and is nil for the memory and file modes.
func notDraining(draining <-chan struct{}) func(context.Context) error {
	return func(context.Context) error {
		select {
		case <-draining:
			return errors.New("shutting down")
		default:
			return nil
		}
	}
}

// sleepCtx waits d or until ctx ends, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
