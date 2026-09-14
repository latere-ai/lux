// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/bridge"

	v1 "latere.ai/x/lux/manifest/v1"
)

// hopByHop are the headers removed in both directions, with every header
// a Connection header names.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true,
}

// callerCredentials are the caller's own credential headers, removed so
// the provider sees only its own.
var callerCredentials = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization", "Cookie"}

// removeHopByHop deletes the hop-by-hop set and the headers Connection
// names.
// connectionNamed is the set of header names a Connection header lists,
// canonicalised, which are hop-by-hop for that one hop whatever their
// name. The same reading serves both directions.
func connectionNamed(h http.Header) map[string]bool {
	named := map[string]bool{}
	for _, c := range h.Values("Connection") {
		for name := range strings.SplitSeq(c, ",") {
			if name = strings.TrimSpace(name); name != "" {
				named[textproto.CanonicalMIMEHeaderKey(name)] = true
			}
		}
	}
	return named
}

func removeHopByHop(h http.Header) {
	for name := range connectionNamed(h) {
		h.Del(name)
	}
	for name := range hopByHop {
		h.Del(name)
	}
}

// outboundHeaders is the caller's header set as it leaves the gateway:
// the hop-by-hop set, every Lux-* header, the caller's credentials, the
// forwarding headers, and Accept-Encoding on a route the gateway reads
// removed; the dialect's own headers removed on a translation. The
// dropped dialect headers are returned so the loss report can name them.
func (c *call) outboundHeaders(m mode) (http.Header, []string) {
	h := c.r.Header.Clone()
	removeHopByHop(h)
	for name := range h {
		if strings.HasPrefix(name, "Lux-") || strings.HasPrefix(name, "X-Forwarded-") || name == "Forwarded" {
			h.Del(name)
		}
	}
	for _, name := range callerCredentials {
		h.Del(name)
	}
	h.Del("Content-Length")
	h.Del("Host")
	if c.route.class != ClassOpaque {
		h.Del("Accept-Encoding")
	}
	var dropped []string
	if m == modeTranslate {
		for _, name := range dialectHeaders {
			if h.Get(name) != "" {
				dropped = append(dropped, name)
			}
			h.Del(name)
		}
	}
	return h, dropped
}

// outbound builds the request toward one target, without the credential,
// which is injected last by attempt. On passthrough the body the probe
// read is forwarded undecoded but for the two member edits; on
// translation the bridge decodes it with the door's codec, writes the
// upstream name, and encodes it with the target's. The loss report is
// returned for the Lux-Loss header and the record, nil when nothing was
// lost.
func (c *call) outbound(ctx context.Context, t Target, m mode) (*http.Request, []string, *failure) {
	p := t.Provider
	base, err := url.Parse(p.Spec.BaseURL)
	if err != nil {
		return nil, nil, fail(CodeProviderUnavailable, "Provider "+p.Metadata.Name+": baseURL: "+err.Error())
	}
	hdr, dropped := c.outboundHeaders(m)
	var body []byte
	var path string
	var loss []string
	switch m {
	case modePassthrough:
		body, path = c.body, c.passthroughTarget(t)
		if t.Model != c.model.Metadata.Name && c.door != v1.DialectGemini {
			body = bridge.SetModel(body, t.Model)
		}
		if c.route.op == opChatCompletions && c.probe.Stream && p.Spec.Dialect == v1.DialectOpenAI {
			body = bridge.SetIncludeUsage(body)
		}
	case modeTranslate:
		b, f := c.bridgeFor(ctx, t)
		if f != nil {
			return nil, nil, f
		}
		var entries []string
		for _, name := range dropped {
			entries = append(entries, lossHeader(name))
		}
		out, l, err := b.Request(c.body, bridge.RequestOptions{Model: t.Model, Loss: entries})
		if err != nil {
			return nil, nil, c.bridgeFailure(ctx, err)
		}
		body, loss = out, l
		if c.route.count() {
			body = bridge.RemoveMember(bridge.RemoveMember(body, "max_tokens"), "stream")
		}
		path = upstreamPath(p.Spec.Dialect, c.route.op, p.Spec.Dialect == v1.DialectOpenAI && OpenAIReasoningFamily(t.Model))
		hdr.Set("Content-Type", "application/json")
		if p.Spec.Dialect == v1.DialectAnthropic && !hasHeader(p.Spec.Headers, "anthropic-version") {
			hdr.Set("anthropic-version", AnthropicVersion)
		}
	case modeEstimate:
	}
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = strippedQuery(c.r.URL.Query())
	req, err := http.NewRequestWithContext(ctx, c.r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, fail(CodeProviderUnavailable, "building the upstream request: "+err.Error())
	}
	req.Header = hdr
	req.ContentLength = int64(len(body))
	c.decorate(req, p)
	return req, loss, nil
}

