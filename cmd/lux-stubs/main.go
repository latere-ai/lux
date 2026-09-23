// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command lux-stubs serves every stub of spec 015 on one loopback address
// each: a stub provider per dialect, the stub issuer and the stub
// authorizer of latere.ai/x/pkg, the stub sink, and the index, whose
// GET / names them all for a caller handed one address. It prints one
// line per stub with its URL and exits on SIGTERM. It is wiring only: the
// behavior lives in the packages under test/stubs and in pkg. It runs
// in tests, under make run, and beside the conformance suite, and never
// in an installation.
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
	"syscall"
	"time"

	"latere.ai/x/pkg/authz/stub"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/test/stubs/authorizer"
	"latere.ai/x/lux/test/stubs/index"
	"latere.ai/x/lux/test/stubs/issuer"
	"latere.ai/x/lux/test/stubs/provider"
	"latere.ai/x/lux/test/stubs/sink"
)

// names are the stubs in the order their lines are printed: the four
// providers by dialect, then the issuer, the authorizer, the sink, and
// the index, whose document names the six before it.
var names = []string{"openai", "anthropic", "gemini", "lux", "issuer", "authorizer", "sink", "index"}

// dialects are the four the binary starts a stub provider for, in the
// order their lines are printed.
var dialects = []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux}

// shutdownGrace bounds the drain at stop.
const shutdownGrace = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run starts every stub and serves until ctx ends: 0 on a clean stop, 1
// on a start-up failure, 2 on a usage error.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lux-stubs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addrs := map[string]*string{}
	for _, name := range names {
		addrs[name] = fs.String(name+"-addr", "127.0.0.1:0", "the address the "+name+" stub listens at")
	}
	credential := fs.String("credential", provider.DefaultCredential, "the credential every stub provider requires")
	issuerURL := fs.String("issuer-url", "", "the URL the other processes reach the issuer at; the default is its own listener")
	es256 := fs.Bool("es256", false, "sign with ES256 instead of RS256")
	authorizerToken := fs.String("authorizer-token", stub.DefaultToken, "the bearer the authorizer requires; LUX_AUTHORIZER_TOKEN takes it")
	authorizerDeny := fs.String("authorizer-deny", "", "an action the authorizer denies for every subject")
	authorizerFail := fs.String("authorizer-fail", "", "an outage: an HTTP status, malformed, no-allow, or hang")
	sinkSecret := fs.String("sink-secret", sink.DefaultSecret, "the HMAC key the sink verifies with; LUX_EVENTS_SECRET takes it")
	failFirst := fs.Int("fail-first", 0, "how many deliveries the sink refuses with 500 before storing one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "lux-stubs: unexpected argument %q; the binary takes flags alone\n", fs.Arg(0))
		return 2
	}

	// Every address is bound before anything serves, so a taken port
	// fails the start rather than one stub of the set.
	var lc net.ListenConfig
	listeners := map[string]net.Listener{}
	for _, name := range names {
		ln, err := lc.Listen(ctx, "tcp", *addrs[name])
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return fail(stderr, fmt.Errorf("-%s-addr: %w", name, err))
		}
		listeners[name] = ln
	}
	urls := map[string]string{}
	for name, ln := range listeners {
		urls[name] = "http://" + ln.Addr().String()
	}
	if *issuerURL == "" {
		*issuerURL = urls["issuer"]
	}

	handlers := map[string]http.Handler{}
	doc := index.Document{Providers: map[string]string{}, Issuer: *issuerURL, Authorizer: urls["authorizer"], Sink: urls["sink"], Credential: *credential}
	for _, d := range dialects {
		doc.Providers[string(d)] = urls[string(d)]
	}
	handlers["index"] = index.New(doc)
	for _, d := range dialects {
		handlers[string(d)] = provider.New(provider.Options{Dialect: d, Credential: *credential})
	}
	iss := issuer.NewHandler(*issuerURL, *es256)
	handlers["issuer"] = iss.Handler()
	authz := authorizer.NewHandler(stub.WithToken(*authorizerToken))
	authorizer.Deny(authz, *authorizerDeny)
	if err := authorizer.Fail(authz, *authorizerFail); err != nil {
		for _, ln := range listeners {
			_ = ln.Close()
		}
		return fail(stderr, err)
	}
	handlers["authorizer"] = authz.Handler()
	handlers["sink"] = sink.New(sink.Options{Secret: []byte(*sinkSecret), FailFirst: *failFirst})

	servers := map[string]*http.Server{}
	errc := make(chan error, len(names))
	for _, name := range names {
		srv := &http.Server{Handler: handlers[name], ReadHeaderTimeout: 10 * time.Second}
		servers[name] = srv
		go func(name string) {
			if err := srv.Serve(listeners[name]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}(name)
		_, _ = fmt.Fprintf(stdout, "lux-stubs: %s %s\n", name, urls[name])
	}
	_, _ = fmt.Fprintf(stdout, "lux-stubs: issuer url %s\n", *issuerURL)
	_, _ = fmt.Fprintln(stdout, "lux-stubs: ready")

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fail(stderr, err)
	}
	// A hung issuer or authorizer request is released before the servers
	// drain, or the drain would wait on it.
	iss.Close()
	authz.Close()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	return 0
}

// fail writes the one line a developer reads on a start-up failure and
// returns exit code 1.
func fail(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "lux-stubs: %v\n", err)
	return 1
}
