// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/tokencount"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The defaults of the two variables spec 004 owns, applied when Options
// leaves them zero.
const (
	DefaultMaxBodyBytes    int64 = 64 << 20
	DefaultUpstreamTimeout       = 10 * time.Minute
)

// defaultOutputTokens is the output reservation of a request that names
// no maximum, spec 007's 1024.
const defaultOutputTokens = 1024

// Handler is the data plane: one http.Handler mounted at the four doors.
// It computes and drives; it holds no store, no identity, and no HTTP
// server of its own.
type Handler struct {
	o       Options
	now     func() time.Time
	newID   func() string
	maxBody int64
	ua      string
	metrics *requestMetrics
}

// New builds the handler. A nil required option is a panic, because a
// door that cannot look up a Key, a Model, a client, or a credential
// cannot answer anything and should not start.
func New(o Options) *Handler {
	required := []struct {
		name string
		nil  bool
	}{
		{"Keys", o.Keys == nil}, {"Catalog", o.Catalog == nil}, {"Credentials", o.Credentials == nil},
		{"Router", o.Router == nil}, {"Clients", o.Clients == nil},
	}
	for _, r := range required {
		if r.nil {
			panic("gateway.New: Options." + r.name + " is nil")
		}
	}
	h := &Handler{o: o, now: o.Now, newID: o.NewID, maxBody: o.MaxBodyBytes, metrics: newRequestMetrics(o.Metrics)}
	if h.now == nil {
		h.now = time.Now
	}
	if h.newID == nil {
		h.newID = func() string { return v1.NewID(v1.PrefixRequest, h.now(), nil) }
	}
	if h.maxBody <= 0 {
		h.maxBody = DefaultMaxBodyBytes
	}
	version := o.Version
	if version == "" {
		version = "dev"
	}
	h.ua = "luxd/" + version
	return h
}

// mode is how a request reaches its target: byte for byte, translated
// through the codecs, or not at all, answered from the estimate.
type mode int

const (
	modePassthrough mode = iota
	modeTranslate
	modeEstimate
)

// call is one request through the pipeline.
type call struct {
	h     *Handler
	w     *responseWriter
	r     *http.Request
	id    string
	start time.Time

	door      v1.Dialect
	route     route
	key       *v1.Key
	body      []byte
	probe     probe
	model     *v1.Model
	targets   []Target
	irReq     *ir.Request // the decoded request, when a translation or an estimate needs one
	decodeErr error
	lease     Lease
	tokens    Tokens
	rec       Record
}

// ServeHTTP runs the pipeline, answers the caller, settles the windows,
// and writes the record.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &call{h: h, r: r, id: h.newID(), start: h.now()}
	c.w = &responseWriter{ResponseWriter: w, rc: http.NewResponseController(w), now: h.now}
	hdr := w.Header()
	hdr.Set(HeaderRequestID, c.id)
	hdr.Set("X-Content-Type-Options", "nosniff")
	if xid := r.Header.Get("X-Request-Id"); echoable(xid) {
		hdr.Set("X-Request-Id", xid)
	}
	c.rec = Record{ID: c.id, At: c.start}
	ctx := r.Context()
	c.finish(ctx, c.run(ctx))
}

