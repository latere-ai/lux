// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command authorizer is the minimal authorization endpoint of
// docs/plane.md: one POST, one decision, answered from the bearer and
// this process's own state alone. The endpoint around the decision is
// latere.ai/x/pkg/authz/server, the contract's own scaffold: it reads
// the bearer, bounds and decodes the body, refuses an action outside
// the vocabulary, denies the reserved probe id, and writes the answer.
// The vocabulary it validates against is latere.ai/x/lux/authorizer's,
// the package that carries the actions luxd asks in, so a copy of this
// program runs go get latere.ai/x/lux first. Run it beside the gateway,
//
//	go run ./examples/authorizer -addr 127.0.0.1:8081 -token "$LUX_AUTHORIZER_TOKEN"
//
// and point the gateway at it with LUX_AUTHORIZER_URL and
// LUX_AUTHORIZER_TOKEN. Everything a platform decides, who declares the
// catalog, what a plan may put on one Key, and whose objects a list
// returns, is in policy.Decide below. docs/plane.md carries this file and
// TestPlaneDocAuthorizerConforms holds the two equal and runs the
// contract's own conformance suite against it.
package main

import (
	"context"
	"encoding/json"
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

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/server"

	"latere.ai/x/lux/authorizer"
)

// spendCap is the ceiling one Key may ask for, by the plan the
// platform's issuer stamps into the token. An administrator is under no
// ceiling: a ceiling refuses a Key that names no limit at all, and the
// catalog and the installation's own Keys are declared without one.
var spendCap = map[string]string{"free": "5", "team": "50"}

// policy is the half of the contract a platform writes. By the time
// Decide is called the scaffold has read the bearer, bounded and decoded
// the body, held the action and the resource kind to the vocabulary, and
// denied the probe, so what is left is the policy and nothing else. It
// answers every action: none of Lux's lists returns a page, so the
// endpoint names no page action and needs no Lister.
//
// Everything a platform decides, who declares the catalog, what a plan
// may put on one Key, and whose objects a list returns, is here.
type policy struct{}

// Decide is the whole policy: the catalog declared by an administrator
// and readable by everyone, an object to its owner alone, and a ceiling
// and a filter on everything else. The kind an action acts on is
// authorizer.Kind's answer, so the catalog's two kinds are named once
// and a new action arrives here as a kind this policy already decides.
//
// An error is never an allow. server.Unavailable is the 503 a gateway
// reads as authorizer_unavailable, which a platform raises when it
// cannot see the state an answer needs; this one raises it only when it
// cannot render its own ceiling.
func (policy) Decide(_ context.Context, req authz.Request) (authz.Decision, error) {
	plan, _ := req.Claims["plan"].(string)
	mine := &authz.Filter{Owners: []string{req.Subject}}
	owner := req.Resource.String("owner")
	switch kind := authorizer.Kind(req.Action); {
	case kind == authorizer.KindOwnership:
		return authz.Decision{Allow: plan == "admin", Reason: "owner assignment requires an administrator"}, nil
	case kind == "Provider", kind == "Model":
		switch {
		case req.Action == authorizer.ActionModelUse || strings.HasSuffix(req.Action, ".read") || strings.HasSuffix(req.Action, ".list"):
			return authz.Decision{Allow: true}, nil // the catalog is the platform's and is offered to every user
		case plan == "admin":
			return authz.Decision{Allow: true}, nil
		}
		return authz.Decision{Reason: "the catalog is declared by the platform"}, nil
	case owner != "" && owner != req.Subject:
		return authz.Decision{Reason: "not yours"}, nil
	case plan == "admin":
		return authz.Decision{Allow: true, Filter: mine}, nil
	default:
		// The ceilings a Key this subject applies is held to, by the wire
		// names luxd decodes; authorizer.WireLimits is the Go type of the
		// same six members, for a platform that renders them from a struct.
		limits, err := json.Marshal(map[string]any{"max_key_spend": spendCap[plan], "max_key_ttl": "720h", "max_keys": 100})
		if err != nil {
			return authz.Decision{}, server.Unavailable("the " + plan + " plan's ceiling cannot be rendered")
		}
		return authz.Decision{Allow: true, Limits: limits, Filter: mine}, nil
	}
}

// handler is the endpoint: the contract's scaffold over this policy and
// the gateway's vocabulary. It holds no session, and it calls neither
// the gateway nor the issuer while deciding, because the gateway is
// waiting inside the very request this answers.
func handler(token string) http.Handler {
	return server.New(server.Options{
		Bearer:     token,
		Vocabulary: authorizer.Vocabulary(),
		Decider:    policy{},
	})
}

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stderr)) }

// run serves until the context ends or the process is asked to stop, and
// returns the exit code: 0 on a clean stop, 1 when the address or the
// token is unusable, 2 on a usage error.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("authorizer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8081", "the address to serve on")
	token := fs.String("token", os.Getenv("AUTHORIZER_TOKEN"), "the bearer the gateway sends, its LUX_AUTHORIZER_TOKEN")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		_, _ = fmt.Fprintln(stderr, "authorizer: -token or AUTHORIZER_TOKEN is the bearer the gateway sends, and there is no unauthenticated mode")
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	var listener net.ListenConfig
	ln, err := listener.Listen(ctx, "tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "authorizer:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "authorizer: deciding at %s\n", ln.Addr())
	srv := &http.Server{Handler: handler(*token), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	err = srv.Serve(ln)
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(stderr, "authorizer:", err)
		return 1
	}
	return 0
}
