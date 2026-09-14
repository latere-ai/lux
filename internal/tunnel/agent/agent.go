// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package agent is the agent side of spec 013's tunnel: it opens the
// session toward the gateway with a token source, parks carriers, and
// serves each proxied request against a runtime on this machine, whose
// address never leaves it. The lux serve command of spec 014 is a thin
// command over Run: it supplies the flags, reconnects with backoff, and
// turns the close reason Run returns into an exit code. The package
// reaches the standard library, the wire format, and
// latere.ai/x/pkg/httpjson for the error envelope, so the lux binary's
// build list stays what spec 014 says.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lux/internal/tunnel/wire"
)

// The bounds of the carrier pool.
const (
	// DefaultCarriers is how many carriers stay parked when neither the
	// options nor the ready frame say.
	DefaultCarriers = 4
	// MaxInFlight is the ceiling of carriers open at once, parked and
	// serving together.
	MaxInFlight = 64
	// retryAfter is the pause before a carrier that failed to park is
	// opened again while the session lives.
	retryAfter = time.Second
)

// Options is what Run opens a session under. Gateway, Provider, Upstream,
// and Token are required.
type Options struct {
	// Gateway is the gateway's public URL, http:// or https://.
	Gateway string
	// Provider is the tunnelled Provider's name or id.
	Provider string
	// Upstream is the runtime's base URL on this machine, the --upstream
	// of lux serve. It is joined to every request path and is never
	// sent.
	Upstream string
	// Token yields the bearer, read for every request the agent sends,
	// so a rotated token is used from the next request on; a token that
	// differs from the last one sent is carried in the next heartbeat.
	// FileToken reads one from a file.
	Token func() (string, error)
	// Carriers is how many carriers stay parked; zero takes the ready
	// frame's number.
	Carriers int
	// UserAgent is sent on every request, lux/<version>.
	UserAgent string
	// Client is the client toward the gateway; nil builds one for the
	// scheme: HTTP/2 by ALPN for https://, unencrypted HTTP/2 for
	// http://.
	Client *http.Client
	// Runtime is the client toward the runtime; nil builds one without
	// compression and without a proxy.
	Runtime *http.Client
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
}

// CloseError is the gateway's close frame: the session ended for
// Reason, one of the wire package's four.
type CloseError struct {
	Reason string
}

func (e *CloseError) Error() string { return "the gateway closed the session: " + e.Reason }

// RefusedError is a connect the gateway refused before a session was
// opened: the status and the error envelope, when the body was one.
type RefusedError struct {
	Status  int
	Code    string
	Message string
	Detail  string
}

func (e *RefusedError) Error() string {
	s := "the gateway refused the connect with " + strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// FileToken reads the bearer from path on every call, trimmed of
// surrounding white space, which is how a token file a login command
// rewrites is picked up without a restart.
func FileToken(path string) func() (string, error) {
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		return token, nil
	}
}

// agent is one Run.
type agent struct {
	o        Options
	gateway  *url.URL
	upstream *url.URL
	client   *http.Client
	runtime  *http.Client
	logger   *slog.Logger
	session  string
	inFlight atomic.Int32
	wg       sync.WaitGroup // the carriers and the heartbeat, joined before Run returns
}

// Run opens one session and serves it until it ends. It returns nil
// when ctx ended, which is a clean stop the gateway sees at once; a
// *CloseError when the gateway closed the session, with the reason the
// caller acts on; a *RefusedError when the connect was refused; and the
// transport's error when the stream broke, which a caller retries with
// backoff. Every goroutine Run started has ended when it returns, so
// nothing writes to the logger after the caller has its answer.
func Run(ctx context.Context, o Options) error {
	a, err := newAgent(o)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		a.wg.Wait()
	}()
	token, err := a.o.Token()
	if err != nil {
		return fmt.Errorf("agent: reading the token: %w", err)
	}
	pr, pw := io.Pipe()
	req, err := a.newRequest(sctx, a.gateway.JoinPath("v1", "providers", a.o.Provider, "tunnel").String(), token, pr)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		_ = pw.Close()
		return fmt.Errorf("agent: connecting to %s: %w", req.URL.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_ = pw.Close()
		return refusal(resp)
	}
	rd := bufio.NewReader(resp.Body)
	var ready wire.Frame
	if err := wire.ReadLine(rd, &ready); err != nil {
		_ = pw.Close()
		return fmt.Errorf("agent: reading the ready frame: %w", err)
	}
	if ready.Type != wire.TypeReady || ready.Session == "" {
		_ = pw.Close()
		return fmt.Errorf("agent: the first frame is a %q, not ready", ready.Type)
	}
	a.session = ready.Session
	ttl, _ := time.ParseDuration(ready.TTL)
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	carriers := a.o.Carriers
	if carriers <= 0 {
		carriers = ready.Carriers
	}
	if carriers <= 0 {
		carriers = DefaultCarriers
	}
	a.logger.InfoContext(ctx, "agent: session ready", "session", a.session, "provider", a.o.Provider, "ttl", ttl, "carriers", carriers)
	for range carriers {
		a.wg.Go(func() { a.park(sctx) })
	}
	a.wg.Go(func() { a.heartbeat(sctx, pw, ttl, token) })
	for {
		var f wire.Frame
		if err := wire.ReadLine(rd, &f); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("agent: the session stream ended: %w", err)
		}
		switch f.Type {
		case wire.TypeHeartbeat:
		case wire.TypeClose:
			a.logger.InfoContext(ctx, "agent: session closed by the gateway", "session", a.session, "reason", f.Reason)
			return &CloseError{Reason: f.Reason}
		default:
			a.logger.WarnContext(ctx, "agent: a frame the session does not take was ignored", "session", a.session, "type", f.Type)
		}
	}
}

