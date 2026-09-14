// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The defaults of the deterministic answer.
const (
	// DefaultCredential is the value the stub expects in the dialect's
	// credential header when Options names none, and the value the
	// examples under deploy/examples carry.
	DefaultCredential = "stub-credential"
	// DefaultInputTokens and DefaultOutputTokens are the usage the
	// answer reports unless a tokens-<in>-<out> name says otherwise.
	DefaultInputTokens  int64 = 100
	DefaultOutputTokens int64 = 20
	// DefaultEvents is how many content events a streamed answer carries
	// unless an events-<n> name says otherwise.
	DefaultEvents = 5
	// HeaderFail is the header that selects a behaviour without a model
	// name, for a test driving the stub directly.
	HeaderFail = "Lux-Stub-Fail"
	// maxBody bounds one recorded request body.
	maxBody = 64 << 20
	// controlPrefix is where the control routes live; no dialect route
	// begins with it.
	controlPrefix = "/_"
	// receivedPath is the record's route.
	receivedPath = "/_received"
)

// Options is what New builds a stub under.
type Options struct {
	// Dialect selects the routes and the shapes. Required.
	Dialect v1.Dialect
	// Credential is the value the dialect's credential header must carry;
	// empty is DefaultCredential.
	Credential string
}

// Received is one request as the stub saw it: the method, the path, the
// query, every header, and the body as text.
type Received struct {
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Query  string      `json:"query"`
	Header http.Header `json:"header"`
	Body   string      `json:"body"`
}

// Stub is one stub provider. It is an http.Handler.
type Stub struct {
	o   Options
	mux *http.ServeMux

	mu       sync.Mutex
	received []Received
}

// New builds a stub for the dialect. A dialect outside the four is a
// panic, because a stub with no route table serves nothing a test could
// assert.
func New(o Options) *Stub {
	if !o.Dialect.Valid() {
		panic("provider.New: dialect " + string(o.Dialect) + " is not one of openai, anthropic, gemini, lux")
	}
	if o.Credential == "" {
		o.Credential = DefaultCredential
	}
	s := &Stub{o: o, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Dialect is the dialect the stub serves.
func (s *Stub) Dialect() v1.Dialect { return s.o.Dialect }

// Received lists every request the stub has served since the last
// Reset, in order, the control routes excepted. An empty record is an
// empty list, never nil.
func (s *Stub) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.received == nil {
		return []Received{}
	}
	return slices.Clone(s.received)
}

// Reset clears the record; DELETE /_received is the same over HTTP.
func (s *Stub) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = nil
}

// ServeHTTP records the request, checks the credential, and dispatches
// on the dialect's route table. A control route is neither recorded nor
// checked.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, controlPrefix) {
		s.control(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the body could not be read: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.received = append(s.received, Received{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(body)})
	s.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if !s.credentialOK(r) {
		s.refuse(w, http.StatusUnauthorized, "the credential is not the one this stub was started with")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// credentialOK compares the dialect's default credential header with the
// configured value, in constant time: Authorization: Bearer for openai
// and lux, x-api-key for anthropic, x-goog-api-key for gemini.
func (s *Stub) credentialOK(r *http.Request) bool {
	got := r.Header.Get(s.o.Dialect.CredentialHeader())
	if s.o.Dialect.CredentialScheme() == v1.SchemeBearer {
		var ok bool
		if got, ok = strings.CutPrefix(got, "Bearer "); !ok {
			return false
		}
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.o.Credential)) == 1
}

// control serves the routes under /_.
func (s *Stub) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == receivedPath && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Received())
	case r.URL.Path == receivedPath && r.Method == http.MethodDelete:
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, r.Method+" "+r.URL.Path+" is no control route; GET and DELETE /_received are", http.StatusNotFound)
	}
}