// passthroughTarget is the upstream path of a passthrough: the caller's
// path relative to the door with the version prefix removed, and on a
// gemini model route the upstream name substituted for the caller's.
func (c *call) passthroughTarget(t Target) string {
	switch c.route.op {
	case opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed:
		_, verb, _ := strings.Cut(c.route.template, ":")
		return "/models/" + t.Model + ":" + verb
	case opChatCompletions, opResponses, opEmbeddings, opMessages, opAnthropicCount, opGenerate, opLuxCount, opModelsList, opModelsRead, opOpaque, opNone:
	}
	return passthroughPath(c.door, c.route.rest)
}

// decorate adds what every outbound request carries: the Provider's
// static headers, the User-Agent, and the request id.
func (c *call) decorate(req *http.Request, p *v1.Provider) {
	for name, value := range p.Spec.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("User-Agent", c.h.ua)
	req.Header.Set(HeaderRequestID, c.id)
}

// inject writes the Provider's credential last, under its header and
// scheme, so it wins over the caller's headers and the Provider's static
// ones. A Provider with no credential, or an empty value, gets no header.
func (c *call) inject(ctx context.Context, req *http.Request, p *v1.Provider) error {
	cred := p.Spec.Credential
	if cred == nil {
		return nil
	}
	value, err := c.h.o.Credentials.Credential(ctx, p.Status.ID)
	if err != nil {
		return err
	}
	if len(value) == 0 {
		return nil
	}
	header, scheme := cred.Header, cred.Scheme
	if header == "" {
		header = p.Spec.Dialect.CredentialHeader()
	}
	if scheme == "" {
		scheme = p.Spec.Dialect.CredentialScheme()
	}
	v := string(value)
	if scheme == v1.SchemeBearer {
		v = "Bearer " + v
	}
	req.Header.Set(header, v)
	return nil
}

// hasHeader reports whether the Provider's static headers name one,
// compared case-insensitively.
func hasHeader(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// strippedQuery is the caller's query without the credential parameter.
func strippedQuery(q url.Values) string {
	q.Del("key")
	return q.Encode()
}

// providerTimeout is the Provider's timeout, the default when a resolved
// Provider carries none.
func providerTimeout(p *v1.Provider) time.Duration {
	if d, err := p.Spec.Timeout.Parse(); err == nil && d > 0 {
		return d
	}
	return DefaultUpstreamTimeout
}

// retryable is spec 008's table over an upstream status: 408, 429, and
// any 5xx declined this attempt, not the request.
func retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// maxUpstreamDetail bounds the upstream body excerpt in a developer
// detail.
const maxUpstreamDetail = 1024

// upstreamDetail reads the first KiB of an upstream error body for the
// developer detail, and never for the caller's body.
func upstreamDetail(resp *http.Response) string {
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamDetail))
	_ = resp.Body.Close()
	return "upstream status " + strconv.Itoa(resp.StatusCode) + ": " + string(excerpt)
}

// forward is stages 8 and 9: the attempt order, one attempt per target,
// a retryable failure before any response byte handing the request to
// the next target, the last attempt's failure as the answer.
func (c *call) forward(ctx context.Context) *failure {
	var last *failure
	for _, t := range c.targets {
		if ctx.Err() != nil {
			return fail(ClientClosed, "")
		}
		m := c.modeFor(t)
		if m == modeEstimate {
			return c.estimate(ctx)
		}
		if !c.h.o.Router.Allow(t) {
			continue
		}
		f, final := c.attempt(ctx, t, m)
		if f == nil || final || c.model.Spec.Fallback == v1.FallbackNever {
			return f
		}
		last = f
	}
	if last == nil {
		return fail(CodeProviderUnavailable, "every admitted target's circuit refused the attempt")
	}
	return last
}