// echoable is spec 011's rule for X-Request-Id: at most 128 bytes of
// printable ASCII, or the header is dropped rather than echoed.
func echoable(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// finish answers a failure the pipeline returned, settles the lease, and
// writes the record. A failure after the first byte has already ended
// the stream with the door's frame; one before it is the envelope.
func (c *call) finish(ctx context.Context, f *failure) {
	switch {
	case f == nil:
		c.rec.Status = StatusOK
	case f.code == ClientClosed:
		c.rec.Status, c.rec.Error = StatusFailed, ClientClosed
	default:
		c.rec.Error = f.code
		if len(c.rec.Attempts) > 0 {
			c.rec.Status = StatusFailed
		} else {
			c.rec.Status = StatusRefused
		}
		if !c.w.committed {
			writeFailure(c.w, c.door, c.id, f)
		}
	}
	if c.lease != nil {
		c.lease.Settle(context.WithoutCancel(ctx), c.tokens)
	}
	c.rec.EndedAt = c.h.now()
	c.rec.Latency = c.rec.EndedAt.Sub(c.start)
	if !c.w.first.IsZero() {
		c.rec.TTFB = c.w.first.Sub(c.start)
	}
	c.rec.Tokens = c.tokens
	c.h.metrics.observe(c.rec)
	if c.h.o.Recorder != nil {
		c.h.o.Recorder.Record(c.rec)
	}
}

// run is the pipeline, each stage total before the next begins.
func (c *call) run(ctx context.Context) *failure {
	c.door, _ = door(c.r.URL.Path)
	c.rec.Door = c.door
	rt, f := match(c.r.Method, c.r.URL.Path)
	if f != nil {
		return f
	}
	c.route = rt
	c.rec.Route, c.rec.Class = rt.template, rt.class
	c.rec.RequestLabels = requestLabels(c.r.Header.Get(HeaderLabels))
	if f := c.readBody(); f != nil {
		return f
	}
	if f := c.authenticate(ctx); f != nil {
		return f
	}
	if rt.class == ClassOpaque && !c.key.Spec.Passthrough {
		return fail(CodeRouteNotAllowed, "Key "+c.key.Status.ID+" has passthrough false and "+rt.rest+" is an opaque route")
	}
	switch rt.op {
	case opModelsList:
		return c.listModels(ctx)
	case opModelsRead:
		return c.readModel(ctx)
	case opOpaque:
		return c.opaque(ctx)
	case opChatCompletions, opResponses, opEmbeddings, opMessages, opAnthropicCount, opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed, opGenerate, opLuxCount, opNone:
	}
	if f := c.resolveModel(ctx); f != nil {
		return f
	}
	if f := c.selectTargets(ctx); f != nil {
		return f
	}
	if f := c.reserve(ctx, Reservation{Key: c.key, Model: c.model}); f != nil {
		return f
	}
	return c.forward(ctx)
}

// readBody is stage 2. A translated or model route reads the whole body
// within the limit, because the probe reads it and a second target may
// replay it; an opaque route streams it through under the same limit.
// A Content-Length above the limit is refused before a byte is read.
func (c *call) readBody() *failure {
	limit := c.h.maxBody
	if c.r.ContentLength > limit {
		return fail(CodeBodyTooLarge, "Content-Length "+strconv.FormatInt(c.r.ContentLength, 10)+" is above the limit of "+strconv.FormatInt(limit, 10)+" bytes")
	}
	switch c.route.class {
	case ClassServed:
		return nil
	case ClassOpaque:
		c.r.Body = http.MaxBytesReader(c.w.ResponseWriter, c.r.Body, limit)
		return nil
	case ClassTranslated, ClassModel:
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.w.ResponseWriter, c.r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			return fail(CodeBodyTooLarge, "the body crossed the limit of "+strconv.FormatInt(limit, 10)+" bytes")
		case c.r.Context().Err() != nil:
			return fail(ClientClosed, "")
		}
		return fail(CodeInvalidRequest, "reading the body: "+err.Error())
	}
	c.body = body
	return nil
}

// authenticate is stage 3: the credential, its hash, the Key, its state.
func (c *call) authenticate(ctx context.Context) *failure {
	value, ok := credential(c.r)
	if !ok {
		return fail(CodeUnauthenticated, "no credential: Authorization: Bearer, x-api-key, x-goog-api-key, or the query parameter key")
	}
	k, err := c.h.o.Keys.ByHash(ctx, hashValue(value))
	if err != nil {
		return fail(CodeStoreUnavailable, "Key lookup: "+err.Error())
	}
	if k == nil {
		return fail(CodeUnauthenticated, "no Key has the presented value")
	}
	c.key = k
	c.rec.KeyID, c.rec.KeyPrefix, c.rec.Owner = k.Status.ID, k.Status.Prefix, k.Status.Owner
	if len(k.Metadata.Labels) > 0 {
		c.rec.Labels = maps.Clone(k.Metadata.Labels)
	}
	return keyState(k, c.h.now())
}

