// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The control plane this front serves is the contract's: the four kinds
// under /v1, the same grammar, the same error envelope, and the same
// usage surface, written over manifest.Decode, manifest.Resolve, and
// metering.Fold. Nothing here is a copy of the gateway's own handlers;
// what makes two fronts agree is that both call one Resolve and both
// pass the conformance suite.

// maxManifestBytes bounds one manifest, as a platform chooses to.
const maxManifestBytes int64 = 64 << 10

// The list page's default and its cap.
const (
	defaultLimit = 50
	maxLimit     = 200
	maxRecords   = 1000
)

// apiError is one refusal on its way to the caller: the contract's code,
// the JSON paths it names, the developer's detail, and the Retry-After
// of a rate refusal.
type apiError struct {
	code       string
	paths      []string
	detail     string
	retryAfter time.Duration
}

func refuse(code, detail string, paths ...string) *apiError {
	return &apiError{code: code, detail: detail, paths: paths}
}

// The codes this front writes, which are the contract's; the doors'
// own come from the gateway package.
const (
	codeUnauthenticated       = "unauthenticated"
	codeForbidden             = "forbidden"
	codeNotFound              = "not_found"
	codeInvalidField          = "invalid_field"
	codeAlreadyExists         = "already_exists"
	codeConflict              = "conflict"
	codeBudgetInUse           = "budget_in_use"
	codeProviderInUse         = "provider_in_use"
	codeBodyTooLarge          = "body_too_large"
	codeMalformedBody         = "malformed_body"
	codeRateLimited           = "rate_limited"
	codeInternal              = "internal"
	codeAuthorizerUnavailable = "authorizer_unavailable"
	codeStoreUnavailable      = "store_unavailable"
)