// attempt sends the request to one target and, on a complete answer,
// writes the response. final reports that no other target should be
// tried: the request was served, the failure is not retryable, or the
// caller has already seen part of an answer.
func (c *call) attempt(parent context.Context, t Target, m mode) (f *failure, final bool) {
	p := t.Provider
	started := c.h.now()
	at := Attempt{Provider: p.Metadata.Name, ProviderID: p.Status.ID, UpstreamModel: t.Model, Status: StatusOK}
	// One lux.upstream span per target tried, the parent of the client
	// span the instrumented transport opens; it ends with the attempt,
	// which on a stream is the stream's end.
	sctx, span := startUpstream(parent, p.Metadata.Name, len(c.rec.Attempts)+1, c.r.Method, c.urlTemplate(t, m))
	var ttfb time.Duration
	defer func() {
		at.Duration = c.h.now().Sub(started)
		if f != nil {
			at.Status, at.Error = StatusFailed, f.code
		}
		c.addAttempt(at)
		c.rec.UpstreamStatus = at.HTTPStatus
		endUpstream(span, at, ttfb)
	}()
	c.rec.Provider, c.rec.ProviderID, c.rec.UpstreamModel = p.Metadata.Name, p.Status.ID, t.Model
	c.rec.TargetDialect, c.rec.Translated = p.Spec.Dialect, m == modeTranslate

	ctx, cancel := context.WithTimeout(sctx, providerTimeout(p))
	defer cancel()
	req, loss, f := c.outbound(ctx, t, m)
	if f != nil {
		return f, true // an encode refusal fails the same on every target of the dialect
	}
	client, err := c.h.o.Clients.Client(ctx, p)
	if err != nil {
		return fail(CodeProviderUnavailable, "Provider "+p.Metadata.Name+": client: "+err.Error()), false
	}
	if err := c.inject(ctx, req, p); err != nil {
		return fail(CodeProviderUnavailable, "Provider "+p.Metadata.Name+": credential: "+err.Error()), false
	}
	resp, err := client.Do(req)
	if err != nil {
		return c.transportFailure(ctx, t, err), false
	}
	ttfb = c.h.now().Sub(started)
	at.HTTPStatus = resp.StatusCode
	switch {
	case retryable(resp.StatusCode):
		c.h.o.Router.RecordFailure(t)
		c.observe(p, resp.StatusCode >= 500)
		return fail(CodeUpstreamError, upstreamDetail(resp)), false
	case resp.StatusCode >= 300 && resp.StatusCode < 400, resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// A redirect is not followed, and a 401 or 403 is the Provider's
		// credential refused, which no caller's request caused: both are
		// the provider's error, not the request's.
		c.h.o.Router.RecordSuccess(t)
		c.observe(p, false)
		return fail(CodeUpstreamError, upstreamDetail(resp)), true
	case resp.StatusCode >= 400:
		c.h.o.Router.RecordSuccess(t)
		c.observe(p, false)
		return fail(CodeUpstreamRejected, upstreamDetail(resp)), true
	}
	c.h.o.Router.RecordSuccess(t)
	c.observe(p, false)
	defer func() { _ = resp.Body.Close() }()
	c.rec.Loss = loss
	return c.respond(ctx, t, m, resp, loss), true
}

// transportFailure classifies an error from the client: the caller gone,
// the Provider's timeout, or a failure before a response line.
func (c *call) transportFailure(ctx context.Context, t Target, err error) *failure {
	var tooLarge *http.MaxBytesError
	switch {
	case c.r.Context().Err() != nil:
		return fail(ClientClosed, "")
	case errors.As(err, &tooLarge):
		return fail(CodeBodyTooLarge, "the body crossed the limit of "+strconv.FormatInt(c.h.maxBody, 10)+" bytes")
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		c.h.o.Router.RecordFailure(t)
		c.observe(t.Provider, true)
		return fail(CodeUpstreamTimeout, "Provider "+t.Provider.Metadata.Name+": no response within "+providerTimeout(t.Provider).String())
	}
	c.h.o.Router.RecordFailure(t)
	c.observe(t.Provider, true)
	return fail(CodeProviderUnavailable, "Provider "+t.Provider.Metadata.Name+": "+err.Error())
}

// observe reports one outcome to the passive health of spec 005.
func (c *call) observe(p *v1.Provider, failed bool) {
	if c.h.o.Health != nil {
		c.h.o.Health.Observe(p.Status.ID, failed)
	}
}

// relayHeaders copies the upstream's response headers to the caller but
// for the hop-by-hop set, Set-Cookie, Content-Length, Content-Encoding,
// and the gateway's own Lux-* names; on a translation the upstream's
// dialect headers go too, because they describe a response the caller
// did not receive. A text/html Content-Type is relayed as
// application/octet-stream, so no door serves markup a browser renders.
func relayHeaders(dst, src http.Header, translated bool) {
	named := connectionNamed(src)
	for name, values := range src {
		name = textproto.CanonicalMIMEHeaderKey(name)
		lower := strings.ToLower(name)
		switch {
		case hopByHop[name], named[name], name == "Set-Cookie", name == "Content-Length", name == "Content-Encoding":
			continue
		case strings.HasPrefix(name, "Lux-"):
			continue
		case translated && (strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "openai-") || strings.HasPrefix(lower, "x-ratelimit-") || lower == "x-request-id"):
			continue
		case name == "Content-Type" && len(values) > 0 && isHTML(values[0]):
			dst.Set(name, "application/octet-stream")
			continue
		}
		dst[name] = values
	}
}

