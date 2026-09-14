// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"io"
	"net/http"
	"strings"

	"latere.ai/x/pkg/bearer"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// ForwardPattern is the internal route's pattern, with the Provider id
// as {id}.
const ForwardPattern = "/internal/tunnel/{id}"

// Forward is POST /internal/tunnel/{provider id} on the internal
// listener: one proxied request forwarded from another replica, with an
// entry of LUX_TUNNEL_FORWARD_SECRET as the bearer and the carrier's
// framing on both bodies. The secret is checked before any body byte is
// read, so a refused entry is retried by the forwarder with the body
// untouched; a session this replica does not hold is
// provider_unavailable and is forwarded nowhere, so a row that moved
// costs one refusal and never a loop. Once the header line is read the
// response is committed with 200, and a failure after that travels in
// the response line's error member.
func (g *Gateway) Forward() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(gateway.HeaderRequestID)
		if id == "" {
			id = g.o.NewID(v1.PrefixRequest)
		}
		w.Header().Set(gateway.HeaderRequestID, id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodPost {
			writeError(w, id, refuse(CodeNotFound, r.Method+" "+r.URL.Path+" is not in the route table"))
			return
		}
		if r.ProtoMajor < 2 {
			writeError(w, id, refuse(CodeNotFound, "the tunnel needs HTTP/2; this connect negotiated "+r.Proto))
			return
		}
		token, ok := bearer.FromRequest(r)
		if !ok || !g.acceptsSecret(token) {
			writeError(w, id, refuse(CodeUnauthenticated, "the bearer is not an entry of LUX_TUNNEL_FORWARD_SECRET on this replica"))
			return
		}
		providerID := r.PathValue("id")
		if providerID == "" {
			providerID = strings.TrimPrefix(r.URL.Path, "/internal/tunnel/")
		}
		s := g.session(providerID)
		if s == nil {
			writeError(w, id, refuse(CodeProviderUnavailable, "this replica holds no session for Provider "+providerID+"; the request was not forwarded again"))
			return
		}
		rd := bufio.NewReaderSize(r.Body, 64<<10)
		var req wire.Request
		if err := wire.ReadLine(rd, &req); err != nil {
			writeError(w, id, refuse(CodeInvalidRequest, "the forwarded request has no header line: "+err.Error()))
			return
		}
		// Accepted: the forwarder streams the body once it sees the 200.
		h := w.Header()
		h.Set("Content-Type", "application/x-ndjson")
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		out := wire.Flushing(w)
		if err := wire.Keepalive(out); err != nil {
			return
		}
		resp, body, err := s.exchange(r.Context(), req, wire.NewBodyReader(rd))
		if err != nil {
			_ = wire.WriteLine(out, wire.Response{Error: err.Error()})
			return
		}
		defer func() { _ = body.Close() }()
		if err := wire.WriteLine(out, resp); err != nil {
			return
		}
		bw := wire.NewBodyWriter(out)
		if _, err := io.Copy(bw, body); err != nil {
			// The stream to the forwarder ends short of the last chunk,
			// which its reader tells from a finished body.
			return
		}
		_ = bw.Close()
	})
}
