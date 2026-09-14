// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"latere.ai/x/pkg/llmdialect/bridge"

	v1 "latere.ai/x/lux/manifest/v1"
)

// relayChunk is the most the relay writes at once: bytes are relayed as
// read, without waiting for event boundaries.
const relayChunk = 64 << 10

// writeError marks a failure writing to the caller, which is the caller
// gone and never the upstream's fault.
type writeError struct{ err error }

func (e *writeError) Error() string { return "writing to the caller: " + e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }

// relayChunks copies src to dst in chunks of at most relayChunk, flushing
// each, and feeds every chunk to the usage scanner when there is one.
func relayChunks(dst *responseWriter, src io.Reader, sn *bridge.UsageScanner) error {
	buf := make([]byte, relayChunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return &writeError{werr}
			}
			if sn != nil {
				_, _ = sn.Write(buf[:n])
			}
			dst.Flush()
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// relayFrames copies an SSE stream frame by frame, rewriting the model
// member of each frame's data to name, flushing per frame. It is the
// relay of a passthrough stream whose Model's name and upstream name
// differ, the one case where the bytes cannot be relayed as read.
func relayFrames(dst *responseWriter, src io.Reader, sn *bridge.UsageScanner, name string) error {
	br := bufio.NewReaderSize(src, relayChunk)
	var frame []byte
	flush := func() error {
		if len(frame) == 0 {
			return nil
		}
		out := bridge.SetModelInFrame(frame, name)
		frame = frame[:0]
		if _, err := dst.Write(out); err != nil {
			return &writeError{err}
		}
		_, _ = sn.Write(out)
		dst.Flush()
		return nil
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			frame = append(frame, line...)
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return flush()
		}
		if err != nil {
			return err
		}
	}
}

// streamPassthrough relays a streamed upstream response as it arrives:
// headers flushed first, the upstream's Content-Type kept, the bytes as
// read, frame by frame with the Model's name written back when the names
// differ. The record's tokens are the last value of each usage member
// the stream carried.
func (c *call) streamPassthrough(ctx context.Context, t Target, resp *http.Response) *failure {
	td := t.Provider.Spec.Dialect
	sse := isSSE(resp.Header.Get("Content-Type"))
	framing := bridge.FramingJSON
	if sse {
		framing = bridge.FramingSSE
	}
	sn := bridge.NewUsageScanner(wireOf(td), framing)
	c.w.WriteHeader(resp.StatusCode)
	c.w.Flush()
	var err error
	if sse && t.Model != c.model.Metadata.Name && c.door != v1.DialectGemini {
		err = relayFrames(c.w, resp.Body, sn, c.model.Metadata.Name)
	} else {
		err = relayChunks(c.w, resp.Body, sn)
	}
	sn.Close()
	c.tokens = c.streamTokens(sn)
	if err != nil {
		return c.endStream(c.streamFailure(ctx, err))
	}
	return nil
}

// streamTokens is the scanner's answer, or the estimate when the stream
// carried no usage member; a count records zero tokens.
func (c *call) streamTokens(sn *bridge.UsageScanner) Tokens {
	if c.route.count() {
		return Tokens{}
	}
	if u, ok := sn.Usage(); ok {
		return tokensOf(u)
	}
	return c.estimatedTokens()
}

// endStream writes the door's one error frame after a stream failed past
// its first byte, and returns the failure for the record. Nothing is
// written when the caller is gone.
func (c *call) endStream(f *failure) *failure {
	if f.code != ClientClosed {
		if frame := streamErrorFrame(c.door, c.id, f); frame != nil {
			_, _ = c.w.Write(frame)
			c.w.Flush()
		}
	}
	return f
}

// streamTranslated relays a stream through the bridge: each upstream
// event decoded with the target's codec and encoded with the door's,
// flushed per event, the Model's name written on message_start and the
// usage read from every event that carries it. A target that answered a
// stream request with one JSON body is decoded whole and re-emitted as
// the door's event sequence. The status and the headers are written
// when the first event arrives. A stream that fails past its first
// event ends with the door's one error frame, which endStream writes,
// so the bridge is given no Fail of its own: one writer of the frame,
// never two.
func (c *call) streamTranslated(ctx context.Context, t Target, resp *http.Response) *failure {
	b, f := c.bridgeFor(ctx, t)
	if f != nil {
		return f
	}
	c.w.Header().Set("Content-Type", "text/event-stream")
	opts := bridge.StreamOptions{
		Model: c.model.Metadata.Name,
		FirstByte: func() error {
			c.w.WriteHeader(http.StatusOK)
			c.w.Flush()
			return nil
		},
		Flush: c.w.Flush,
	}
	var usage bridge.Usage
	var err error
	if isSSE(resp.Header.Get("Content-Type")) {
		usage, err = b.Stream(c.w, resp.Body, opts)
	} else {
		body, f := c.readWhole(ctx, resp)
		if f != nil {
			return f
		}
		usage, err = b.StreamResponse(c.w, body, opts)
	}
	c.tokens = tokensOf(usage)
	if err != nil {
		return c.endStream(c.bridgeFailure(ctx, err))
	}
	return nil
}