func isHTML(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/html")
}

func isSSE(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
}

// respond is stage 9: headers, then the body or the stream, in the
// door's dialect.
func (c *call) respond(ctx context.Context, t Target, m mode, resp *http.Response, loss []string) *failure {
	relayHeaders(c.w.Header(), resp.Header, m == modeTranslate)
	if len(loss) > 0 {
		c.w.Header().Set(HeaderLoss, strings.Join(loss, ","))
	}
	if !c.rec.Stream {
		return c.respondWhole(ctx, t, m, resp)
	}
	if m == modeTranslate {
		return c.streamTranslated(ctx, t, resp)
	}
	return c.streamPassthrough(ctx, t, resp)
}

// readWhole reads a non-streaming upstream body up to the cap; one byte
// more is upstream_error naming the cap.
func (c *call) readWhole(ctx context.Context, resp *http.Response) ([]byte, *failure) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.h.maxBody+1))
	if err != nil {
		return nil, c.readFailure(ctx, err)
	}
	if int64(len(body)) > c.h.maxBody {
		return nil, fail(CodeUpstreamError, "the upstream body is above the cap of "+strconv.FormatInt(c.h.maxBody, 10)+" bytes")
	}
	return body, nil
}

// readFailure classifies an error reading an upstream body after the
// response line arrived.
func (c *call) readFailure(ctx context.Context, err error) *failure {
	switch {
	case c.r.Context().Err() != nil:
		return fail(ClientClosed, "")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail(CodeUpstreamTimeout, "the upstream body did not finish within the Provider's timeout")
	}
	return fail(CodeUpstreamError, "reading the upstream body: "+err.Error())
}

// respondWhole writes a non-streaming response: on passthrough the body
// with the Model's name written back, on translation the body the bridge
// decoded with the target's codec, gave the Model's name, and encoded
// with the door's, its usage the record's tokens.
func (c *call) respondWhole(ctx context.Context, t Target, m mode, resp *http.Response) *failure {
	body, f := c.readWhole(ctx, resp)
	if f != nil {
		return f
	}
	td := t.Provider.Spec.Dialect
	switch {
	case c.route.count():
		c.tokens = Tokens{}
	case m == modePassthrough:
		if t.Model != c.model.Metadata.Name && c.door != v1.DialectGemini {
			body = bridge.SetModel(body, c.model.Metadata.Name)
		}
		if u, ok := bridge.UsageOf(wireOf(td), body); ok {
			c.tokens = tokensOf(u)
		} else {
			c.tokens = c.estimatedTokens()
		}
	case m == modeTranslate:
		b, f := c.bridgeFor(ctx, t)
		if f != nil {
			return f
		}
		out, _, usage, err := b.Response(body, bridge.ResponseOptions{Model: c.model.Metadata.Name})
		if err != nil {
			return c.bridgeFailure(ctx, err)
		}
		body = out
		c.tokens = tokensOf(usage)
		c.w.Header().Set("Content-Type", "application/json")
	case m == modeEstimate:
	}
	c.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(body)
	return nil
}

// opaque is the opaque route: the Provider from Lux-Provider or the one
// candidate, the reservation of one request and no tokens, and the bytes
// streamed through in both directions.
func (c *call) opaque(ctx context.Context) *failure {
	p, f := c.chooseProvider(ctx)
	if f != nil {
		return f
	}
	if f := c.reserve(ctx, Reservation{Key: c.key, Opaque: true}); f != nil {
		return f
	}
	return c.forwardOpaque(ctx, p)
}

