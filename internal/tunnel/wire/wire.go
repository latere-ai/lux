// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package wire is the tunnel protocol of spec 013 as bytes: the frames
// of the session stream, the header lines of a carrier, and the framing
// of a body. Both halves of the tunnel, the gateway in internal/tunnel
// and the agent in internal/tunnel/agent, encode and decode through
// this package and nothing else, so one exchange is one definition. It
// imports the standard library alone, because the agent half is linked
// into the lux command, whose build list spec 014 keeps small.
//
// Every stream is one line of JSON followed by a body. The session
// stream is lines only, newline-delimited, one Frame each. A carrier
// is one Request line then the request body toward the agent, and one
// Response line then the response body toward the gateway. A body is
// carried in HTTP chunked encoding rather than to the end of the
// stream, because an HTTP/2 server cannot end its response while it
// still reads the request, so the end of a body has to be marked in
// band; the encoding also tells a body that finished from a stream
// that broke, which a stream of model tokens cannot tell on its own.
// An empty line before a header line is a keepalive and is skipped.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
)

// The frame types of the session stream.
const (
	// TypeReady is the gateway's first frame: the session is registered.
	TypeReady = "ready"
	// TypeHeartbeat is sent by the agent every ttl/3 and answered by the
	// gateway once the registry row is renewed.
	TypeHeartbeat = "heartbeat"
	// TypeClose is the gateway's last frame: the session ends, with the
	// reason.
	TypeClose = "close"
)

// The close reasons, each telling the agent what to do next.
const (
	// ReasonSuperseded is another agent holding this Provider now: a
	// retry would fight it, so the agent stops.
	ReasonSuperseded = "superseded"
	// ReasonTokenExpired is the session's bearer expiring with no fresh
	// token: the agent reconnects once with a fresh one and stops when
	// it has none.
	ReasonTokenExpired = "token_expired"
	// ReasonProviderDeleted is the object gone: the agent stops.
	ReasonProviderDeleted = "provider_deleted"
	// ReasonDraining is this replica shutting down: the agent reconnects
	// at once and lands on another.
	ReasonDraining = "draining"
)

// HeaderSession is the request header a carrier names its session in.
const HeaderSession = "Lux-Tunnel-Session"

// MaxLine bounds one header line. A request's header set fits in a
// fraction of it; a line past it is a peer that is not speaking this
// protocol.
const MaxLine = 1 << 20

// ErrLineTooLong is a header line past MaxLine.
var ErrLineTooLong = errors.New("tunnel wire: the header line is longer than 1 MiB")

// Frame is one line of the session stream, in either direction. The
// members that do not apply to a type are omitted.
type Frame struct {
	Type string `json:"type"`
	// Session and TTL are the ready frame's: the session id and the
	// liveness window, whose third is the heartbeat period.
	Session string `json:"session,omitempty"`
	TTL     string `json:"ttl,omitempty"`
	// Carriers is how many carriers the ready frame asks the agent to
	// park.
	Carriers int `json:"carriers,omitempty"`
	// Token is a fresh bearer on a heartbeat from the agent.
	Token string `json:"token,omitempty"`
	// Reason is the close frame's.
	Reason string `json:"reason,omitempty"`
}

// Request is the header line the gateway writes onto a carrier: one
// proxied upstream request without its body. Path is the dialect's
// operation path under the agent's upstream address, which the agent
// joins to its own base URL; the gateway never learns that address.
type Request struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Headers map[string][]string `json:"headers"`
	// Stream says the gateway expects an event stream, so the agent
	// flushes as bytes arrive. It is advisory: the agent flushes every
	// chunk whatever it says.
	Stream bool `json:"stream"`
}

// Response is the header line the agent writes back: the runtime's
// status and headers, or, when the agent could not reach the runtime,
// Error and nothing else, which the gateway reports as the transport
// failure it is.
type Response struct {
	Status  int                 `json:"status,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Error   string              `json:"error,omitempty"`
}

// WriteLine writes v as one line of JSON and flushes when w flushes.
func WriteLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("tunnel wire: encoding a %T: %w", v, err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	return flush(w)
}

// Keepalive writes one empty line, which a reader before its header
// line skips.
func Keepalive(w io.Writer) error {
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return err
	}
	return flush(w)
}

// ReadLine reads the next non-empty line into v. io.EOF is the stream
// ending before a line; a partial last line is io.ErrUnexpectedEOF.
func ReadLine(r *bufio.Reader, v any) error {
	for {
		line, err := readLine(r)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := json.Unmarshal(line, v); err != nil {
			return fmt.Errorf("tunnel wire: the line is not a %T: %w", v, err)
		}
		return nil
	}
}

// readLine reads one line up to MaxLine, without its newline.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxLine+1 {
			return nil, ErrLineTooLong
		}
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// NewBodyWriter frames a body in chunked encoding onto w, flushing
// after every chunk so a token reaches the other end as it is written.
// Close writes the last chunk, which is what the reader takes as the
// body's end; the stream stays open for what follows.
func NewBodyWriter(w io.Writer) io.WriteCloser {
	return &bodyWriter{cw: httputil.NewChunkedWriter(w), w: w}
}

type bodyWriter struct {
	cw io.WriteCloser
	w  io.Writer
}

func (b *bodyWriter) Write(p []byte) (int, error) {
	n, err := b.cw.Write(p)
	if err != nil {
		return n, err
	}
	return n, flush(b.w)
}

func (b *bodyWriter) Close() error {
	if err := b.cw.Close(); err != nil {
		return err
	}
	return flush(b.w)
}

// NewBodyReader reads a body NewBodyWriter framed: io.EOF at the last
// chunk, io.ErrUnexpectedEOF when the stream ended before it.
func NewBodyReader(r *bufio.Reader) io.Reader {
	return httputil.NewChunkedReader(r)
}

// Flushing returns a writer over w that flushes the response after
// every write, so a header line and every chunk leave the process as
// they are written rather than when a buffer fills. A writer that is
// not an http.ResponseWriter is returned as it is.
func Flushing(w io.Writer) io.Writer {
	if rw, ok := w.(http.ResponseWriter); ok {
		return &flushWriter{w: rw, rc: http.NewResponseController(rw)}
	}
	return w
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, f.Flush()
}

// Flush flushes the response; a writer that cannot flush is not an
// error, because the bytes are written all the same.
func (f *flushWriter) Flush() error {
	if err := f.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// flush flushes w when it can.
func flush(w io.Writer) error {
	switch f := w.(type) {
	case *flushWriter:
		return f.Flush()
	case interface{ Flush() error }:
		return f.Flush()
	case http.Flusher:
		f.Flush()
	}
	return nil
}
