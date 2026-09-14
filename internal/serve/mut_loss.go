// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_loss

package serve

import (
	"net/http"

	"latere.ai/x/lux/gateway"
)

// The mutation of spec 018's third row: the translation loss report is
// dropped from every door answer. Only case004TranslationLoss may
// redden.
func init() {
	mutateHandler = func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(&lossless{ResponseWriter: w}, r)
		})
	}
}

// lossless drops Lux-Loss as the header is committed.
type lossless struct {
	http.ResponseWriter
}

func (l *lossless) WriteHeader(code int) {
	l.Header().Del(gateway.HeaderLoss)
	l.ResponseWriter.WriteHeader(code)
}

func (l *lossless) Write(p []byte) (int, error) {
	l.Header().Del(gateway.HeaderLoss)
	return l.ResponseWriter.Write(p)
}

func (l *lossless) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (l *lossless) Unwrap() http.ResponseWriter { return l.ResponseWriter }
