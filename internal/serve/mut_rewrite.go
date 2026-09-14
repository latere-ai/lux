// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_default || mut_keyvalue

package serve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
)

// rewriteResponses is the shape two mutation shims of spec 018 share: a
// middleware that buffers a /v1 answer and lets edit change its JSON
// body before it reaches the caller. It is compiled under those two
// tags alone.
func rewriteResponses(h http.Handler, edit func(r *http.Request, status int, body map[string]any) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &bufferedWriter{header: http.Header{}, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		var tree map[string]any
		if json.Unmarshal(body, &tree) == nil && edit(r, rec.status, tree) {
			body, _ = json.Marshal(tree)
			body = append(body, '\n')
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

// bufferedWriter holds one answer whole.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header         { return b.header }
func (b *bufferedWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.body.Write(p) }
