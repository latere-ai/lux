// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	v1 "latere.ai/x/lux/manifest/v1"
)

// stubProvider is one stub upstream of a dialect for the package's own
// tests, speaking the contract spec 015's provider stub will speak: the
// dialect's translated and model routes, a model list, a catch-all that
// answers 200 {}, deterministic content and usage, failure injection by
// upstream model name, and GET and DELETE /_received. It checks the
// credential the dialect defaults to against its own and answers 401,
// recorded, to anything else.
type stubProvider struct {
	dialect    string
	credential string
	srv        *httptest.Server

	mu       sync.Mutex
	received []received
}

// The fixed usage every stub answer reports unless the upstream model
// name asks otherwise.
const (
	stubInputTokens  = 100
	stubOutputTokens = 20
	stubEvents       = 5
)

func newStubProvider(t testing.TB, dialect, credential string) *stubProvider {
	t.Helper()
	s := &stubProvider{dialect: dialect, credential: credential}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubProvider) URL() string { return s.srv.URL }

// ServeHTTP records the request, checks the credential, and answers by
// route and by the behaviour the upstream model name asks for.
func (s *stubProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_received" {
		s.control(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.received = append(s.received, received{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: r.Header.Clone(), Body: string(body)})
	s.mu.Unlock()
	d := v1.Dialect(s.dialect)
	want := s.credential
	if d.CredentialScheme() == v1.SchemeBearer {
		want = "Bearer " + s.credential
	}
	if r.Header.Get(d.CredentialHeader()) != want {
		s.failure(w, http.StatusUnauthorized, "the credential is not the stub's")
		return
	}
	model := s.modelOf(r, body)
	in, out, events := stubInputTokens, stubOutputTokens, stubEvents
	switch {
	case model == "fail-500":
		s.failure(w, http.StatusInternalServerError, "stub failure")
		return
	case model == "fail-400":
		s.failure(w, http.StatusBadRequest, "stub failure")
		return
	case model == "fail-401":
		s.failure(w, http.StatusUnauthorized, "stub failure")
		return
	case model == "fail-429":
		w.Header().Set("Retry-After", "1")
		s.failure(w, http.StatusTooManyRequests, "stub failure")
		return
	case model == "hang":
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush() //nolint:errcheck // a flush the test cannot act on
		<-r.Context().Done()
		return
	case strings.HasPrefix(model, "slow-"):
		if d, err := time.ParseDuration(strings.TrimPrefix(model, "slow-")); err == nil {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
	case strings.HasPrefix(model, "tokens-"):
		parts := strings.Split(strings.TrimPrefix(model, "tokens-"), "-")
		if len(parts) == 2 {
			in, _ = strconv.Atoi(parts[0])
			out, _ = strconv.Atoi(parts[1])
		}
	case strings.HasPrefix(model, "events-"):
		events, _ = strconv.Atoi(strings.TrimPrefix(model, "events-"))
	}
	content := "stub:" + s.dialect + ":" + model + ":" + digest(body)
	stream := strings.Contains(string(body), `"stream":true`) || strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
	s.answer(w, r, model, content, in, out, events, stream)
}

// control is GET and DELETE /_received.
func (s *stubProvider) control(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		out := s.received
		if out == nil {
			out = []received{}
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodDelete:
		s.received = nil
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// modelOf reads the upstream model name from the body or, on the gemini
// dialect, from the path.
func (s *stubProvider) modelOf(r *http.Request, body []byte) string {
	if s.dialect == "gemini" {
		rest := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
		if i := strings.LastIndexByte(rest, ':'); i > 0 {
			return rest[:i]
		}
		return rest
	}
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// digest is the first eight hex characters of the SHA-256 of the body.
func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:8]
}

// failure writes the dialect's error body at the status.
func (s *stubProvider) failure(w http.ResponseWriter, status int, message string) {
	var body string
	switch s.dialect {
	case "anthropic":
		body = `{"type":"error","error":{"type":"api_error","message":"` + message + `"}}`
	case "gemini":
		body = `{"error":{"code":` + strconv.Itoa(status) + `,"message":"` + message + `","status":"INTERNAL"}}`
	case "lux":
		body = `{"error":{"code":"internal","message":"` + message + `."}}`
	default:
		body = `{"error":{"message":"` + message + `","type":"server_error","code":null}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// answer writes the route's deterministic answer.
func (s *stubProvider) answer(w http.ResponseWriter, r *http.Request, model, content string, in, out, events int, stream bool) {
	path := r.URL.Path
	writeJSON := func(v string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, v)
	}
	switch s.dialect {
	case "openai":
		switch path {
		case "/v1/chat/completions":
			if stream {
				s.sse(w, r, openaiChunks(model, content, in, out, events))
				return
			}
			writeJSON(`{"id":"chatcmpl-stub","object":"chat.completion","created":0,"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` + itoa(in) + `,"completion_tokens":` + itoa(out) + `}}`)
		case "/v1/responses":
			writeJSON(`{"id":"resp_stub","object":"response","model":"` + model + `","status":"completed","output":[{"type":"message","id":"msg_stub","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + content + `","annotations":[]}]}],"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `}}`)
		case "/v1/embeddings":
			writeJSON(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"` + model + `","usage":{"prompt_tokens":` + itoa(in) + `,"total_tokens":` + itoa(in) + `}}`)
		case "/v1/models":
			writeJSON(`{"object":"list","data":[{"id":"stub-gpt","object":"model"}]}`)
		default:
			writeJSON(`{}`)
		}
	case "anthropic":
		switch path {
		case "/v1/messages":
			if stream {
				s.sse(w, r, anthropicEvents(model, content, in, out, events))
				return
			}
			writeJSON(`{"id":"msg_stub","type":"message","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"` + content + `"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `}}`)
		case "/v1/messages/count_tokens":
			writeJSON(`{"input_tokens":` + itoa(in) + `}`)
		case "/v1/models":
			writeJSON(`{"data":[{"type":"model","id":"stub-claude"}],"has_more":false}`)
		default:
			writeJSON(`{}`)
		}
	case "gemini":
		verb := ""
		if i := strings.LastIndexByte(path, ':'); i > 0 {
			verb = path[i+1:]
		}
		chunk := func(text string, cand int) string {
			return `{"candidates":[{"content":{"parts":[{"text":"` + text + `"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":` + itoa(in) + `,"candidatesTokenCount":` + itoa(cand) + `},"modelVersion":"` + model + `"}`
		}
		switch {
		case verb == "generateContent":
			writeJSON(chunk(content, out))
		case verb == "streamGenerateContent":
			var chunks []string
			for i := range events {
				chunks = append(chunks, chunk("x", (out*(i+1))/events))
			}
			if r.URL.Query().Get("alt") == "sse" {
				frames := make([]string, 0, len(chunks))
				for _, c := range chunks {
					frames = append(frames, "data: "+c+"\n\n")
				}
				s.sse(w, r, frames)
				return
			}
			writeJSON("[" + strings.Join(chunks, ",\n") + "]")
		case verb == "countTokens":
			writeJSON(`{"totalTokens":` + itoa(in) + `}`)
		case verb == "embedContent":
			writeJSON(`{"embedding":{"values":[0.1,0.2,0.3]}}`)
		case path == "/v1beta/models":
			writeJSON(`{"models":[{"name":"models/stub-gemini","supportedGenerationMethods":["generateContent","countTokens"]}]}`)
		default:
			writeJSON(`{}`)
		}
	case "lux":
		switch path {
		case "/v1/generate":
			if stream {
				s.sse(w, r, luxEvents(model, content, in, out, events))
				return
			}
			writeJSON(`{"id":"lux-stub","model":"` + model + `","blocks":[{"type":"text","text":"` + content + `"}],"stop_reason":"end_turn","usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `}}`)
		case "/v1/count_tokens":
			writeJSON(`{"input_tokens":` + itoa(in) + `}`)
		case "/v1/models":
			writeJSON(`{"object":"list","data":[{"id":"stub-lux","object":"model"}]}`)
		default:
			writeJSON(`{}`)
		}
	}
}

// sse writes frames one by one, flushing each.
func (s *stubProvider) sse(w http.ResponseWriter, r *http.Request, frames []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for _, f := range frames {
		if r.Context().Err() != nil {
			return
		}
		_, _ = io.WriteString(w, f)
		_ = rc.Flush()
	}
}

// openaiChunks are events content chunks, one usage chunk, and [DONE].
func openaiChunks(model, content string, in, out, events int) []string {
	var frames []string
	for range events {
		frames = append(frames, `data: {"id":"chatcmpl-stub","object":"chat.completion.chunk","created":0,"model":"`+model+`","choices":[{"index":0,"delta":{"content":"`+content[:1]+`"},"finish_reason":null}]}`+"\n\n")
	}
	frames = append(frames, `data: {"id":"chatcmpl-stub","object":"chat.completion.chunk","created":0,"model":"`+model+`","choices":[],"usage":{"prompt_tokens":`+itoa(in)+`,"completion_tokens":`+itoa(out)+`}}`+"\n\n")
	return append(frames, "data: [DONE]\n\n")
}

// anthropicEvents is the Messages stream: message_start with the input,
// one text block of events deltas, message_delta with the output.
func anthropicEvents(model, content string, in, out, events int) []string {
	frames := []string{
		"event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_stub","type":"message","role":"assistant","model":"` + model + `","content":[],"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":1}}}` + "\n\n",
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
	}
	for range events {
		frames = append(frames, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+content[:1]+`"}}`+"\n\n")
	}
	return append(frames,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`+"\n\n",
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":`+itoa(out)+`}}`+"\n\n",
		"event: message_stop\ndata: "+`{"type":"message_stop"}`+"\n\n",
	)
}

// luxEvents is the lux stream in the same sequence.
func luxEvents(model, content string, in, out, events int) []string {
	frames := []string{"event: message_start\ndata: " + `{"type":"message_start","id":"lux-stub","model":"` + model + `","usage":{"input_tokens":` + itoa(in) + `}}` + "\n\n",
		"event: block_start\ndata: " + `{"type":"block_start","index":0,"block":{"type":"text"}}` + "\n\n"}
	for range events {
		frames = append(frames, "event: text_delta\ndata: "+`{"type":"text_delta","index":0,"delta":"`+content[:1]+`"}`+"\n\n")
	}
	return append(frames,
		"event: block_stop\ndata: "+`{"type":"block_stop","index":0}`+"\n\n",
		"event: message_delta\ndata: "+`{"type":"message_delta","stop_reason":"end_turn","usage":{"output_tokens":`+itoa(out)+`}}`+"\n\n",
		"event: message_stop\ndata: "+`{"type":"message_stop"}`+"\n\n",
	)
}

// stubSet is the four stub providers, the stub issuer and authorizer,
// and the document at GET / naming each, as lux-stubs serves it.
type stubSet struct {
	providers map[string]*stubProvider
	doc       *httptest.Server
	iss       *issuertest.Server
	az        *stub.Server
}

// stubCredential is the value the harness's stub providers check.
const stubCredential = "sk-stub-credential-for-the-conformance-harness"

// newStubSet starts one provider per dialect checking stubCredential,
// and the document naming them all.
func newStubSet(t testing.TB, iss *issuertest.Server, az *stub.Server) *stubSet {
	t.Helper()
	set := &stubSet{providers: map[string]*stubProvider{}, iss: iss, az: az}
	doc := stubs{Providers: map[string]string{}, Credential: stubCredential}
	for _, d := range []string{"openai", "anthropic", "gemini", "lux"} {
		set.providers[d] = newStubProvider(t, d, stubCredential)
		doc.Providers[d] = set.providers[d].URL()
	}
	if iss != nil {
		doc.Issuer = iss.URL()
	}
	if az != nil {
		doc.Authorizer = az.URL()
	}
	set.doc = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(set.doc.Close)
	return set
}

// URL is the stubs document's address, the value of LUX_TEST_STUBS_URL.
func (s *stubSet) URL() string { return s.doc.URL }
