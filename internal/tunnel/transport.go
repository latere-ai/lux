// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/semaphore"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// transport is the RoundTripper of one tunneled Provider: spec 005's
// client shape, the concurrency semaphore and the User-Agent, over a
// carrier instead of a socket. The request's URL has no scheme and no
// host, because the Provider has no baseURL: its path is the dialect's
// operation path, which the agent joins to its own upstream address.
type transport struct {
	g          *Gateway
	providerID string
	name       string
	sem        *semaphore.Semaphore
	userAgent  string
}

// RoundTrip acquires a slot for at most the request's remaining
// deadline, writes the request onto a carrier of the session, local or
// forwarded, and returns the runtime's answer as a response whose body
// streams; the slot is held until the body is closed.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wait := time.Duration(math.MaxInt64)
	if deadline, ok := ctx.Deadline(); ok {
		wait = deadline.Sub(t.g.now())
	}
	release, ok := t.sem.Acquire(ctx, wait)
	if !ok {
		if err := ctx.Err(); errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %d slot(s) toward %s all in flight", gateway.ErrProviderBusy, t.sem.Size(), t.name)
	}
	hdr := req.Header.Clone()
	if hdr == nil {
		hdr = http.Header{}
	}
	hdr.Set("User-Agent", t.userAgent)
	var body io.Reader
	if req.Body != nil && req.Body != http.NoBody {
		body = req.Body
		if req.ContentLength > 0 {
			hdr.Set("Content-Length", strconv.FormatInt(req.ContentLength, 10))
		}
	}
	id := hdr.Get(gateway.HeaderRequestID)
	if id == "" {
		id = t.g.o.NewID(v1.PrefixRequest)
		hdr.Set(gateway.HeaderRequestID, id)
	}
	wreq := wire.Request{
		ID: id, Method: req.Method, Path: req.URL.EscapedPath(), Query: req.URL.RawQuery, Headers: hdr,
		Stream: strings.Contains(hdr.Get("Accept"), "text/event-stream"),
	}
	resp, rc, err := t.g.exchange(ctx, t.providerID, wreq, body)
	if err != nil {
		release()
		return nil, err
	}
	return &http.Response{
		Status:        strconv.Itoa(resp.Status) + " " + http.StatusText(resp.Status),
		StatusCode:    resp.Status,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		Header:        canonical(resp.Headers),
		Body:          &releasing{ReadCloser: rc, release: release},
		ContentLength: -1,
		Request:       req,
	}, nil
}

// canonical builds a header from the wire's map, every name in its
// canonical form, and drops what describes the hop rather than the
// response.
func canonical(m map[string][]string) http.Header {
	h := make(http.Header, len(m))
	for name, values := range m {
		name = textproto.CanonicalMIMEHeaderKey(name)
		switch name {
		case "Transfer-Encoding", "Connection", "Keep-Alive", "Content-Length":
			continue
		}
		h[name] = values
	}
	return h
}

// releasing gives the slot back when the body is closed.
type releasing struct {
	io.ReadCloser
	release func()
}

func (r *releasing) Close() error {
	defer r.release()
	return r.ReadCloser.Close()
}

// exchange sends one proxied request toward the Provider's session:
// onto a parked carrier when this replica holds it, to the holder's
// forward route when another does, and nowhere when the registry has no
// live row.
func (g *Gateway) exchange(ctx context.Context, providerID string, req wire.Request, body io.Reader) (wire.Response, io.ReadCloser, error) {
	if s := g.session(providerID); s != nil {
		return s.exchange(ctx, req, body)
	}
	row, err := g.o.Store.Tunnels().Get(ctx, providerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return wire.Response{}, nil, fmt.Errorf("%w: Provider %s", ErrNoSession, providerID)
	case err != nil:
		return wire.Response{}, nil, fmt.Errorf("tunnel: reading the registry for Provider %s: %w", providerID, err)
	case row.Replica == "" || row.Replica == g.o.Replica:
		return wire.Response{}, nil, fmt.Errorf("%w: the registry names session %s on replica %q, which this replica does not hold", ErrNoSession, row.Session, row.Replica)
	case g.o.Replica == "":
		return wire.Response{}, nil, fmt.Errorf("%w: session %s is on replica %s", ErrNoForward, row.Session, row.Replica)
	}
	return g.forwardTo(ctx, row, req, body)
}