// resolveModel is stage 5: the name from the path or the probe, the
// Model by exact name, the Key's selectors.
func (c *call) resolveModel(ctx context.Context) *failure {
	p, err := probeBody(c.body)
	if err != nil {
		return fail(CodeInvalidRequest, err.Error())
	}
	c.probe = p
	c.rec.Stream = p.stream || c.route.op == opGeminiStream
	name := c.route.model
	if c.route.modelFromBody() {
		if !p.hasModel {
			return fail(CodeInvalidRequest, "the body has no string model member")
		}
		name = p.model
	}
	m, f := c.lookupModel(ctx, name)
	if f != nil {
		return f
	}
	c.model = m
	return nil
}

// lookupModel resolves a name against the catalog and holds it to the
// Key's selectors: model_not_found before model_not_allowed, so a caller
// learns whether the name exists before whether this Key may use it.
func (c *call) lookupModel(ctx context.Context, name string) (*v1.Model, *failure) {
	m, err := c.h.o.Catalog.Model(ctx, name)
	if err != nil {
		return nil, fail(CodeStoreUnavailable, "Model lookup: "+err.Error())
	}
	if m == nil {
		return nil, fail(CodeModelNotFound, "no Model named "+strconv.Quote(name))
	}
	c.rec.Model, c.rec.ModelID = m.Metadata.Name, m.Status.ID
	if !allowed(c.key, m.Metadata.Name) {
		return nil, fail(CodeModelNotAllowed, "none of the Key's selectors "+fmt.Sprint(c.key.Spec.Models)+" matches "+strconv.Quote(m.Metadata.Name))
	}
	return m, nil
}

// selectTargets is stage 6: the Router's order, kept to the targets this
// door can reach on this route. None admitted is provider_unavailable;
// none reachable is dialect_unsupported, so the caller learns which of
// the two problems it has.
func (c *call) selectTargets(ctx context.Context) *failure {
	targets, err := c.h.o.Router.Targets(ctx, c.model)
	if err != nil {
		return fail(CodeStoreUnavailable, "target selection: "+err.Error())
	}
	if len(targets) == 0 {
		return fail(CodeProviderUnavailable, "no admitted target for Model "+strconv.Quote(c.model.Metadata.Name))
	}
	var dialects []string
	for _, t := range targets {
		d := t.Provider.Spec.Dialect
		if c.route.bridgeable(d) {
			c.targets = append(c.targets, t)
		} else if !slices.Contains(dialects, string(d)) {
			dialects = append(dialects, string(d))
		}
	}
	if len(c.targets) == 0 {
		return fail(CodeDialectUnsupported, "the /"+string(c.door)+" door cannot reach "+c.route.template+" on a target of dialect "+strings.Join(dialects, ", "))
	}
	if c.modeFor(c.targets[0]) != modePassthrough {
		c.decode()
	}
	return nil
}

// modeFor decides how the request reaches one target: equal dialects is
// passthrough, a translated route across dialects is translation, and a
// count no upstream answers is the estimate.
func (c *call) modeFor(t Target) mode {
	td := t.Provider.Spec.Dialect
	switch c.route.op {
	case opAnthropicCount:
		if td == v1.DialectAnthropic {
			return modePassthrough
		}
		return modeEstimate
	case opLuxCount:
		if td == v1.DialectAnthropic {
			return modeTranslate
		}
		return modeEstimate
	case opChatCompletions, opResponses, opEmbeddings, opMessages, opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed, opGenerate, opModelsList, opModelsRead, opOpaque, opNone:
	}
	if td == c.door {
		return modePassthrough
	}
	return modeTranslate
}

// decode reads the body with the door's codec once, for the reservation's
// estimate and for a count; a translation decodes again per target so
// its loss report is that target's alone.
func (c *call) decode() {
	if c.irReq != nil || c.decodeErr != nil {
		return
	}
	fe := frontendFor(c.route.op)
	if fe == nil {
		return
	}
	c.irReq, c.decodeErr = fe.DecodeRequest(c.body)
}