// routes registers the dialect's rows of spec 005's table and the
// catch-all that records an opaque request and answers 200 {}.
func (s *Stub) routes() {
	switch s.o.Dialect {
	case v1.DialectOpenAI:
		s.mux.HandleFunc("POST /v1/chat/completions", s.handle(routeChat))
		s.mux.HandleFunc("POST /v1/responses", s.handle(routeResponses))
		s.mux.HandleFunc("POST /v1/embeddings", s.handle(routeEmbeddings))
		s.mux.HandleFunc("GET /v1/models", s.handle(routeModels))
	case v1.DialectAnthropic:
		s.mux.HandleFunc("POST /v1/messages", s.handle(routeMessages))
		s.mux.HandleFunc("POST /v1/messages/count_tokens", s.handle(routeAnthropicCount))
		s.mux.HandleFunc("GET /v1/models", s.handle(routeModels))
	case v1.DialectGemini:
		s.mux.HandleFunc("POST /v1beta/models/{model...}", s.handle(routeGemini))
		s.mux.HandleFunc("GET /v1beta/models", s.handle(routeModels))
	case v1.DialectLux:
		s.mux.HandleFunc("POST /v1/generate", s.handle(routeGenerate))
		s.mux.HandleFunc("POST /v1/count_tokens", s.handle(routeLuxCount))
		s.mux.HandleFunc("GET /v1/models", s.handle(routeModels))
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{}) })
}

// handle answers one route: the request is read, the behaviour chosen
// from the header or the upstream model name, and the answer rendered
// in the route's shape.
func (s *Stub) handle(rt routeKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind := rt
		body, _ := io.ReadAll(r.Body)
		req := parseRequest(s.o.Dialect, kind, r, body)
		if kind == routeGemini {
			var ok bool
			if kind, ok = geminiRoute(req.verb); !ok {
				s.refuse(w, http.StatusNotFound, "no such method on a model: "+req.verb)
				return
			}
		}
		name := r.Header.Get(HeaderFail)
		if name == "" {
			name = req.model
		}
		s.answer(w, r, kind, req, ParseBehaviour(name))
	}
}

// answer runs the behaviour: a refusal, a wait, a hang, a broken stream,
// or the deterministic answer, whole or streamed.
func (s *Stub) answer(w http.ResponseWriter, r *http.Request, rt routeKind, req parsed, b Behaviour) {
	switch b.Kind {
	case KindFail500:
		s.refuse(w, http.StatusInternalServerError, "the stub was asked to fail with 500")
		return
	case KindFail429:
		w.Header().Set("Retry-After", "1")
		s.refuse(w, http.StatusTooManyRequests, "the stub was asked to fail with 429")
		return
	case KindFail401:
		s.refuse(w, http.StatusUnauthorized, "the credential is not the one this stub was started with")
		return
	case KindFail400:
		s.refuse(w, http.StatusBadRequest, "the stub was asked to reject the request")
		return
	case KindFail529:
		s.refuse(w, 529, "the stub was asked to report itself overloaded")
		return
	case KindFailHTML:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><html><body><h1>stub</h1><p>a page where a body was expected</p></body></html>\n")
		return
	case KindFailBody:
		writeJSON(w, http.StatusOK, map[string]any{"stub": "a body that is not the dialect's shape"})
		return
	case KindRedirect:
		w.Header().Set("Location", "http://elsewhere.example.com"+r.URL.Path)
		w.WriteHeader(http.StatusFound)
		return
	case KindHang:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flush(w)
		<-r.Context().Done()
		return
	case KindSlow:
		t := time.NewTimer(b.Wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done():
			return
		}
	case KindFailStreamMid:
		if rt == routeModels {
			break
		}
		st := newStreamer(w, s.o.Dialect, rt, req, b)
		st.begin()
		st.content(0)
		st.content(1)
		flush(w)
		// The one way a handler closes the connection without a
		// terminating chunk: the server drops it and logs nothing.
		panic(http.ErrAbortHandler)
	case KindNormal, KindTokens, KindEvents:
	}
	if rt == routeModels {
		writeJSON(w, http.StatusOK, modelList(s.o.Dialect))
		return
	}
	if req.stream && streams(rt) {
		st := newStreamer(w, s.o.Dialect, rt, req, b)
		st.begin()
		for i := range b.Events {
			st.content(i)
		}
		st.finish()
		return
	}
	writeJSON(w, http.StatusOK, whole(s.o.Dialect, rt, req, b))
}

// refuse writes the dialect's error body for status.
func (s *Stub) refuse(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorBody(s.o.Dialect, status, message))
}

// writeJSON writes v as one JSON body. The maps this package builds
// marshal with sorted keys, so one request twice yields identical bytes.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// flush pushes what was written to the connection; a writer that cannot
// flush is left to its own pace.
func flush(w http.ResponseWriter) {
	_ = http.NewResponseController(w).Flush()
}
