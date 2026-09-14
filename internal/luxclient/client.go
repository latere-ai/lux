// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HeaderRequestID is the response header every answer of the API
// carries, spec 011's, read here and never sent.
const HeaderRequestID = "Lux-Request-Id"

// FirstByteTimeout is the deadline to the first response byte. There is
// no deadline after it, so a long list or a streamed body runs to its
// end.
const FirstByteTimeout = 10 * time.Second

// MaxBodyBytes caps a response body read whole, so a wrong URL that
// answers with something enormous does not exhaust the process.
const MaxBodyBytes int64 = 64 << 20

// Client speaks the /v1 API at BaseURL with the token Token yields per
// request. A nil Token sends no Authorization header, which is what the
// two documents no bearer guards need. A nil HTTP is NewHTTPClient().
type Client struct {
	BaseURL   string
	Token     TokenSource
	HTTP      *http.Client
	UserAgent string
}

// NewHTTPClient is the transport policy of spec 014: the process's
// HTTP_PROXY, HTTPS_PROXY, and NO_PROXY honoured, FirstByteTimeout to the
// response headers, no deadline on the body, and no retry anywhere.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: FirstByteTimeout,
		TLSHandshakeTimeout:   FirstByteTimeout,
	}}
}

// Response is one answer: the status, the headers, the body as it
// arrived, and the request id the server minted.
type Response struct {
	Status    int
	Header    http.Header
	Body      []byte
	RequestID string
}

// Request is one call before it is sent.
type Request struct {
	Method      string
	Path        string // absolute, /v1/keys/run-42
	Query       url.Values
	Header      http.Header
	Body        []byte
	ContentType string
}

// Do sends one request and reads the whole answer. A 2xx is a Response;
// any other status is an *Error when the body is the envelope and an
// *UnreadableError when it is not; a failure before a response is a
// *TransportError. Nothing is retried.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	target := strings.TrimRight(c.BaseURL, "/") + req.Path
	if len(req.Query) > 0 {
		target += "?" + req.Query.Encode()
	}
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}
	r, err := http.NewRequestWithContext(ctx, req.Method, target, body)
	if err != nil {
		return nil, &TransportError{Method: req.Method, URL: target, Err: err}
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	if req.Body != nil {
		r.Header.Set("Content-Type", req.ContentType)
	}
	r.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		r.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = NewHTTPClient()
	}
	resp, err := hc.Do(r)
	if err != nil {
		return nil, &TransportError{Method: req.Method, URL: target, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	if err != nil {
		return nil, &TransportError{Method: req.Method, URL: target, Err: err}
	}
	out := &Response{Status: resp.StatusCode, Header: resp.Header, Body: data, RequestID: resp.Header.Get(HeaderRequestID)}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return out, nil
	}
	if e, ok := decodeError(resp.StatusCode, data, out.RequestID, resp.Header.Get("Retry-After")); ok {
		return nil, &e
	}
	return nil, &UnreadableError{Method: req.Method, URL: target, Status: resp.StatusCode, RequestID: out.RequestID, Body: data}
}

// ObjectPath is /v1/{plural}/{name}. A Model's name may carry slashes,
// which stay slashes, since spec 011's model routes match any number of
// segments; every other character a segment cannot carry is escaped.
func ObjectPath(plural, name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/v1/" + plural + "/" + strings.Join(segs, "/")
}

// Apply is PUT /v1/{plural}/{name} with the manifest as the body.
// ifMatch, when set, is sent as If-Match: * or If-Match: "<version>".
func (c *Client) Apply(ctx context.Context, plural, name string, body []byte, contentType, ifMatch string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPut, Path: ObjectPath(plural, name), Body: body, ContentType: contentType, Header: ifMatchHeader(ifMatch)})
}

// Get is GET /v1/{plural}/{name}.
func (c *Client) Get(ctx context.Context, plural, name string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: ObjectPath(plural, name)})
}

// Delete is DELETE /v1/{plural}/{name}, with If-Match as Apply sends it.
func (c *Client) Delete(ctx context.Context, plural, name, ifMatch string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodDelete, Path: ObjectPath(plural, name), Header: ifMatchHeader(ifMatch)})
}

// Rotate is POST /v1/keys/{name}/rotate.
func (c *Client) Rotate(ctx context.Context, name string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodPost, Path: ObjectPath("keys", name) + "/rotate"})
}

// Self is GET /v1/self.
func (c *Client) Self(ctx context.Context) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/self"})
}

// WellKnown is GET /.well-known/lux, sent without a bearer whatever
// Token holds, because the document needs none.
func (c *Client) WellKnown(ctx context.Context) (*Response, error) {
	bare := *c
	bare.Token = nil
	return bare.Do(ctx, Request{Method: http.MethodGet, Path: "/.well-known/lux"})
}

// Usage is GET /v1/usage with the query as given; the route pages
// nothing.
func (c *Client) Usage(ctx context.Context, query url.Values) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: "/v1/usage", Query: query})
}

// Requests is GET /v1/requests followed through next_cursor, as List.
func (c *Client) Requests(ctx context.Context, query url.Values, limit int) (*Response, error) {
	return c.List(ctx, "/v1/requests", query, limit)
}

// Models is the one door route this client speaks, GET /lux/v1/models,
// with Token yielding the Key rather than an issuer token.
func (c *Client) Models(ctx context.Context) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: "/lux/v1/models"})
}

// ifMatchHeader renders --if-match: * as it is, a version quoted, and a
// value already quoted as given.
func ifMatchHeader(v string) http.Header {
	if v == "" {
		return nil
	}
	if v != "*" && !strings.HasPrefix(v, `"`) {
		v = `"` + v + `"`
	}
	return http.Header{"If-Match": []string{v}}
}