// forwardTo sends the request to the holder's forward route with the
// first secret, and on unauthenticated with each next one before any
// body byte, so a rotation in progress fails no forward: a replica on
// the old list still accepts the old entry, and a replica on the new
// list is asked with the new one first.
func (g *Gateway) forwardTo(ctx context.Context, row store.Tunnel, req wire.Request, body io.Reader) (wire.Response, io.ReadCloser, error) {
	if len(g.o.Secrets) == 0 {
		return wire.Response{}, nil, fmt.Errorf("%w: LUX_TUNNEL_FORWARD_SECRET is unset", ErrNoForward)
	}
	target := "http://" + row.Replica + "/internal/tunnel/" + row.ProviderID
	var last error
	for i, secret := range g.o.Secrets {
		resp, rc, refused, err := g.forwardWith(ctx, target, secret, req, body)
		if err == nil {
			return resp, rc, nil
		}
		last = err
		if !refused {
			break
		}
		g.logger.InfoContext(ctx, "tunnel: a forward secret was refused; trying the next", "replica", row.Replica, "entry", i+1, "of", len(g.o.Secrets))
	}
	return wire.Response{}, nil, last
}

// forwardWith is one attempt with one secret. The header line goes
// first and the body only once the holder answered 200, so a refusal
// leaves the body untouched for the next attempt; refused reports an
// unauthenticated answer.
func (g *Gateway) forwardWith(ctx context.Context, target, secret string, req wire.Request, body io.Reader) (_ wire.Response, _ io.ReadCloser, refused bool, _ error) {
	attempt, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	accepted := make(chan struct{})
	go func() {
		if err := wire.WriteLine(pw, req); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		select {
		case <-accepted:
		case <-attempt.Done():
			_ = pw.CloseWithError(attempt.Err())
			return
		}
		bw := wire.NewBodyWriter(pw)
		if body != nil {
			if _, err := io.Copy(bw, body); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		if err := bw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	hreq, err := http.NewRequestWithContext(attempt, http.MethodPost, target, pr)
	if err != nil {
		cancel()
		return wire.Response{}, nil, false, fmt.Errorf("tunnel: building the forward to %s: %w", target, err)
	}
	hreq.Header.Set("Authorization", "Bearer "+secret)
	hreq.Header.Set("Content-Type", "application/x-ndjson")
	hreq.Header.Set("User-Agent", "luxd/"+g.o.Version)
	hreq.Header.Set(gateway.HeaderRequestID, req.ID)
	hresp, err := g.forward.Do(hreq)
	if err != nil {
		cancel()
		return wire.Response{}, nil, false, fmt.Errorf("tunnel: forward to %s: %w", target, err)
	}
	if hresp.StatusCode != http.StatusOK {
		excerpt, _ := io.ReadAll(io.LimitReader(hresp.Body, 4096))
		_ = hresp.Body.Close()
		cancel()
		return wire.Response{}, nil, hresp.StatusCode == http.StatusUnauthorized,
			fmt.Errorf("tunnel: forward to %s answered %d: %s", target, hresp.StatusCode, strings.Join(strings.Fields(string(excerpt)), " "))
	}
	close(accepted)
	rd := bufio.NewReader(hresp.Body)
	var wresp wire.Response
	finish := func() {
		_ = hresp.Body.Close()
		cancel()
	}
	if err := wire.ReadLine(rd, &wresp); err != nil {
		finish()
		return wire.Response{}, nil, false, fmt.Errorf("tunnel: reading the holder's response line from %s: %w", target, err)
	}
	if wresp.Error != "" {
		finish()
		return wire.Response{}, nil, false, fmt.Errorf("tunnel: the holder %s could not serve request %s: %s", target, req.ID, wresp.Error)
	}
	return wresp, &exchangeBody{r: wire.NewBodyReader(rd), finish: finish}, false, nil
}

// newForwardClient is the client toward another replica's internal
// listener: unencrypted HTTP/2 alone, because the forward hop streams
// both ways and an internal listener speaks plaintext.
func newForwardClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{
		Protocols:           protocols,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}}
}