// status is the HTTP status of a code, the contract's table; a code
// outside it is a bug here and is a 500.
func statusOf(code string) int {
	if s, ok := codeStatus[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// message is the one fixed user sentence of a code. It never carries
// the developer's detail, which travels apart.
func messageOf(code string) string {
	if m, ok := codeMessage[code]; ok {
		return m
	}
	return "Something went wrong on this server."
}

var codeStatus = map[string]int{
	"malformed_body": 400, "multi_document": 400, "unsupported_version": 400, "unsupported_kind": 400,
	"unknown_field": 400, "missing_field": 400, "invalid_field": 400, "reserved_prefix": 400,
	"exclusive_fields": 400, "duplicate_target": 400, "invalid_request": 400, "currency_mismatch": 400,
	"unauthenticated": 401, "forbidden": 403, "not_found": 404, "read_only": 405,
	"already_exists": 409, "conflict": 409, "immutable_field": 409, "budget_in_use": 409, "provider_in_use": 409,
	"body_too_large": 413, "unsupported_media_type": 415, "ceiling_exceeded": 422, "rate_limited": 429,
	"internal": 500, "authorizer_unavailable": 503, "store_unavailable": 503,
}

var codeMessage = map[string]string{
	"malformed_body":         "The request body is not valid JSON or YAML.",
	"multi_document":         "Send one manifest per request.",
	"unsupported_version":    "This server serves lux.latere.ai/v1beta1.",
	"unsupported_kind":       "This server does not serve that kind.",
	"unknown_field":          "The manifest has a field this schema does not know.",
	"missing_field":          "A required field is missing.",
	"invalid_field":          "A field has a value it cannot take.",
	"reserved_prefix":        "That name is reserved for the gateway.",
	"exclusive_fields":       "Two fields that cannot be set together are set.",
	"duplicate_target":       "Two targets name the same provider and model.",
	"invalid_request":        "The request body is not valid for this request.",
	"currency_mismatch":      "The budget and the model are priced in different currencies.",
	"unauthenticated":        "This request needs a valid credential.",
	"forbidden":              "You do not have permission to do this.",
	"not_found":              "There is no such object.",
	"read_only":              "This server reads its manifests from a directory and cannot change them.",
	"already_exists":         "An object of this kind already has that name.",
	"conflict":               "The object changed since you read it; read it again and retry.",
	"immutable_field":        "This field cannot be changed after the object is created.",
	"budget_in_use":          "Keys still draw from this budget.",
	"provider_in_use":        "Models still target this provider.",
	"body_too_large":         "The request body is larger than this server accepts.",
	"unsupported_media_type": "Send the manifest as JSON or YAML.",
	"ceiling_exceeded":       "The value is above what you may ask for.",
	"rate_limited":           "Too many requests; wait and retry.",
	"internal":               "Something went wrong on this server.",
	"authorizer_unavailable": "The permission service is unavailable; retry shortly.",
	"store_unavailable":      "This server cannot reach its store; retry shortly.",
}

// mapErr turns any error a handler meets into the refusal it answers
// with: a manifest.Error keeps its code and paths, an authorizer that
// did not decide is authorizer_unavailable, and a store failure is
// store_unavailable.
func mapErr(err error) *apiError {
	if ae, ok := errors.AsType[*apiError](err); ok {
		return ae
	}
	if me, ok := errors.AsType[*manifest.Error](err); ok {
		return &apiError{code: string(me.Code), paths: me.Paths, detail: me.Detail}
	}
	if _, ok := errors.AsType[*authz.Unavailable](err); ok {
		return refuse(codeAuthorizerUnavailable, err.Error())
	}
	switch {
	case errors.Is(err, errNotFound):
		return refuse(codeNotFound, err.Error())
	case errors.Is(err, errVersionConflict):
		return refuse(codeConflict, err.Error())
	case errors.Is(err, errValueTaken):
		return refuse(codeInvalidField, "a Key with this value already exists", "spec.value")
	}
	return refuse(codeStoreUnavailable, err.Error())
}

// Error implements error so a refusal travels through a Resolve.
func (e *apiError) Error() string {
	if e.detail == "" {
		return e.code
	}
	return e.code + ": " + e.detail
}

// call is one control plane request.
type call struct {
	p      *Plane
	w      *responseWriter
	r      *http.Request
	id     string
	caller caller
}

// responseWriter notes the commit, so a refusal after the first byte is
// not written twice.
type responseWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.committed {
		return
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// handlerFunc is one route.
type handlerFunc func(c *call, ctx context.Context) *apiError

// control is the /v1 surface and the well-known document as one
// handler.
func (p *Plane) control() http.Handler {
	mux := http.NewServeMux()
	for _, kind := range kinds {
		plural := plurals[kind]
		mux.Handle("/v1/"+plural, p.route(map[string]handlerFunc{
			http.MethodGet: func(c *call, ctx context.Context) *apiError { return c.list(ctx, kind) },
		}))
		item := "/v1/" + plural + "/{name}"
		if kind == v1.KindModel {
			item = "/v1/" + plural + "/{name...}"
		}
		mux.Handle(item, p.route(map[string]handlerFunc{
			http.MethodPut:    func(c *call, ctx context.Context) *apiError { return c.apply(ctx, kind, c.r.PathValue("name")) },
			http.MethodGet:    func(c *call, ctx context.Context) *apiError { return c.read(ctx, kind, c.r.PathValue("name")) },
			http.MethodDelete: func(c *call, ctx context.Context) *apiError { return c.remove(ctx, kind, c.r.PathValue("name")) },
		}))
	}
	mux.Handle("/v1/keys/{name}/rotate", p.route(map[string]handlerFunc{http.MethodPost: (*call).rotate}))
	mux.Handle("/v1/usage", p.route(map[string]handlerFunc{http.MethodGet: (*call).usage}))
	mux.Handle("/v1/requests", p.route(map[string]handlerFunc{http.MethodGet: (*call).requests}))
	mux.Handle("/v1/self", p.route(map[string]handlerFunc{http.MethodGet: (*call).self}))
	mux.Handle("/v1/openapi.json", p.route(map[string]handlerFunc{http.MethodGet: (*call).openAPI}))
	mux.Handle("/.well-known/lux", p.route(map[string]handlerFunc{http.MethodGet: (*call).wellKnown}))
	mux.Handle("/", p.route(nil))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := p.begin(w, r)
		// A path the mux would redirect is no route of the table, so it
		// is answered rather than moved.
		if clean := path.Clean(r.URL.Path); clean != r.URL.Path {
			c.fail(refuse(codeNotFound, r.Method+" "+r.URL.Path+" is not in the route table"))
			return
		}
		mux.ServeHTTP(c.w, r.WithContext(withCall(r.Context(), c)))
	})
}

// plurals are the path segments of the four kinds.
var plurals = map[string]string{
	v1.KindProvider: "providers", v1.KindModel: "models", v1.KindKey: "keys", v1.KindBudget: "budgets",
}

// prefixes are the id prefix of each kind.
var prefixes = map[string]string{
	v1.KindProvider: v1.PrefixProvider, v1.KindModel: v1.PrefixModel,
	v1.KindKey: v1.PrefixKey, v1.KindBudget: v1.PrefixBudget,
}

// actions are the five a kind's routes ask: create, read, update,
// delete, list.
var actions = map[string][5]string{
	v1.KindProvider: {actionProviderCreate, actionProviderRead, actionProviderUpdate, actionProviderDelete, actionProviderList},
	v1.KindModel:    {actionModelCreate, actionModelRead, actionModelUpdate, actionModelDelete, actionModelList},
	v1.KindKey:      {actionKeyCreate, actionKeyRead, actionKeyUpdate, actionKeyDelete, actionKeyList},
	v1.KindBudget:   {actionBudgetCreate, actionBudgetRead, actionBudgetUpdate, actionBudgetDelete, actionBudgetList},
}

type callKey struct{}

func withCall(ctx context.Context, c *call) context.Context {
	return context.WithValue(ctx, callKey{}, c)
}

func callOf(r *http.Request) *call {
	c, _ := r.Context().Value(callKey{}).(*call)
	return c
}

// begin mints the request id and the two headers every answer carries.
func (p *Plane) begin(w http.ResponseWriter, r *http.Request) *call {
	c := &call{p: p, r: r, id: v1.NewID(v1.PrefixRequest, p.now(), nil)}
	c.w = &responseWriter{ResponseWriter: w}
	c.w.Header().Set(gateway.HeaderRequestID, c.id)
	if x := r.Header.Get("X-Request-Id"); printableASCII(x) {
		c.w.Header().Set("X-Request-Id", x)
	}
	return c
}

// printableASCII is the rule for echoing a caller's own id: at most 128
// bytes of printable ASCII, or nothing is echoed.
func printableASCII(s string) bool {
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

// route dispatches on the method; a method the table does not list is
// not_found in the envelope and never a bare 405.
func (p *Plane) route(methods map[string]handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := callOf(r)
		c.r = r
		fn, ok := methods[r.Method]
		if !ok {
			c.fail(refuse(codeNotFound, r.Method+" "+r.URL.Path+" is not in the route table"))
			return
		}
		if err := fn(c, r.Context()); err != nil {
			c.fail(err)
		}
	})
}

