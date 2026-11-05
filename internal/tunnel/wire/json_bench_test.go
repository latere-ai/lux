// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"testing"
)

// The benchmarks below measure the JSON cost of the tunnel protocol's
// lines: the gateway writes a Request line onto a carrier for every
// proxied call and reads the Response line back, and both sides pass
// Frames on the session stream.

func jsonBenchRequest() Request {
	return Request{
		ID: "tun_01JBENCH0000000000000000", Method: "POST", Path: "/v1/chat/completions", Query: "",
		Headers: map[string][]string{
			"Content-Type":    {"application/json"},
			"Accept":          {"text/event-stream"},
			"Authorization":   {"Bearer sk-bench-0000000000000000000000000000"},
			"User-Agent":      {"lux-gateway/0.50.0"},
			"X-Request-Id":    {"req_01JBENCH0000000000000000"},
			"Accept-Encoding": {"gzip"},
			"Traceparent":     {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		},
		Stream: true,
	}
}

func jsonBenchResponse() Response {
	return Response{
		Status: 200,
		Headers: map[string][]string{
			"Content-Type":  {"text/event-stream"},
			"Cache-Control": {"no-cache"},
			"Date":          {"Mon, 14 Sep 2026 10:30:00 GMT"},
			"X-Request-Id":  {"req_01JBENCH0000000000000000"},
		},
	}
}

// jsonBenchLine writes v as a line and reads it back into out.
func jsonBenchLine(b *testing.B, v, out any) int {
	b.Helper()
	var buf bytes.Buffer
	if err := WriteLine(&buf, v); err != nil {
		b.Fatal(err)
	}
	n := buf.Len()
	if err := ReadLine(bufio.NewReader(&buf), out); err != nil {
		b.Fatal(err)
	}
	return n
}

// BenchmarkJSONCarrierRequestEncode is the Request header line the
// gateway writes onto a carrier for one proxied call.
func BenchmarkJSONCarrierRequestEncode(b *testing.B) {
	req := jsonBenchRequest()
	var probe Request
	n := jsonBenchLine(b, req, &probe)
	var buf bytes.Buffer
	b.ReportAllocs()
	b.SetBytes(int64(n))
	for b.Loop() {
		buf.Reset()
		if err := WriteLine(&buf, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONCarrierRequestDecode is the agent's read of that line.
func BenchmarkJSONCarrierRequestDecode(b *testing.B) {
	var line bytes.Buffer
	if err := WriteLine(&line, jsonBenchRequest()); err != nil {
		b.Fatal(err)
	}
	data := line.Bytes()
	r := bytes.NewReader(data)
	br := bufio.NewReader(r)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r.Reset(data)
		br.Reset(r)
		var req Request
		if err := ReadLine(br, &req); err != nil {
			b.Fatal(err)
		}
		if req.ID == "" {
			b.Fatal("the line decoded empty")
		}
	}
}

// BenchmarkJSONCarrierResponseRoundTrip is the Response line the agent
// writes back and the gateway reads.
func BenchmarkJSONCarrierResponseRoundTrip(b *testing.B) {
	resp := jsonBenchResponse()
	var probe Response
	n := jsonBenchLine(b, resp, &probe)
	var buf bytes.Buffer
	br := bufio.NewReader(&buf)
	b.ReportAllocs()
	b.SetBytes(int64(n))
	for b.Loop() {
		buf.Reset()
		br.Reset(&buf)
		if err := WriteLine(&buf, resp); err != nil {
			b.Fatal(err)
		}
		var out Response
		if err := ReadLine(br, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONSessionFrameRoundTrip is a heartbeat frame of the
// session stream, written by the agent and read by the gateway.
func BenchmarkJSONSessionFrameRoundTrip(b *testing.B) {
	frame := Frame{Type: TypeHeartbeat, Token: "eyJhbGciOiJSUzI1NiIsImtpZCI6ImJlbmNoIn0.eyJzdWIiOiI3YzllNjY3OSIsImV4cCI6MTc5MDAwMDAwMH0.c2lnbmF0dXJlLWJ5dGVzLWZvci1iZW5jaA"}
	var probe Frame
	n := jsonBenchLine(b, frame, &probe)
	var buf bytes.Buffer
	br := bufio.NewReader(&buf)
	b.ReportAllocs()
	b.SetBytes(int64(n))
	for b.Loop() {
		buf.Reset()
		br.Reset(&buf)
		if err := WriteLine(&buf, frame); err != nil {
			b.Fatal(err)
		}
		var out Frame
		if err := ReadLine(br, &out); err != nil {
			b.Fatal(err)
		}
	}
}
