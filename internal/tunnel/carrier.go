// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/lux/internal/tunnel/wire"
)

// CarrierRequest is what internal/api hands ServeCarrier once the
// bearer is verified: the Provider the path names, the session the
// header names, and the bearer's subject, which must be the session's.
type CarrierRequest struct {
	Provider string // the path's id or name
	Session  string // Lux-Tunnel-Session
	Subject  string
}

// carrier is one parked POST from the agent: the response body toward
// it, the request body from it, and the handoff of one job.
type carrier struct {
	out  io.Writer       // toward the agent, flushing
	in   *bufio.Reader   // from the agent
	jobs chan *job       // unbuffered: a send completes only when the carrier took it
	gone <-chan struct{} // the carrier's request context
}

// job is one proxied request on its way through a carrier.
type job struct {
	req    wire.Request
	body   io.Reader
	done   chan struct{} // closed when the response body is closed or the exchange abandoned
	failed chan error    // the carrier's write failure
	once   sync.Once
}

func (j *job) finish() { j.once.Do(func() { close(j.done) }) }

func (j *job) fail(err error) {
	select {
	case j.failed <- err:
	default:
	}
}

// ServeCarrier is POST /v1/providers/{id-or-name}/tunnel/carry after
// internal/api authenticated it: the HTTP/2 check, the session live on
// this replica and the bearer's subject its subject, then the carrier
// parked until it is given work, with an empty line every ttl/3 so an
// idle timeout between the agent and the gateway does not cut it.
func (g *Gateway) ServeCarrier(w http.ResponseWriter, r *http.Request, cr CarrierRequest) *Error {
	if r.ProtoMajor < 2 {
		return refuse(CodeNotFound, "the tunnel needs HTTP/2; this connect negotiated "+r.Proto)
	}
	s := g.sessionByRef(cr.Provider)
	if s == nil || s.id != cr.Session {
		return refuse(CodeUnauthenticated, "no live session "+strconv.Quote(cr.Session)+" for Provider "+cr.Provider+" on this replica")
	}
	if s.subject != cr.Subject {
		return refuse(CodeUnauthenticated, "the carrier's subject "+cr.Subject+" is not the session's "+s.subject)
	}
	c := &carrier{out: wire.Flushing(w), in: bufio.NewReaderSize(r.Body, 64<<10), jobs: make(chan *job), gone: r.Context().Done()}
	select {
	case s.parked <- c:
	default:
		return refuse(CodeProviderUnavailable, strconv.Itoa(MaxParked)+" carriers are already parked for session "+s.id)
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// The first empty line commits the response, so the agent knows it
	// is parked.
	if err := wire.Keepalive(c.out); err != nil {
		return nil
	}
	tick := time.NewTicker(g.ttl / 3)
	defer tick.Stop()
	for {
		select {
		case j := <-c.jobs:
			c.serve(j, s.done)
			return nil
		case <-tick.C:
			if err := wire.Keepalive(c.out); err != nil {
				return nil
			}
		case <-s.done:
			return nil
		case <-c.gone:
			return nil
		}
	}
}

// serve writes the job onto the carrier, the header line first and
// flushed before any body byte, then the body in chunks, and holds the
// carrier open until the response body was consumed, the agent left, or
// the session ended.
func (c *carrier) serve(j *job, sessionDone <-chan struct{}) {
	if err := wire.WriteLine(c.out, j.req); err != nil {
		j.fail(err)
		return
	}
	bw := wire.NewBodyWriter(c.out)
	if j.body != nil {
		if _, err := io.Copy(bw, j.body); err != nil {
			j.fail(err)
			return
		}
	}
	if err := bw.Close(); err != nil {
		j.fail(err)
		return
	}
	select {
	case <-j.done:
	case <-c.gone:
	case <-sessionDone:
	}
}

// exchange sends one proxied request through a parked carrier of the
// session and returns the agent's header line and the response body,
// which the caller closes. It waits for a carrier until ctx ends, which
// the caller bounds by the Provider's timeout.
func (s *session) exchange(ctx context.Context, req wire.Request, body io.Reader) (wire.Response, io.ReadCloser, error) {
	for {
		var c *carrier
		select {
		case c = <-s.parked:
		case <-ctx.Done():
			return wire.Response{}, nil, fmt.Errorf("%w: session %s: %v", ErrNoCarrier, s.id, ctx.Err())
		case <-s.done:
			return wire.Response{}, nil, fmt.Errorf("%w: session %s", ErrSessionClosed, s.id)
		}
		j := &job{req: req, body: body, done: make(chan struct{}), failed: make(chan error, 1)}
		select {
		case c.jobs <- j:
			return s.await(ctx, c, j)
		case <-c.gone:
			// This carrier left while it was parked; the next one.
		case <-ctx.Done():
			return wire.Response{}, nil, fmt.Errorf("%w: session %s: %v", ErrNoCarrier, s.id, ctx.Err())
		case <-s.done:
			return wire.Response{}, nil, fmt.Errorf("%w: session %s", ErrSessionClosed, s.id)
		}
	}
}

// await reads the agent's header line while the carrier writes the
// request, and hands back the body.
func (s *session) await(ctx context.Context, c *carrier, j *job) (wire.Response, io.ReadCloser, error) {
	type header struct {
		resp wire.Response
		err  error
	}
	ch := make(chan header, 1)
	go func() {
		var resp wire.Response
		err := wire.ReadLine(c.in, &resp)
		ch <- header{resp, err}
	}()
	abandon := func(err error) (wire.Response, io.ReadCloser, error) {
		j.finish()
		return wire.Response{}, nil, err
	}
	select {
	case h := <-ch:
		switch {
		case h.err != nil:
			return abandon(fmt.Errorf("tunnel: reading the agent's response line for request %s on session %s: %w", j.req.ID, s.id, h.err))
		case h.resp.Error != "":
			return abandon(fmt.Errorf("tunnel: the agent could not serve request %s: %s", j.req.ID, h.resp.Error))
		case h.resp.Status < 100 || h.resp.Status > 999:
			return abandon(fmt.Errorf("tunnel: the agent answered request %s with status %d, which is not one", j.req.ID, h.resp.Status))
		}
		return h.resp, &exchangeBody{r: wire.NewBodyReader(c.in), finish: j.finish}, nil
	case err := <-j.failed:
		return abandon(fmt.Errorf("tunnel: writing request %s onto a carrier of session %s: %w", j.req.ID, s.id, err))
	case <-ctx.Done():
		return abandon(ctx.Err())
	case <-s.done:
		return abandon(fmt.Errorf("%w: session %s", ErrSessionClosed, s.id))
	}
}

// exchangeBody is the response body of one exchange: the agent's chunks,
// and on Close the release of the carrier.
type exchangeBody struct {
	r      io.Reader
	finish func()
}

func (b *exchangeBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *exchangeBody) Close() error {
	b.finish()
	return nil
}
