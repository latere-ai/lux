// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command authorizer is the minimal authorization endpoint of
// docs/plane.md: one POST, one decision, answered from the bearer and
// this process's own state alone. The actions it decides on are named
// through latere.ai/x/lux/authorizer, the package that carries the
// vocabulary luxd asks in, so a copy of this program runs
// go get latere.ai/x/lux first. Run it beside the gateway,
//
//	go run ./examples/authorizer -addr 127.0.0.1:8081 -token "$LUX_AUTHORIZER_TOKEN"
//
// and point the gateway at it with LUX_AUTHORIZER_URL and
// LUX_AUTHORIZER_TOKEN. Everything a platform decides, who declares the
// catalogue, what a plan may put on one Key, and whose objects a list
// returns, is in decide below. docs/plane.md carries this file and
// TestPlaneDocAuthorizerConforms holds the two equal and runs the
// contract's own conformance suite against it.
package main

import (
	"context"
	"crypto/subtle"
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

	"latere.ai/x/lux/authorizer"
)

// req is the envelope the gateway POSTs, of which this endpoint reads
// four fields. A Go authorizer may decode it into authz.Request from
// latere.ai/x/pkg/authz instead; one in any language reads the JSON
// names below.
type req struct {
	Subject  string         `json:"subject"`
	Claims   map[string]any `json:"claims"`
	Action   string         `json:"action"`
	Resource map[string]any `json:"resource"`
}

// resp is one decision: the verdict, the reason a deny carries into the
// gateway's developer detail, the ceilings a Key this subject applies is
// held to, and the filter a list and a usage query are narrowed by.
type resp struct {
	Allow  bool           `json:"allow"`
	Reason string         `json:"reason,omitempty"`
	Limits map[string]any `json:"limits,omitempty"`
	Filter map[string]any `json:"filter,omitempty"`
}

// probeID is authz.ProbeID: every authorizer denies it, so luxd check
// can tell an endpoint that reads the request from one that does not.
const probeID = "00000000-0000-0000-0000-000000000001"

// spendCap is the ceiling one Key may ask for, by the plan the
// platform's issuer stamps into the token. An administrator is under no
// ceiling: a ceiling refuses a Key that names no limit at all, and the
// catalogue and the installation's own Keys are declared without one.
var spendCap = map[string]string{"free": "5", "team": "50"}

// decide is the whole policy: the probe first, an action outside the
// vocabulary, the catalogue declared by an administrator and readable by
// everyone, an object to its owner alone, and a ceiling and a filter on
// everything else. The kind an action acts on is authorizer.Kind's
// answer, so the catalogue's two kinds are named once and a new action
// arrives here as a kind this policy already decides.
func decide(r req) resp {
	plan, _ := r.Claims["plan"].(string)
	switch kind := authorizer.Kind(r.Action); {
	case r.Resource["id"] == probeID:
		return resp{Allow: false, Reason: "the probe id is reserved"}
	case kind == "":
		return resp{Reason: "no action of the gateway's vocabulary"}
	case kind == "Provider", kind == "Model":
		switch {
		case r.Action == authorizer.ActionModelUse || strings.HasSuffix(r.Action, ".read") || strings.HasSuffix(r.Action, ".list"):
			return resp{Allow: true} // the catalogue is the platform's and is offered to every user
		case plan == "admin":
			return resp{Allow: true}
		}
		return resp{Reason: "the catalogue is declared by the platform"}
	case r.Resource["owner"] != nil && r.Resource["owner"] != r.Subject:
		return resp{Allow: false, Reason: "not yours"}
	case plan == "admin":
		return resp{Allow: true, Filter: map[string]any{"owners": []string{r.Subject}}}
	default:
		return resp{Allow: true,
			Limits: map[string]any{"max_key_spend": spendCap[plan], "max_key_ttl": "720h", "max_keys": 100},
			Filter: map[string]any{"owners": []string{r.Subject}}}
	}
}

// handler answers one decision per POST. Everything it needs is the
// bearer and the body: it holds no session, and it calls neither the
// gateway nor the issuer while deciding, because the gateway is waiting
// inside the very request this answers.
func handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "post one decision request", http.StatusMethodNotAllowed)
			return
		}
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) != 1 {
			http.Error(w, "the bearer is not this endpoint's", http.StatusUnauthorized)
			return
		}
		var in req
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			http.Error(w, "the body is no decision request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(decide(in))
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
	server := &http.Server{Handler: handler(*token), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(ln)
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(stderr, "authorizer:", err)
		return 1
	}
	return 0
}