// reserve is stage 7. The reservation is spec 007's: the input estimate
// and the requested output, or 1024; a count and an opaque route reserve
// zero tokens.
func (c *call) reserve(ctx context.Context, res Reservation) *failure {
	if c.h.o.Limiter == nil {
		return nil
	}
	if !res.Opaque && !c.route.count() {
		res.OutputTokens = c.probe.maxTokens
		if res.OutputTokens == 0 {
			res.OutputTokens = defaultOutputTokens
		}
		if c.irReq != nil {
			res.InputTokens = tokencount.Estimate(c.irReq)
		} else {
			res.InputTokens = int64(len(c.body)) / 4
		}
	}
	lease, err := c.h.o.Limiter.Reserve(ctx, res)
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return &failure{code: refusal.Code, detail: refusal.Detail, retryAfter: refusal.RetryAfter}
		}
		return fail(CodeStoreUnavailable, "limits: "+err.Error())
	}
	c.lease = lease
	return nil
}

// listModels answers GET /v1/models: the Models whose names match one of
// the Key's selectors and whose status.available is true, sorted, in the
// door's list shape; never forwarded.
func (c *call) listModels(ctx context.Context) *failure {
	models, err := c.h.o.Catalog.Models(ctx)
	if err != nil {
		return fail(CodeStoreUnavailable, "Model list: "+err.Error())
	}
	var names []string
	for _, m := range models {
		if m.Status.Available != nil && *m.Status.Available && allowed(c.key, m.Metadata.Name) {
			names = append(names, m.Metadata.Name)
		}
	}
	slices.Sort(names)
	c.writeJSON(http.StatusOK, modelList(c.door, names))
	return nil
}

// readModel answers GET /v1/models/{model}: the one entry, or
// model_not_found, or model_not_allowed, so the list and the read agree.
func (c *call) readModel(ctx context.Context) *failure {
	m, f := c.lookupModel(ctx, c.route.model)
	if f != nil {
		return f
	}
	c.writeJSON(http.StatusOK, modelEntry(c.door, m.Metadata.Name))
	return nil
}

// estimate answers a token count no upstream answers:
// {"input_tokens": n} from tokencount.Estimate over the decoded request,
// with Lux-Estimated: true so a caller can tell a heuristic from a
// tokenizer's answer. The record says ok with zero tokens.
func (c *call) estimate() *failure {
	c.decode()
	if c.decodeErr != nil {
		return fail(CodeInvalidRequest, c.decodeErr.Error())
	}
	n := tokencount.Estimate(c.irReq)
	c.w.Header().Set(HeaderEstimated, "true")
	c.writeJSON(http.StatusOK, []byte(`{"input_tokens":`+strconv.FormatInt(n, 10)+"}\n"))
	return nil
}

// writeJSON writes one whole JSON body the gateway composed.
func (c *call) writeJSON(status int, body []byte) {
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	c.w.WriteHeader(status)
	_, _ = c.w.Write(body)
}

// estimatedTokens is the record's block when the upstream reported no
// usage: the estimator over the decoded request, and the body's length
// in bytes divided by four when the door has no codec for the body.
func (c *call) estimatedTokens() Tokens {
	c.decode()
	if c.irReq != nil {
		return Tokens{Input: tokencount.Estimate(c.irReq), Estimated: true}
	}
	return Tokens{Input: int64(len(c.body)) / 4, Estimated: true}
}

// responseWriter wraps the caller's connection: it notes the commit
// point, the first byte written, which is the response header, and
// flushes through the ResponseController.
type responseWriter struct {
	http.ResponseWriter
	rc        *http.ResponseController
	now       func() time.Time
	committed bool
	status    int
	first     time.Time
}

// WriteHeader commits the response once.
func (w *responseWriter) WriteHeader(code int) {
	if w.committed {
		return
	}
	w.committed, w.status, w.first = true, code, w.now()
	w.ResponseWriter.WriteHeader(code)
}

// Write commits a 200 when nothing did.
func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// Flush pushes what was written to the caller; a connection that cannot
// flush is written at its own pace.
func (w *responseWriter) Flush() { _ = w.rc.Flush() }