// fail writes the envelope of one refusal.
func (c *call) fail(e *apiError) {
	details := map[string]any{"request_id": c.id}
	if len(e.paths) > 0 {
		details["paths"] = e.paths
	}
	if e.detail != "" {
		details["detail"] = e.detail
	}
	h := c.w.Header()
	h.Set(gateway.HeaderError, e.code)
	if e.retryAfter > 0 {
		h.Set("Retry-After", strconv.FormatInt(max(int64((e.retryAfter+time.Second-1)/time.Second), 1), 10))
	}
	httpjson.WriteError(c.w, statusOf(e.code), httpjson.Error{Code: e.code, Message: messageOf(e.code), Details: details})
}

// writeJSON answers one body, with the ETag when a version is given.
func (c *call) writeJSON(status int, body any, version int64) *apiError {
	data, err := json.Marshal(body)
	if err != nil {
		return refuse(codeInternal, "")
	}
	data = append(data, '\n')
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	if version > 0 {
		h.Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	c.w.WriteHeader(status)
	_, _ = c.w.Write(data)
	return nil
}

// authenticate verifies the bearer and applies the platform's own
// control plane rate to the subject.
func (c *call) authenticate() *apiError {
	caller, err := c.p.identity.authenticate(c.r)
	if err != nil {
		return refuse(codeUnauthenticated, err.Error())
	}
	c.caller = caller
	if c.p.o.RequestsPerMinute <= 0 {
		return nil
	}
	c.p.subjects.SetRate(caller.subject, c.p.o.RequestsPerMinute)
	a := c.p.subjects.Allow(caller.subject)
	h := c.w.Header()
	h.Set("RateLimit-Limit", strconv.Itoa(c.p.o.RequestsPerMinute))
	h.Set("RateLimit-Remaining", strconv.Itoa(a.Remaining))
	h.Set("RateLimit-Reset", strconv.FormatInt(max(int64((a.Retry+time.Second-1)/time.Second), 0), 10))
	if !a.OK {
		return &apiError{code: codeRateLimited, retryAfter: a.Retry, detail: "the subject's requests a minute are spent"}
	}
	return nil
}

// authorize asks the platform's endpoint about one action. A deny is
// forbidden with the reason in the developer detail alone.
func (c *call) authorize(ctx context.Context, action string, res authz.Resource) (authz.Decision, *apiError) {
	d, err := c.p.decide(ctx, c.caller, action, res, authz.Caller{ID: c.id, IP: c.r.RemoteAddr, UserAgent: c.r.UserAgent()})
	if err != nil {
		return d, mapErr(err)
	}
	if !d.Allow {
		return d, refuse(codeForbidden, d.Reason)
	}
	return d, nil
}

// load reads one object by the path segment: an id when it carries the
// kind's prefix, a name otherwise, and not_found for an id of another
// kind.
func (c *call) load(kind, ref string) (v1.Object, int64, *apiError) {
	if ref == "" {
		return nil, 0, refuse(codeNotFound, "the path names no "+kind)
	}
	var (
		obj     v1.Object
		version int64
		err     error
	)
	switch {
	case strings.HasPrefix(ref, prefixes[kind]):
		obj, version, err = c.p.store.get(kind, ref)
	case hasKindPrefix(ref):
		return nil, 0, refuse(codeNotFound, strconv.Quote(ref)+" is an id of another kind")
	default:
		obj, version, err = c.p.store.byName(kind, ref)
	}
	if errors.Is(err, errNotFound) {
		return nil, 0, refuse(codeNotFound, "no "+kind+" "+strconv.Quote(ref))
	}
	if err != nil {
		return nil, 0, mapErr(err)
	}
	return obj, version, nil
}

// hasKindPrefix reports a segment that begins with any kind's id
// prefix, which no name may.
func hasKindPrefix(s string) bool {
	for _, p := range v1.KindPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// read is GET /v1/{kind}s/{id-or-name}.
func (c *call) read(ctx context.Context, kind, ref string) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	obj, version, err := c.load(kind, ref)
	if err != nil {
		return err
	}
	if _, err := c.authorize(ctx, actions[kind][1], resourceFor(kind, obj)); err != nil {
		return err
	}
	return c.writeJSON(http.StatusOK, c.p.render(hideSecrets(obj)), version)
}

// hideSecrets clears what no read ever carries: a Key's value.
func hideSecrets(obj v1.Object) v1.Object {
	if k, ok := obj.(*v1.Key); ok {
		k.Status.Value = ""
	}
	return obj
}

// listAnswer is {"items": [...], "next_cursor": "..."}.
type listAnswer struct {
	Items      []v1.Object `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// list is GET /v1/{kind}s with the label, owner, limit, cursor, and,
// for Models, source and provider parameters, narrowed by whatever the
// authorizer's filter names.
func (c *call) list(ctx context.Context, kind string) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	q, err := parseList(kind, c.r.URL.Query())
	if err != nil {
		return err
	}
	d, err := c.authorize(ctx, actions[kind][4], authz.NewResource(kind, "", map[string]any{"labels": map[string]string{}}))
	if err != nil {
		return err
	}
	after := ""
	if q.cursor != "" {
		name, ok := afterCursor(kind, q.cursor)
		if !ok {
			return refuse(codeInvalidField, "the cursor is not this kind's", "cursor")
		}
		after = name
	}
	items := make([]v1.Object, 0, q.limit)
	next := ""
	for _, obj := range c.p.store.list(kind) {
		if after != "" && obj.Name() <= after {
			continue
		}
		if !q.admits(obj) || !allowedBy(d.Filter, obj) {
			continue
		}
		if len(items) == q.limit {
			next = cursorOf(kind, items[len(items)-1].Name())
			break
		}
		items = append(items, c.p.render(hideSecrets(obj)))
	}
	return c.writeJSON(http.StatusOK, listAnswer{Items: items, NextCursor: next}, 0)
}

// listQuery is a list's parameters, parsed.
type listQuery struct {
	labels   map[string]string
	owner    string
	source   string
	provider string
	limit    int
	cursor   string
	nothing  bool // two values for one label match nothing
}

// admits reports whether one object passes the query's filters.
func (q listQuery) admits(obj v1.Object) bool {
	if q.nothing {
		return false
	}
	if q.owner != "" && obj.Owner() != q.owner {
		return false
	}
	labels := labelsOf(obj)
	for k, v := range q.labels {
		if labels[k] != v {
			return false
		}
	}
	m, isModel := obj.(*v1.Model)
	if q.source != "" && (!isModel || string(m.Status.Source) != q.source) {
		return false
	}
	if q.provider != "" {
		if !isModel {
			return false
		}
		found := false
		for _, t := range m.Spec.Targets {
			if t.Provider == q.provider {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// allowedBy is the authorizer's filter over one object.
func allowedBy(f *authz.Filter, obj v1.Object) bool {
	if f == nil {
		return true
	}
	if len(f.Owners) > 0 && !slices.Contains(f.Owners, obj.Owner()) {
		return false
	}
	labels := labelsOf(obj)
	for k, v := range f.Labels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// parseList reads the parameters the kind's route defines and refuses
// any other with invalid_field at its name.
func parseList(kind string, values map[string][]string) (listQuery, *apiError) {
	q := listQuery{labels: map[string]string{}, limit: defaultLimit}
	for name, vals := range values {
		switch name {
		case "label":
			for _, v := range vals {
				key, val, ok := strings.Cut(v, "=")
				if !ok || key == "" {
					return q, refuse(codeInvalidField, "label "+strconv.Quote(v)+" is not k=v", "label")
				}
				if prev, dup := q.labels[key]; dup && prev != val {
					q.nothing = true
					continue
				}
				q.labels[key] = val
			}
		case "owner":
			q.owner = last(vals)
		case "limit":
			n, err := strconv.Atoi(last(vals))
			if err != nil || n < 1 || n > maxLimit {
				return q, refuse(codeInvalidField, "limit "+strconv.Quote(last(vals))+" is not an integer from 1 to "+strconv.Itoa(maxLimit), "limit")
			}
			q.limit = n
		case "cursor":
			q.cursor = last(vals)
		case "source":
			if kind != v1.KindModel {
				return q, refuse(codeInvalidField, "source is a parameter of /v1/models alone", "source")
			}
			if s := v1.Source(last(vals)); !s.Valid() {
				return q, refuse(codeInvalidField, "source "+strconv.Quote(last(vals))+" is not declared or discovered", "source")
			}
			q.source = last(vals)
		case "provider":
			if kind != v1.KindModel {
				return q, refuse(codeInvalidField, "provider is a parameter of /v1/models alone", "provider")
			}
			q.provider = last(vals)
		default:
			return q, refuse(codeInvalidField, "no list route defines the parameter "+strconv.Quote(name), name)
		}
	}
	return q, nil
}

func last(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[len(vals)-1]
}

// apply is PUT /v1/{kind}s/{name}: decode with the route's hint,
// authorize the create or the update, hold the preconditions, resolve,
// fill the status this front owns, and write.
func (c *call) apply(ctx context.Context, kind, name string) *apiError {
	if name == "" {
		return refuse(codeNotFound, "the path names no "+kind)
	}
	if hasKindPrefix(name) {
		return refuse(codeInvalidField, "an apply is by name, and "+strconv.Quote(name)+" is an id", "metadata.name")
	}
	if err := c.authenticate(); err != nil {
		return err
	}
	pre, err := parsePrecondition(c.r.Header)
	if err != nil {
		return err
	}
	body, err := c.readBody()
	if err != nil {
		return err
	}
	in, derr := manifest.Decode(body, c.r.Header.Get("Content-Type"), manifest.Hint{APIVersion: v1.APIVersion, Kind: kind, Name: name})
	if derr != nil {
		return mapErr(derr)
	}
	existing, version, err := c.loadForApply(kind, name)
	if err != nil {
		return err
	}
	action, subject := actions[kind][0], in
	if existing != nil {
		action, subject = actions[kind][2], existing
	}
	d, err := c.authorize(ctx, action, resourceFor(kind, subject))
	if err != nil {
		return err
	}
	if err := pre.check(existing != nil, version); err != nil {
		return err
	}
	limits, lerr := decodeLimits(d)
	if lerr != nil {
		return refuse(codeAuthorizerUnavailable, "the authorizer's limits do not read: "+lerr.Error())
	}
	refs := c.lookup()
	resolved, rerr := manifest.Resolve(ctx, in, manifest.Options{
		Actor:                 manifest.Actor{Subject: c.caller.subject},
		Lookup:                refs,
		Defaults:              c.p.o.Defaults,
		Limits:                limits,
		Existing:              existing,
		AllowPrivateUpstreams: c.p.o.AllowPrivateUpstreams,
		PublicURL:             c.p.o.PublicURL,
		Now:                   c.p.now,
		NewName:               newName,
	})
	if rerr != nil {
		return mapErr(rerr)
	}
	obj := resolved.Object
	value, err := c.fillStatus(obj, existing, refs)
	if err != nil {
		return err
	}
	written, perr := c.p.store.put(obj, version)
	if perr != nil {
		if strings.Contains(perr.Error(), "already exists") {
			return refuse(codeAlreadyExists, perr.Error())
		}
		return mapErr(perr)
	}
	setVersion(obj, written)
	status := http.StatusOK
	if existing == nil {
		status = http.StatusCreated
	}
	c.p.render(obj)
	if value != "" {
		setKeyValue(obj, value)
	}
	return c.writeJSON(status, obj, written)
}

// loadForApply is the live object of the name, or nil when the name is
// free.
func (c *call) loadForApply(kind, name string) (v1.Object, int64, *apiError) {
	obj, version, err := c.p.store.byName(kind, name)
	if errors.Is(err, errNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, mapErr(err)
	}
	return obj, version, nil
}

// readBody reads the manifest within the bound, refusing a
// Content-Length above it before a byte is read.
func (c *call) readBody() ([]byte, *apiError) {
	if c.r.ContentLength > maxManifestBytes {
		return nil, refuse(codeBodyTooLarge, "Content-Length "+strconv.FormatInt(c.r.ContentLength, 10)+" is above "+strconv.FormatInt(maxManifestBytes, 10))
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, maxManifestBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, refuse(codeBodyTooLarge, "the body crossed "+strconv.FormatInt(maxManifestBytes, 10)+" bytes")
		}
		return nil, refuse(codeMalformedBody, "reading the body: "+err.Error())
	}
	return body, nil
}

// decodeLimits reads the ceilings the authorizer granted, in the wire
// names of the contract's limits object.
func decodeLimits(d authz.Decision) (manifest.Limits, error) {
	var wire struct {
		MaxKeyRequestsPerMinute int    `json:"max_key_requests_per_minute"`
		MaxKeyTokensPerMinute   int    `json:"max_key_tokens_per_minute"`
		MaxKeySpend             string `json:"max_key_spend"`
		MaxKeyTTL               string `json:"max_key_ttl"`
	}
	if err := d.DecodeLimits(&wire); err != nil {
		return manifest.Limits{}, err
	}
	out := manifest.Limits{MaxRequestsPerMinute: wire.MaxKeyRequestsPerMinute, MaxTokensPerMinute: wire.MaxKeyTokensPerMinute}
	if wire.MaxKeySpend != "" {
		money, err := v1.ParseMoney(wire.MaxKeySpend)
		if err != nil {
			return out, err
		}
		out.MaxSpend = money
	}
	if wire.MaxKeyTTL != "" {
		ttl, err := time.ParseDuration(wire.MaxKeyTTL)
		if err != nil {
			return out, err
		}
		out.MaxTTL = ttl
	}
	return out, nil
}

// fillStatus writes the members this front owns before the object is
// stored: the id, the owner, the creation time, a Provider's credential
// state, a Model's availability, and a Key's hash. It returns a minted
// Key value, which the create response carries once.
func (c *call) fillStatus(obj, existing v1.Object, refs *lookup) (string, *apiError) {
	id, owner, created := v1.NewID(prefixes[obj.Kind()], c.p.now(), nil), c.caller.subject, c.p.now()
	if existing != nil {
		id, owner, created = existing.ID(), existing.Owner(), createdAt(existing)
	}
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, owner, created, c.p.now()
		return "", c.fillCredential(x, existing)
	case *v1.Model:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, owner, created, c.p.now()
		// This front runs no health job: every declared Model is
		// published as available, and a platform that probes its
		// providers writes what it observed here instead.
		available := true
		x.Status.Available = &available
		x.Status.Targets = nil
		for _, t := range x.Spec.Targets {
			x.Status.Targets = append(x.Status.Targets, v1.TargetStatus{Provider: t.Provider, Model: t.Model, Health: v1.HealthHealthy})
		}
		return "", nil
	case *v1.Budget:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, owner, created, c.p.now()
		return "", nil
	case *v1.Key:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt, x.Status.UpdatedAt = id, owner, created, c.p.now()
		return c.fillKey(x, existing, refs)
	}
	return "", refuse(codeInternal, "")
}

// fillKey registers a Key's value on the create: the caller's own when
// the manifest supplied one, a minted one otherwise. The value leaves
// the object here and returns on the create response alone.
func (c *call) fillKey(k *v1.Key, existing v1.Object, refs *lookup) (string, *apiError) {
	if k.Spec.Budget != "" && refs.budget != nil {
		k.Status.Budget = &v1.BudgetRef{Name: refs.budget.Metadata.Name, ID: refs.budget.Status.ID}
	}
	if old, ok := existing.(*v1.Key); ok {
		k.Status.Prefix = old.Status.Prefix
		k.Spec.ClearValue()
		return "", nil
	}
	value, supplied := k.Spec.Value()
	if !supplied {
		minted, err := mintKeyValue()
		if err != nil {
			return "", refuse(codeInternal, "")
		}
		value = minted
	}
	k.Spec.ClearValue()
	k.Status.Prefix = keyPrefix(value, supplied)
	if err := c.p.store.putHash(k.Status.ID, hashValue(value)); err != nil {
		return "", mapErr(err)
	}
	if supplied {
		return "", nil
	}
	return value, nil
}

// fillCredential seals nothing and returns nothing: the value a
// manifest carried is held by the platform's store, and the status
// counts the values applied. A body without one keeps what is stored.
func (c *call) fillCredential(p *v1.Provider, existing v1.Object) *apiError {
	var stored *v1.CredentialStatus
	if old, ok := existing.(*v1.Provider); ok {
		stored = old.Status.Credential
	}
	value, ok := "", false
	if p.Spec.Credential != nil {
		value, ok = p.Spec.Credential.Value()
		p.Spec.Credential.ClearValue()
	}
	if !ok {
		if stored != nil {
			p.Status.Credential = stored
		} else {
			p.Status.Credential = &v1.CredentialStatus{}
		}
		return nil
	}
	version := 1
	if stored != nil {
		version = stored.Version + 1
	}
	p.Status.Credential = &v1.CredentialStatus{Set: true, Version: version, UpdatedAt: c.p.now()}
	c.p.store.putCredential(p.Status.ID, []byte(value))
	return nil
}

// createdAt is an object's creation time.
func createdAt(obj v1.Object) time.Time {
	switch x := obj.(type) {
	case *v1.Provider:
		return x.Status.CreatedAt
	case *v1.Model:
		return x.Status.CreatedAt
	case *v1.Key:
		return x.Status.CreatedAt
	case *v1.Budget:
		return x.Status.CreatedAt
	}
	return time.Time{}
}

// setVersion writes the version the store returned, which is the ETag.
func setVersion(obj v1.Object, version int64) {
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.Version = version
	case *v1.Model:
		x.Status.Version = version
	case *v1.Key:
		x.Status.Version = version
	case *v1.Budget:
		x.Status.Version = version
	}
}

// setKeyValue puts the minted value on the answer, the one time it is
// shown.
func setKeyValue(obj v1.Object, value string) {
	if k, ok := obj.(*v1.Key); ok {
		k.Status.Value = value
	}
}

// newName is the name an apply without one takes.
func newName() string { return "key-" + strings.ToLower(v1.NewID("", time.Now(), nil)) }

// remove is DELETE /v1/{kind}s/{id-or-name}.
func (c *call) remove(ctx context.Context, kind, ref string) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	pre, err := parsePrecondition(c.r.Header)
	if err != nil {
		return err
	}
	if pre.free {
		return refuse(codeInvalidField, "If-None-Match has no meaning on a delete", "If-None-Match")
	}
	obj, version, err := c.load(kind, ref)
	if err != nil {
		return err
	}
	if _, err := c.authorize(ctx, actions[kind][3], resourceFor(kind, obj)); err != nil {
		return err
	}
	if err := pre.check(true, version); err != nil {
		return err
	}
	if err := c.inUse(obj); err != nil {
		return err
	}
	c.p.store.remove(kind, obj.ID())
	if p, ok := obj.(*v1.Provider); ok {
		c.p.clients.Revoke(p.Status.ID)
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}

// inUse refuses a Budget a Key draws from and a Provider a Model
// targets.
func (c *call) inUse(obj v1.Object) *apiError {
	switch x := obj.(type) {
	case *v1.Budget:
		for _, o := range c.p.store.list(v1.KindKey) {
			if k, ok := o.(*v1.Key); ok && k.Status.Budget != nil && k.Status.Budget.ID == x.Status.ID {
				return refuse(codeBudgetInUse, "Key "+k.Status.ID+" draws from Budget "+x.Status.ID)
			}
		}
	case *v1.Provider:
		for _, o := range c.p.store.list(v1.KindModel) {
			m, ok := o.(*v1.Model)
			if !ok {
				continue
			}
			for _, t := range m.Spec.Targets {
				if t.Provider == x.Status.ID || t.Provider == x.Metadata.Name {
					return refuse(codeProviderInUse, "Model "+m.Status.ID+" targets Provider "+x.Status.ID)
				}
			}
		}
	}
	return nil
}

// rotate is POST /v1/keys/{name}/rotate: a new value, everything else
// kept.
func (c *call) rotate(ctx context.Context) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	pre, err := parsePrecondition(c.r.Header)
	if err != nil {
		return err
	}
	if pre.free {
		return refuse(codeInvalidField, "If-None-Match has no meaning on a rotate", "If-None-Match")
	}
	obj, version, err := c.load(v1.KindKey, c.r.PathValue("name"))
	if err != nil {
		return err
	}
	key, ok := obj.(*v1.Key)
	if !ok {
		return refuse(codeInternal, "")
	}
	if _, err := c.authorize(ctx, actionKeyUpdate, resourceFor(v1.KindKey, key)); err != nil {
		return err
	}
	if err := pre.check(true, version); err != nil {
		return err
	}
	value, merr := mintKeyValue()
	if merr != nil {
		return refuse(codeInternal, "")
	}
	key.Status.Prefix = keyPrefix(value, false)
	key.Status.UpdatedAt = c.p.now()
	if err := c.p.store.putHash(key.Status.ID, hashValue(value)); err != nil {
		return mapErr(err)
	}
	written, perr := c.p.store.put(key, version)
	if perr != nil {
		return mapErr(perr)
	}
	setVersion(key, written)
	c.p.render(key)
	key.Status.Value = value
	return c.writeJSON(http.StatusOK, key, written)
}

// selfAnswer is GET /v1/self: who the caller is and who decides.
type selfAnswer struct {
	Subject string         `json:"subject,omitempty"`
	Issuer  string         `json:"issuer,omitempty"`
	Sub     string         `json:"sub,omitempty"`
	Claims  map[string]any `json:"claims,omitempty"`
	Policy  string         `json:"policy"`
}

// self asks no action: asking permission to see the identity the caller
// just presented would be a category error.
func (c *call) self(context.Context) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	return c.writeJSON(http.StatusOK, selfAnswer{
		Subject: c.caller.subject, Issuer: c.caller.issuer, Sub: c.caller.sub,
		Claims: maps.Clone(c.caller.claims), Policy: "authorizer",
	}, 0)
}

// wellKnownAnswer is the one document a client reads before it has a
// token.
type wellKnownAnswer struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	APIVersion string            `json:"apiVersion"`
	API        string            `json:"api"`
	OpenAPI    string            `json:"openapi"`
	Doors      map[string]string `json:"doors"`
	Dialects   []string          `json:"dialects"`
	Issuers    []string          `json:"issuers"`
	Audience   string            `json:"audience"`
	Mode       string            `json:"mode"`
}

func (c *call) wellKnown(context.Context) *apiError {
	base := c.p.o.PublicURL.String()
	doc := wellKnownAnswer{
		Name: "lux", Version: c.p.o.Version, APIVersion: v1.APIVersion,
		API: base + "/v1", OpenAPI: base + "/v1/openapi.json",
		Doors: map[string]string{}, Dialects: dialects,
		Issuers: []string{c.p.identity.issuer}, Audience: c.p.identity.audience, Mode: "server",
	}
	if doc.Version == "" {
		doc.Version = "dev"
	}
	for _, d := range dialects {
		doc.Doors[d] = base + "/" + d
	}
	return c.writeJSON(http.StatusOK, doc, 0)
}

// openAPI serves the document every answer of this front is held to.
func (c *call) openAPI(context.Context) *apiError {
	c.w.Header().Set("Content-Type", "application/json")
	c.w.Header().Set("Content-Length", strconv.Itoa(len(c.p.openapi)))
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(c.p.openapi)
	return nil
}

// usageAnswer is GET /v1/usage.
type usageAnswer struct {
	Items []metering.Row `json:"items"`
}

// usage is GET /v1/usage: the query parsed, the names resolved to ids,
// the authorizer's filter intersected, and metering.Fold over the
// records this front kept.
func (c *call) usage(ctx context.Context) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	q, by, interval, err := c.parseUsage()
	if err != nil {
		return err
	}
	d, err := c.authorize(ctx, actionUsageRead, authz.NewResource(kindUsage, "", map[string]any{
		"keys": q.Keys, "owners": q.Owners, "labels": map[string]string{},
	}))
	if err != nil {
		return err
	}
	var owners []string
	var labels map[string]string
	if d.Filter != nil {
		owners, labels = d.Filter.Owners, d.Filter.Labels
	}
	q, ok := metering.Intersect(q, owners, labels)
	if !ok {
		return c.writeJSON(http.StatusOK, usageAnswer{Items: []metering.Row{}}, 0)
	}
	q = q.WithDefaults(c.p.now())
	if verr := q.Validate(); verr != nil {
		return queryRefusal(verr)
	}
	rows := metering.Fold(c.matching(q), by, interval)
	if rows == nil {
		rows = []metering.Row{}
	}
	return c.writeJSON(http.StatusOK, usageAnswer{Items: rows}, 0)
}

// queryRefusal turns metering's own parameter error into the envelope's
// invalid_field at the parameter it names.
func queryRefusal(err error) *apiError {
	if qe, ok := errors.AsType[*metering.QueryError](err); ok {
		return refuse(codeInvalidField, qe.Error(), qe.Field)
	}
	return refuse(codeInvalidField, err.Error())
}

// matching is every record the query's filters admit.
func (c *call) matching(q metering.Query) []metering.Record {
	return c.p.store.recordsOf(func(r metering.Record) bool {
		switch {
		case r.At.Before(q.From) || r.At.After(q.To):
			return false
		case len(q.Keys) > 0 && !slices.Contains(q.Keys, r.Key.ID):
			return false
		case len(q.Models) > 0 && !slices.Contains(q.Models, r.Model.ID):
			return false
		case len(q.Providers) > 0 && !slices.Contains(q.Providers, r.Provider.ID):
			return false
		case len(q.Owners) > 0 && !slices.Contains(q.Owners, r.Owner):
			return false
		}
		for k, v := range q.Labels {
			if r.Labels[k] != v {
				return false
			}
		}
		return true
	})
}

// parseUsage reads GET /v1/usage's parameters: the range, the
// groupings, the interval, and the filters, each name resolved to the
// id the records carry.
func (c *call) parseUsage() (metering.Query, []metering.Dimension, metering.Interval, *apiError) {
	var q metering.Query
	var by []metering.Dimension
	interval := metering.IntervalNone
	for name, vals := range c.r.URL.Query() {
		var err *apiError
		switch name {
		case "from":
			q.From, err = parseTime(name, last(vals))
		case "to":
			q.To, err = parseTime(name, last(vals))
		case "by":
			for d := range strings.SplitSeq(last(vals), ",") {
				by = append(by, metering.Dimension(strings.TrimSpace(d)))
			}
			q.By = by
		case "interval":
			interval = metering.Interval(last(vals))
			q.Interval = interval
		case "key":
			q.Keys = append(q.Keys, c.ids(v1.KindKey, v1.PrefixKey, vals)...)
		case "model":
			q.Models = append(q.Models, c.ids(v1.KindModel, v1.PrefixModel, vals)...)
		case "provider":
			q.Providers = append(q.Providers, c.ids(v1.KindProvider, v1.PrefixProvider, vals)...)
		case "owner":
			q.Owners = append(q.Owners, vals...)
		case "label":
			if q.Labels == nil {
				q.Labels = map[string]string{}
			}
			for _, v := range vals {
				k, val, ok := strings.Cut(v, "=")
				if !ok {
					return q, by, interval, refuse(codeInvalidField, "label "+strconv.Quote(v)+" is not k=v", "label")
				}
				q.Labels[k] = val
			}
		default:
			return q, by, interval, refuse(codeInvalidField, "no usage route defines the parameter "+strconv.Quote(name), name)
		}
		if err != nil {
			return q, by, interval, err
		}
	}
	return q, by, interval, nil
}

// ids resolves each value to the id the records carry: the value when
// it is already an id, the object's id when it names one, and a value
// that names nothing when it names nothing, so the answer is empty
// rather than a refusal.
func (c *call) ids(kind, prefix string, vals []string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if strings.HasPrefix(v, prefix) {
			out = append(out, v)
			continue
		}
		if obj, _, err := c.p.store.byName(kind, v); err == nil {
			out = append(out, obj.ID())
			continue
		}
		out = append(out, v)
	}
	return out
}

func parseTime(name, value string) (time.Time, *apiError) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, refuse(codeInvalidField, name+" "+strconv.Quote(value)+" is not an RFC 3339 time", name)
	}
	return t, nil
}

// recordsAnswer is GET /v1/requests.
type recordsAnswer struct {
	Items  []metering.Record `json:"items"`
	Source string            `json:"source"`
}

// requests is GET /v1/requests: the records themselves, newest first.
func (c *call) requests(ctx context.Context) *apiError {
	if err := c.authenticate(); err != nil {
		return err
	}
	q := metering.Query{}
	limit := defaultLimit
	var status, errCode string
	for name, vals := range c.r.URL.Query() {
		var err *apiError
		switch name {
		case "from":
			q.From, err = parseTime(name, last(vals))
		case "to":
			q.To, err = parseTime(name, last(vals))
		case "key":
			q.Keys = append(q.Keys, c.ids(v1.KindKey, v1.PrefixKey, vals)...)
		case "model":
			q.Models = append(q.Models, c.ids(v1.KindModel, v1.PrefixModel, vals)...)
		case "provider":
			q.Providers = append(q.Providers, c.ids(v1.KindProvider, v1.PrefixProvider, vals)...)
		case "owner":
			q.Owners = append(q.Owners, vals...)
		case "status":
			status = last(vals)
		case "error":
			errCode = last(vals)
		case "limit":
			n, aerr := strconv.Atoi(last(vals))
			if aerr != nil || n < 1 || n > maxRecords {
				return refuse(codeInvalidField, "limit "+strconv.Quote(last(vals))+" is not an integer from 1 to "+strconv.Itoa(maxRecords), "limit")
			}
			limit = n
		default:
			return refuse(codeInvalidField, "no records route defines the parameter "+strconv.Quote(name), name)
		}
		if err != nil {
			return err
		}
	}
	d, aerr := c.authorize(ctx, actionUsageRead, authz.NewResource(kindUsage, "", map[string]any{
		"keys": q.Keys, "owners": q.Owners, "labels": map[string]string{},
	}))
	if aerr != nil {
		return aerr
	}
	var owners []string
	var labels map[string]string
	if d.Filter != nil {
		owners, labels = d.Filter.Owners, d.Filter.Labels
	}
	q, ok := metering.Intersect(q, owners, labels)
	if !ok {
		return c.writeJSON(http.StatusOK, recordsAnswer{Items: []metering.Record{}, Source: "memory"}, 0)
	}
	q = q.WithDefaults(c.p.now())
	items := make([]metering.Record, 0, limit)
	for _, r := range c.matching(q) {
		if status != "" && string(r.Status) != status {
			continue
		}
		if errCode != "" && r.Error != errCode {
			continue
		}
		if len(items) == limit {
			break
		}
		items = append(items, r)
	}
	return c.writeJSON(http.StatusOK, recordsAnswer{Items: items, Source: "memory"}, 0)
}

// precondition is what If-Match and If-None-Match asked.
type precondition struct {
	exists  bool
	version int64
	free    bool
}

// parsePrecondition reads the two headers; the only forms accepted are
// * and one quoted integer on If-Match, and * on If-None-Match.
func parsePrecondition(h http.Header) (precondition, *apiError) {
	var p precondition
	match, noneMatch := h.Get("If-Match"), h.Get("If-None-Match")
	if match != "" && noneMatch != "" {
		return p, refuse(codeInvalidField, "one precondition is accepted", "If-Match", "If-None-Match")
	}
	switch {
	case match == "*":
		p.exists = true
	case match != "":
		n, ok := quotedVersion(match)
		if !ok {
			return p, refuse(codeInvalidField, "If-Match "+match+" is not * or one quoted integer", "If-Match")
		}
		p.version = n
	case noneMatch == "*":
		p.free = true
	case noneMatch != "":
		return p, refuse(codeInvalidField, "If-None-Match "+noneMatch+" is not *", "If-None-Match")
	}
	return p, nil
}

func quotedVersion(s string) (int64, bool) {
	if len(s) < 3 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return 0, false
	}
	n, err := strconv.ParseInt(s[1:len(s)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// check holds what was loaded to the precondition.
func (p precondition) check(exists bool, version int64) *apiError {
	switch {
	case p.free && exists:
		return refuse(codeAlreadyExists, "If-None-Match: * was sent and the name is taken")
	case (p.exists || p.version > 0) && !exists:
		return refuse(codeNotFound, "If-Match was sent and no object of that name exists")
	case p.version > 0 && version != p.version:
		return refuse(codeConflict, "If-Match \""+strconv.FormatInt(p.version, 10)+"\" was sent and the object is at version "+strconv.FormatInt(version, 10))
	}
	return nil
}
