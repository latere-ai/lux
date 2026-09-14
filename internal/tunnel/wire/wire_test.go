// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCarrierFraming is spec 013's framing row at the byte level: one
// header line, then the body in chunks, in each direction, an empty
// line before the header skipped, and a body's end told from a stream
// that broke.
func TestCarrierFraming(t *testing.T) {
	var buf bytes.Buffer
	if err := Keepalive(&buf); err != nil {
		t.Fatal(err)
	}
	req := Request{ID: "req_1", Method: "POST", Path: "/chat/completions", Query: "a=1", Headers: map[string][]string{"Content-Type": {"application/json"}}, Stream: true}
	if err := WriteLine(&buf, req); err != nil {
		t.Fatal(err)
	}
	bw := NewBodyWriter(&buf)
	for _, part := range []string{`{"model":`, `"m"}`} {
		if _, err := io.WriteString(bw, part); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := bw.Write(nil); n != 0 || err != nil {
		t.Fatalf("an empty write is not a chunk: %d %v", n, err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}
	// What follows the body must stay readable: the stream is not over.
	if err := WriteLine(&buf, Response{Status: 200, Headers: map[string][]string{"Content-Type": {"text/event-stream"}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "\n{\"id\":\"req_1\"") {
		t.Fatalf("framing:\n%s", buf.String())
	}

	rd := bufio.NewReader(&buf)
	var got Request
	if err := ReadLine(rd, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "req_1" || got.Path != "/chat/completions" || got.Query != "a=1" || !got.Stream || got.Headers["Content-Type"][0] != "application/json" {
		t.Errorf("request line %+v", got)
	}
	body, err := io.ReadAll(NewBodyReader(rd))
	if err != nil || string(body) != `{"model":"m"}` {
		t.Fatalf("body %q %v", body, err)
	}
	var resp Response
	if err := ReadLine(rd, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || resp.Headers["Content-Type"][0] != "text/event-stream" {
		t.Errorf("response line %+v", resp)
	}

	// A broken stream is not a finished body.
	var truncated bytes.Buffer
	tw := NewBodyWriter(&truncated)
	_, _ = io.WriteString(tw, "data: hello\n\n")
	_, err = io.ReadAll(NewBodyReader(bufio.NewReader(&truncated)))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a truncated body read as %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestReadLineBounds: the stream ending before a line is io.EOF, a
// partial last line is io.ErrUnexpectedEOF, a line past MaxLine is
// refused, and a line that is not the type is an error naming it.
func TestReadLineBounds(t *testing.T) {
	var f Frame
	if err := ReadLine(bufio.NewReader(strings.NewReader("\n\n")), &f); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: %v", err)
	}
	if err := ReadLine(bufio.NewReader(strings.NewReader(`{"type":"ready"`)), &f); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("partial line: %v", err)
	}
	long := strings.Repeat("x", MaxLine+2) + "\n"
	if err := ReadLine(bufio.NewReaderSize(strings.NewReader(long), 4096), &f); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("long line: %v", err)
	}
	if err := ReadLine(bufio.NewReader(strings.NewReader("[1]\n")), &f); err == nil || !strings.Contains(err.Error(), "not a *wire.Frame") {
		t.Errorf("wrong shape: %v", err)
	}
	// A line longer than the reader's buffer but under the bound reads.
	f = Frame{}
	line := `{"type":"heartbeat","token":"` + strings.Repeat("t", 10000) + `"}` + "\n"
	if err := ReadLine(bufio.NewReaderSize(strings.NewReader(line), 4096), &f); err != nil || len(f.Token) != 10000 {
		t.Errorf("a long token: %v %d", err, len(f.Token))
	}
}

// TestFlushingWritesThrough: the flushing writer over a response
// recorder writes the bytes and flushes, and a plain writer is
// returned as it is.
func TestFlushingWritesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := Flushing(rec)
	if err := WriteLine(w, Frame{Type: TypeReady, Session: "tun_1", TTL: "30s", Carriers: 4}); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed || rec.Body.String() != `{"type":"ready","session":"tun_1","ttl":"30s","carriers":4}`+"\n" {
		t.Errorf("flushed %v body %q", rec.Flushed, rec.Body.String())
	}
	var buf bytes.Buffer
	if Flushing(&buf) != &buf {
		t.Error("a plain writer was wrapped")
	}
	if err := WriteLine(&buf, make(chan int)); err == nil {
		t.Error("an unencodable value was written")
	}
	// A failing writer reports its failure from every entry point.
	fw := failing{}
	if err := WriteLine(fw, Frame{}); err == nil {
		t.Error("WriteLine on a failing writer")
	}
	if err := Keepalive(fw); err == nil {
		t.Error("Keepalive on a failing writer")
	}
	bw := NewBodyWriter(fw)
	if _, err := bw.Write([]byte("x")); err == nil {
		t.Error("body Write on a failing writer")
	}
	if err := bw.Close(); err == nil {
		t.Error("body Close on a failing writer")
	}
	// A writer whose Flush fails reports that too.
	if err := WriteLine(flushFails{}, Frame{}); err == nil {
		t.Error("a failing Flush was swallowed")
	}
	if err := Keepalive(flushFails{}); err == nil {
		t.Error("a failing Flush on a keepalive was swallowed")
	}
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("closed") }

type flushFails struct{}

func (flushFails) Write(p []byte) (int, error) { return len(p), nil }
func (flushFails) Flush() error                { return errors.New("flush failed") }