// chooseProvider picks an opaque route's Provider: by name from
// Lux-Provider, a Provider of the door's dialect that one of the Key's
// selectors reaches through some Model; without the header, the one
// such Provider when there is exactly one; otherwise provider_required.
func (c *call) chooseProvider(ctx context.Context) (*v1.Provider, *failure) {
	models, err := c.h.o.Catalog.Models(ctx)
	if err != nil {
		return nil, fail(CodeStoreUnavailable, "Model list: "+err.Error())
	}
	reachable := map[string]bool{}
	for _, m := range models {
		if !allowed(c.key, m.Metadata.Name) {
			continue
		}
		for _, t := range m.Spec.Targets {
			reachable[t.Provider] = true
		}
	}
	var candidates []*v1.Provider
	for ref := range reachable {
		p, err := c.h.o.Catalog.Provider(ctx, ref)
		if err != nil {
			return nil, fail(CodeStoreUnavailable, "Provider lookup: "+err.Error())
		}
		if p != nil && p.Spec.Dialect == c.door {
			candidates = append(candidates, p)
		}
	}
	if name := c.r.Header.Get(HeaderProvider); name != "" {
		for _, p := range candidates {
			if p.Metadata.Name == name || p.Status.ID == name {
				return p, nil
			}
		}
		return nil, fail(CodeProviderRequired, "Lux-Provider "+strconv.Quote(name)+" names no "+string(c.door)+" Provider the Key's selectors reach")
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return nil, fail(CodeProviderRequired, strconv.Itoa(len(candidates))+" "+string(c.door)+" Providers are reachable through the Key's selectors; name one with Lux-Provider")
}

// forwardOpaque streams the caller's body to the Provider and the
// Provider's answer back, whole and unread: the caller chose the
// Provider and speaks its API directly, so its answer, an error status
// included, is the caller's to read. The record carries the upstream
// status and zero estimated tokens.
func (c *call) forwardOpaque(parent context.Context, p *v1.Provider) *failure {
	c.rec.Provider, c.rec.ProviderID, c.rec.TargetDialect = p.Metadata.Name, p.Status.ID, p.Spec.Dialect
	c.tokens = Tokens{Estimated: true}
	// The one lux.upstream span of an opaque request, under the route's
	// template rather than the caller's path, which is the caller's own.
	sctx, span := startUpstream(parent, p.Metadata.Name, 1, c.r.Method, c.route.template)
	started := c.h.now()
	at := Attempt{Provider: p.Metadata.Name, ProviderID: p.Status.ID, Status: StatusOK}
	var ttfb time.Duration
	defer func() { endUpstream(span, at, ttfb) }()
	base, err := url.Parse(p.Spec.BaseURL)
	if err != nil {
		return fail(CodeProviderUnavailable, "Provider "+p.Metadata.Name+": baseURL: "+err.Error())
	}
	ctx, cancel := context.WithTimeout(sctx, providerTimeout(p))
	defer cancel()
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + passthroughPath(c.door, c.route.rest)
	u.RawPath = ""
	u.RawQuery = strippedQuery(c.r.URL.Query())
	req, err := http.NewRequestWithContext(ctx, c.r.Method, u.String(), c.r.Body)
	if err != nil {
		return fail(CodeProviderUnavailable, "building the upstream request: "+err.Error())
	}
	req.Header, _ = c.outboundHeaders(modePassthrough)
	req.ContentLength = c.r.ContentLength
	c.decorate(req, p)
	client, err := c.h.o.Clients.Client(ctx, p)
	if err != nil {
		return fail(CodeProviderUnavailable, "Provider "+p.Metadata.Name+": client: "+err.Error())
	}
	if err := c.inject(ctx, req, p); err != nil {
		return fail(CodeProviderUnavailable, "Provider "+p.Metadata.Name+": credential: "+err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		f := c.transportFailure(ctx, Target{Provider: p}, err)
		at.Status, at.Error, at.Duration = StatusFailed, f.code, c.h.now().Sub(started)
		c.addAttempt(at)
		return f
	}
	defer func() { _ = resp.Body.Close() }()
	ttfb = c.h.now().Sub(started)
	at.HTTPStatus, at.Duration = resp.StatusCode, ttfb
	c.addAttempt(at)
	c.rec.UpstreamStatus = resp.StatusCode
	c.observe(p, resp.StatusCode >= 500)
	relayHeaders(c.w.Header(), resp.Header, false)
	c.w.WriteHeader(resp.StatusCode)
	c.w.Flush()
	if err := relayChunks(c.w, resp.Body, nil); err != nil {
		return c.streamFailure(ctx, err)
	}
	return nil
}

// streamFailure classifies an error after the first response byte: the
// caller gone, or the upstream cut, which is never retried.
func (c *call) streamFailure(ctx context.Context, err error) *failure {
	var we *writeError
	switch {
	case c.r.Context().Err() != nil, errors.As(err, &we):
		return fail(ClientClosed, "")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail(CodeUpstreamTimeout, "the stream did not finish within the Provider's timeout")
	}
	return fail(CodeUpstreamError, "the upstream stream failed: "+err.Error())
}