// newAgent checks the options and builds the clients.
func newAgent(o Options) (*agent, error) {
	switch {
	case o.Gateway == "":
		return nil, errors.New("agent: Options.Gateway is empty")
	case o.Provider == "":
		return nil, errors.New("agent: Options.Provider is empty")
	case o.Upstream == "":
		return nil, errors.New("agent: Options.Upstream is empty")
	case o.Token == nil:
		return nil, errors.New("agent: Options.Token is nil")
	}
	gw, err := url.Parse(o.Gateway)
	if err != nil || (gw.Scheme != "http" && gw.Scheme != "https") || gw.Host == "" {
		return nil, fmt.Errorf("agent: the gateway URL %q is not an http:// or https:// URL with a host", o.Gateway)
	}
	up, err := url.Parse(o.Upstream)
	if err != nil || (up.Scheme != "http" && up.Scheme != "https") || up.Host == "" {
		return nil, fmt.Errorf("agent: the upstream URL %q is not an http:// or https:// URL with a host", o.Upstream)
	}
	a := &agent{o: o, gateway: gw, upstream: up, client: o.Client, runtime: o.Runtime, logger: o.Logger}
	if a.client == nil {
		a.client = newGatewayClient(gw.Scheme == "http")
	}
	if a.runtime == nil {
		a.runtime = &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			DisableCompression:  true,
			MaxIdleConnsPerHost: MaxInFlight,
			IdleConnTimeout:     90 * time.Second,
		}}
	}
	if a.logger == nil {
		a.logger = slog.Default()
	}
	if a.o.UserAgent == "" {
		a.o.UserAgent = "lux"
	}
	return a, nil
}

// newGatewayClient is the client toward the gateway: HTTP/2 by ALPN
// over TLS, and unencrypted HTTP/2 alone for a plaintext URL, so the
// connect never falls back to HTTP/1.1 and meets the gateway's
// refusal.
func newGatewayClient(plaintext bool) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
		DisableCompression:  true,
		IdleConnTimeout:     90 * time.Second,
	}
	if plaintext {
		protocols := new(http.Protocols)
		protocols.SetUnencryptedHTTP2(true)
		tr.Protocols = protocols
	}
	return &http.Client{Transport: tr}
}

// newRequest is one POST toward the gateway with the bearer.
func (a *agent) newRequest(ctx context.Context, target, token string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, fmt.Errorf("agent: building the request to %s: %w", target, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("User-Agent", a.o.UserAgent)
	return req, nil
}

// refusal reads a refused connect into a RefusedError.
func refusal(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &RefusedError{Status: resp.StatusCode}
	var env httpjson.ErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		e.Code, e.Message = env.Error.Code, env.Error.Message
		e.Detail, _ = env.Error.Details["detail"].(string)
		return e
	}
	e.Detail = strings.Join(strings.Fields(string(body)), " ")
	return e
}

// heartbeat sends a heartbeat every ttl/3, carrying a fresh token when
// the source yields one that differs from the last sent, and closes the
// session's request body when ctx ends, which is the clean disconnect.
func (a *agent) heartbeat(ctx context.Context, pw *io.PipeWriter, ttl time.Duration, sent string) {
	tick := time.NewTicker(ttl / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = pw.Close()
			return
		case <-tick.C:
			f := wire.Frame{Type: wire.TypeHeartbeat}
			switch token, err := a.o.Token(); {
			case err != nil:
				a.logger.WarnContext(ctx, "agent: reading the token", "err", err)
			case token != sent:
				f.Token, sent = token, token
			}
			if err := wire.WriteLine(pw, f); err != nil {
				return
			}
		}
	}
}

