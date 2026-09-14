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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"latere.ai/x/pkg/health"
	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/version"
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
// or runtime failure, 2 on a usage error. serve is the only subcommand
// today; spec 002 keeps the table.
func run(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	name, rest := subcommand(args)
	switch name {
	case "", "serve":
		return serve(ctx, rest, getenv, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "luxd: unknown subcommand %q; serve is the default and the only one\n", name)
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

// serve is the node: the two listeners and the probes of spec 002. The
// dialect doors, the API, and the routing of later specs mount here.
func serve(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
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

	st, files, notice, err := openStore(ctx, cfg, getenv)
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = st.Close() }()
	_, _ = fmt.Fprintf(stdout, "luxd: %s\n", notice)
	if files != nil {
		stopHUP := reloadOnHUP(ctx, files, stdout, stderr)
		defer stopHUP()
	}

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
	})

	public := http.NewServeMux()
	for _, p := range []string{"/livez", "/readyz", "/version"} {
		public.Handle("GET "+p, probes)
	}
	public.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, version.String())
	})

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

	servers := []*http.Server{
		{Handler: public, ReadHeaderTimeout: 10 * time.Second},
		{Handler: probes, ReadHeaderTimeout: 10 * time.Second},
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
	sleepCtx(stopping, drainDelay)
	shutdownCtx, cancel := context.WithTimeout(stopping, gracePeriod)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return 0
}

// openStore constructs the store the configuration selects, wrapped in
// Instrument, and returns the file mode's store beside it when that is
// the mode, so serve can wire the re-read, and the substance of the one
// start-up line that names the mode. The Postgres store lands in a later
// phase; until then a configured LUX_DB_URL is refused rather than
// silently answered with state in memory, because an operator who asked
// for durability and got a process's memory would find out at the first
// restart.
func openStore(ctx context.Context, cfg config.Config, getenv config.Getenv) (store.Store, *filemode.Store, string, error) {
	reg := metrics.NewRegistry()
	switch {
	case cfg.DBURL != "":
		return nil, nil, "", errors.New("LUX_DB_URL: the Postgres store is not in this build; unset it to hold state in memory, or set LUX_MANIFEST_DIR to read manifests from a directory")
	case cfg.ManifestDir != "":
		files, err := filemode.Load(ctx, filemode.Options{Dir: cfg.ManifestDir, Getenv: getenv})
		if err != nil {
			return nil, nil, "", err
		}
		return store.Instrument(files, reg), files, files.Notice(), nil
	default:
		return store.Instrument(memory.New(), reg), nil, memory.Notice, nil
	}
}

// reloadOnHUP re-reads the directory on every SIGHUP until ctx ends or
// the returned stop is called, printing what was read, or the file and
// the path that stopped the read while the previous snapshot keeps
// serving.
func reloadOnHUP(ctx context.Context, files *filemode.Store, stdout, stderr io.Writer) (stop func()) {
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
