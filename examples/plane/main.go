// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command plane is the example of docs/plane.md's second door: a
// platform's own binary, built from the three packages this repository
// exports, with a store, an identity, and a control plane of its own.
// It imports manifest for the contract, gateway for the data plane, and
// metering for the windows and the ledger, and nothing under internal/,
// which is exactly what a platform outside this repository can do.
//
//	go run ./examples/plane \
//	  -addr 127.0.0.1:8080 -public-url http://127.0.0.1:8080 \
//	  -issuer https://login.example.com -audience lux \
//	  -authorizer http://127.0.0.1:8081 -authorizer-token "$AUTHORIZER_TOKEN"
//
// It is held to the contract by TestExamplePlaneConforms, which runs
// the conformance suite of test/conformance against it, so what this
// file demonstrates is a front a platform's own suite would pass.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"latere.ai/x/lux/manifest"
)

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stderr)) }

// run serves until the context ends or the process is asked to stop,
// and returns the exit code: 0 on a clean stop, 1 when the front cannot
// be built or served, 2 on a usage error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("plane", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8080", "the address to serve on")
	public := fs.String("public-url", "", "the address callers reach this front at; the listener's by default")
	issuer := fs.String("issuer", "", "the platform's OIDC issuer, whose tokens open the control plane")
	audience := fs.String("audience", "lux", "the audience a caller's token must carry")
	authorizer := fs.String("authorizer", "", "the platform's permission endpoint")
	token := fs.String("authorizer-token", os.Getenv("AUTHORIZER_TOKEN"), "the bearer the permission endpoint requires")
	private := fs.Bool("allow-private-upstreams", false, "admit a Provider on a loopback or private address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *issuer == "" || *authorizer == "" {
		_, _ = fmt.Fprintln(stderr, "plane: -issuer and -authorizer are required; this front verifies its own callers and decides nothing itself")
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	var listener net.ListenConfig
	ln, err := listener.Listen(ctx, "tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "plane:", err)
		return 1
	}
	if *public == "" {
		*public = "http://" + ln.Addr().String()
	}
	publicURL, err := url.Parse(*public)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "plane: -public-url:", err)
		return 1
	}
	p, err := New(ctx, Options{
		PublicURL: publicURL, IssuerURL: *issuer, Audience: *audience,
		AuthorizerURL: *authorizer, AuthorizerToken: *token,
		Defaults:              manifest.Defaults{Timeout: 10 * time.Minute},
		AllowPrivateUpstreams: *private, RequestsPerMinute: 6000, Version: "example",
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "plane:", err)
		return 1
	}
	go p.Run(ctx)
	_, _ = fmt.Fprintf(stderr, "plane: serving %s\n", *public)
	server := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(ln)
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(stderr, "plane:", err)
		return 1
	}
	return 0
}