// park keeps one carrier's place in the pool: it opens a carrier, and
// when the carrier is given work opens the next, so the parked count
// holds while the session lives; a carrier that failed to park is
// retried after a pause, and a place at the ceiling waits for a slot.
func (a *agent) park(ctx context.Context) {
	for ctx.Err() == nil {
		if a.inFlight.Load() >= MaxInFlight {
			select {
			case <-time.After(retryAfter / 10):
			case <-ctx.Done():
			}
			continue
		}
		a.inFlight.Add(1)
		replaced, err := a.carry(ctx)
		a.inFlight.Add(-1)
		if replaced {
			return // the replacement holds the place now
		}
		if err != nil && ctx.Err() == nil {
			a.logger.WarnContext(ctx, "agent: a carrier ended without work", "session", a.session, "err", err)
			select {
			case <-time.After(retryAfter):
			case <-ctx.Done():
			}
		}
	}
}

// carry opens one carrier, waits parked until the gateway writes a
// request onto it, opens a replacement, and serves the request against
// the runtime. replaced reports that a replacement was opened.
func (a *agent) carry(ctx context.Context) (replaced bool, err error) {
	token, err := a.o.Token()
	if err != nil {
		return false, fmt.Errorf("reading the token: %w", err)
	}
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	// An HTTP/2 stream whose request body is still open is not reset when
	// its context ends until the body ends, so the end of the context
	// ends the body, which resets the parked stream at once.
	stop := context.AfterFunc(ctx, func() { _ = pw.CloseWithError(ctx.Err()) })
	defer stop()
	req, err := a.newRequest(ctx, a.gateway.JoinPath("v1", "providers", a.o.Provider, "tunnel", "carry").String(), token, pr)
	if err != nil {
		return false, err
	}
	req.Header.Set(wire.HeaderSession, a.session)
	resp, err := a.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, refusal(resp)
	}
	rd := bufio.NewReader(resp.Body)
	var wreq wire.Request
	if err := wire.ReadLine(rd, &wreq); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil // the carrier ended while parked: the session is over
		}
		return false, fmt.Errorf("reading the request line: %w", err)
	}
	a.wg.Go(func() { a.park(ctx) })
	a.serve(ctx, wreq, rd, pw)
	// The request body ends here, which is this side's half of the
	// stream; the gateway's half ends once the caller has consumed the
	// answer, and closing it earlier would reset the stream under the
	// caller. So the carrier is drained to its end before it is closed.
	_ = pw.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return true, nil
}

// hopByHop are the headers that describe a hop and never cross one.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true, "Content-Length": true,
}

// serve sends one proxied request to the runtime and writes its answer
// back onto the carrier: the response line as soon as the runtime's
// headers arrive, then the body chunk by chunk. A failure to reach the
// runtime is the response line's error member; a carrier that breaks
// while the body streams cancels the runtime's request, which is how a
// caller's disconnect reaches the runtime.
func (a *agent) serve(ctx context.Context, wreq wire.Request, rd *bufio.Reader, out io.Writer) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	target := *a.upstream
	target.Path = strings.TrimSuffix(a.upstream.Path, "/") + wreq.Path
	target.RawPath = ""
	target.RawQuery = wreq.Query
	rreq, err := http.NewRequestWithContext(rctx, wreq.Method, target.String(), wire.NewBodyReader(rd))
	if err != nil {
		_ = wire.WriteLine(out, wire.Response{Error: "building the request toward the runtime: " + a.redact(err.Error())})
		return
	}
	for name, values := range wreq.Headers {
		name = textproto.CanonicalMIMEHeaderKey(name)
		if name == "Content-Length" {
			if n, err := strconv.ParseInt(values[0], 10, 64); err == nil {
				rreq.ContentLength = n
			}
			continue
		}
		if hopByHop[name] {
			continue
		}
		rreq.Header[name] = values
	}
	if rreq.ContentLength == 0 && !hasBody(wreq.Method) {
		// A body of unknown length on a GET would go chunked, which some
		// runtimes refuse; the terminator is left unread on a carrier
		// that is spent anyway.
		rreq.Body = http.NoBody
	}
	rresp, err := a.runtime.Do(rreq)
	if err != nil {
		_ = wire.WriteLine(out, wire.Response{Error: a.redact(err.Error())})
		return
	}
	defer func() { _ = rresp.Body.Close() }()
	headers := make(map[string][]string, len(rresp.Header))
	for name, values := range rresp.Header {
		if !hopByHop[textproto.CanonicalMIMEHeaderKey(name)] {
			headers[name] = values
		}
	}
	if err := wire.WriteLine(out, wire.Response{Status: rresp.StatusCode, Headers: headers}); err != nil {
		return
	}
	bw := wire.NewBodyWriter(out)
	if _, err := io.Copy(bw, rresp.Body); err != nil {
		cancel()
		a.logger.InfoContext(ctx, "agent: the response stream ended short", "request_id", wreq.ID, "err", a.redact(err.Error()))
		return
	}
	_ = bw.Close()
}

// hasBody reports whether a method carries a body by default.
func hasBody(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}

// redact replaces the runtime's address in a message with a word, so
// what reaches the gateway names nothing on this machine.
func (a *agent) redact(s string) string {
	return strings.ReplaceAll(s, a.upstream.Host, "the runtime")
}
