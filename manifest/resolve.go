// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Actor is who is applying: the rendered subject of spec 006. Resolve
// carries it for the Lookup the caller constructs and reads it for
// nothing else.
type Actor struct {
	Subject string
}

// Lookup answers the references a manifest names, scoped to the actor.
// It returns ErrNotFound, or an *Error with code not_found, for an object
// that does not exist and for one the authorizer refuses, provider.read
// for a target, budget.draw for a budget, model.use for a selector, so
// existence does not leak, and ErrAuthorizerUnavailable, or any other
// error, when it cannot decide. A Provider comes back without its
// credential value. The API constructs one per request; an importer
// constructs its own.
type Lookup interface {
	Provider(ctx context.Context, nameOrID string) (*v1.Provider, error)
	Budget(ctx context.Context, nameOrID string) (*v1.Budget, error)
	// Models is the model.use decision for one selector: the Models it
	// matches now that the actor may use, possibly none, or a refusal of
	// the selector as a whole.
	Models(ctx context.Context, selector string) ([]v1.ModelRef, error)
}

// Defaults are the values an absent field takes, from the operator's
// configuration.
type Defaults struct {
	RequestsPerMinute, TokensPerMinute int
	Timeout                            time.Duration
}

// Limits are what the authorizer granted this actor; zero is no limit.
// The API decodes them from the decision's limits object, one wire name
// to one field: max_key_requests_per_minute to MaxRequestsPerMinute,
// max_key_tokens_per_minute to MaxTokensPerMinute, max_key_spend to
// MaxSpend, max_key_ttl to MaxTTL.
type Limits struct {
	MaxRequestsPerMinute, MaxTokensPerMinute int
	MaxSpend                                 v1.Money
	MaxTTL                                   time.Duration
}

// Options are what one Resolve runs under.
type Options struct {
	Actor                 Actor
	Lookup                Lookup
	Defaults              Defaults
	Limits                Limits
	Existing              v1.Object // the current object on update; nil on create
	FileMode              bool      // valueFrom allowed, a supplied Key value refused
	AllowPrivateUpstreams bool
	TunnelEnabled         bool             // a tunnel: true Provider is refused without it
	PublicURL             *url.URL         // the gateway's own address; a baseURL there is a loop
	Now                   func() time.Time // the clock; nil is time.Now
	NewName               func() string    // an absent metadata.name; nil makes one missing_field
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Resolved is Resolve's answer: the object with metadata and spec fully
// resolved, its status carrying only warnings and, for a Key, the models
// each selector matched and the effective expiry; and the warnings apart.
type Resolved struct {
	Object   v1.Object
	Warnings []string
}

// Resolve runs the six stages of spec 003 over a decoded object, each one
// whole before the next begins: structural validation, defaulting, the
// references through Lookup, the authorizer's ceilings, the update rules
// against Existing, and the consistency across fields. It is
// deterministic: the same input, options, Now, NewName, and Lookup
// answers produce byte-identical output. The input is not changed; every
// refusal is an *Error.
func Resolve(ctx context.Context, in v1.Object, o Options) (*Resolved, error) {
	now := o.now()
	var (
		obj      v1.Object
		warnings []string
		err      error
	)
	switch x := in.(type) {
	case *v1.Provider:
		obj, warnings, err = resolveProvider(x, o)
	case *v1.Model:
		obj, warnings, err = resolveModel(ctx, x, o)
	case *v1.Key:
		obj, warnings, err = resolveKey(ctx, x, o, now)
	case *v1.Budget:
		obj, warnings, err = resolveBudget(x, o)
	case nil:
		return nil, errors.New("manifest: Resolve of a nil object")
	default:
		return nil, fmt.Errorf("manifest: %T is not a kind this package resolves", in)
	}
	if err != nil {
		return nil, err
	}
	if warnings == nil {
		warnings = []string{}
	}
	setWarnings(obj, warnings)
	return &Resolved{Object: obj, Warnings: warnings}, nil
}

// Each kind's resolver runs stage 1 over the input as given, then copies
// it and runs the later stages over the copy, so the caller's object is
// never changed and the copy starts from an empty status.

func resolveProvider(in *v1.Provider, o Options) (*v1.Provider, []string, error) {
	if in == nil {
		in = &v1.Provider{}
	}
	if err := checkMeta(v1.KindProvider, in.Metadata); err != nil {
		return nil, nil, err
	}
	warnings, err := checkProvider(in, o)
	if err != nil {
		return nil, nil, err
	}
	p, err := cloneAs(in)
	if err != nil {
		return nil, nil, err
	}
	p.Status = v1.ProviderStatus{}
	if in.Spec.Credential != nil {
		if v, ok := in.Spec.Credential.Value(); ok {
			p.Spec.Credential.SetValue(v)
		}
	}
	if err := defaultName(&p.Metadata, o); err != nil {
		return nil, nil, err
	}
	defaultProvider(p, o)
	// Stages 3 and 4 have nothing to ask for a Provider. Stage 5: the
	// version bump on a new credential value is the API's, from whether
	// the resolved object carries one; an absent value keeps the stored.
	old, err := existing[*v1.Provider](o)
	if err != nil {
		return nil, nil, err
	}
	if old != nil {
		var paths []string
		paths = appendIf(paths, old.Metadata.Name != p.Metadata.Name, "metadata.name")
		paths = appendIf(paths, old.Spec.Dialect != p.Spec.Dialect, "spec.dialect")
		paths = appendIf(paths, old.Spec.Tunnel != p.Spec.Tunnel, "spec.tunnel")
		paths = appendIf(paths, envOf(credentialFrom(old)) != envOf(credentialFrom(p)), "spec.credential.valueFrom.env")
		if err := immutable(paths); err != nil {
			return nil, nil, err
		}
	}
	return p, warnings, nil
}

func resolveModel(ctx context.Context, in *v1.Model, o Options) (*v1.Model, []string, error) {
	if in == nil {
		in = &v1.Model{}
	}
	if err := checkMeta(v1.KindModel, in.Metadata); err != nil {
		return nil, nil, err
	}
	if err := checkModel(in); err != nil {
		return nil, nil, err
	}
	m, err := cloneAs(in)
	if err != nil {
		return nil, nil, err
	}
	m.Status = v1.ModelStatus{}
	if err := defaultName(&m.Metadata, o); err != nil {
		return nil, nil, err
	}
	defaultModel(m)
	for i, t := range m.Spec.Targets {
		path := indexPath("spec.targets", i) + ".provider"
		if o.Lookup == nil {
			return nil, nil, refuse(CodeAuthorizerUnavailable, "Options.Lookup is nil, so the reference cannot be resolved", path)
		}
		p, err := o.Lookup.Provider(ctx, t.Provider)
		if err != nil {
			return nil, nil, lookupError(err, path)
		}
		if p == nil {
			return nil, nil, refuse(CodeNotFound, "the Lookup returned no Provider for "+strconv.Quote(t.Provider), path)
		}
	}
	old, err := existing[*v1.Model](o)
	if err != nil {
		return nil, nil, err
	}
	if old != nil {
		if err := immutable(appendIf(nil, old.Metadata.Name != m.Metadata.Name, "metadata.name")); err != nil {
			return nil, nil, err
		}
	}
	if m.Spec.ContextWindow > 0 && m.Spec.MaxOutputTokens > m.Spec.ContextWindow {
		return nil, nil, refuse(CodeInvalidField, "maxOutputTokens "+strconv.Itoa(m.Spec.MaxOutputTokens)+" is above contextWindow "+strconv.Itoa(m.Spec.ContextWindow), "spec.maxOutputTokens")
	}
	var warnings []string
	if m.Spec.Pricing == nil {
		warnings = append(warnings, "This model has no pricing, so a key with a spend limit or a budget cannot use it unless it allows unpriced models.")
	}
	return m, warnings, nil
}

func resolveKey(ctx context.Context, in *v1.Key, o Options, now time.Time) (*v1.Key, []string, error) {
	if in == nil {
		in = &v1.Key{}
	}
	if err := checkMeta(v1.KindKey, in.Metadata); err != nil {
		return nil, nil, err
	}
	if err := checkKey(in, o, now); err != nil {
		return nil, nil, err
	}
	k, err := cloneAs(in)
	if err != nil {
		return nil, nil, err
	}
	k.Status = v1.KeyStatus{}
	if v, ok := in.Spec.Value(); ok {
		k.Spec.SetValue(v)
	}
	if v, ok := in.Spec.ValueSHA256(); ok {
		k.Spec.SetValueSHA256(v)
	}
	if err := defaultName(&k.Metadata, o); err != nil {
		return nil, nil, err
	}
	old, err := existing[*v1.Key](o)
	if err != nil {
		return nil, nil, err
	}
	var createdAt time.Time
	if old != nil {
		createdAt = old.Status.CreatedAt
	}
	defaultKey(k, o, now, createdAt)
	// Preserve the persisted deadline, including Keys written before creation
	// timestamps and TTL resolution shared one clock sample.
	if old != nil && k.Spec.TTL != "" {
		k.Status.ExpiresAt = old.Status.ExpiresAt
	}

	// Stage 3: the budget, then every selector.
	if o.Lookup == nil {
		return nil, nil, refuse(CodeAuthorizerUnavailable, "Options.Lookup is nil, so the references cannot be resolved", "spec.models")
	}
	if k.Spec.Budget != "" {
		b, err := o.Lookup.Budget(ctx, k.Spec.Budget)
		if err != nil {
			return nil, nil, lookupError(err, "spec.budget")
		}
		if b == nil {
			return nil, nil, refuse(CodeNotFound, "the Lookup returned no Budget for "+strconv.Quote(k.Spec.Budget), "spec.budget")
		}
	}
	var warnings []string
	k.Status.Selectors = make([]v1.SelectorStatus, 0, len(k.Spec.Models))
	for i, sel := range k.Spec.Models {
		refs, err := o.Lookup.Models(ctx, sel)
		if err != nil {
			return nil, nil, lookupError(err, indexPath("spec.models", i))
		}
		names := make([]string, 0, len(refs))
		for _, r := range refs {
			names = append(names, r.Name)
		}
		slices.Sort(names)
		names = slices.Compact(names)
		k.Status.Selectors = append(k.Status.Selectors, v1.SelectorStatus{Selector: sel, Matched: names})
		if len(names) == 0 {
			warnings = append(warnings, "Selector "+strconv.Quote(sel)+" matches no model right now; a model declared or discovered later will match it.")
		}
	}

	// Stage 4: the authorizer's ceilings. A value of 0, no limit, is above
	// any ceiling, and so is an absent spend limit or expiry.
	if err := checkCeilings(k, o.Limits, now, createdAt); err != nil {
		return nil, nil, err
	}

	// Stage 5: the update rules.
	if old != nil {
		var paths []string
		paths = appendIf(paths, old.Metadata.Name != k.Metadata.Name, "metadata.name")
		paths = appendIf(paths, !sameDuration(old.Spec.TTL, k.Spec.TTL), "spec.ttl")
		paths = appendIf(paths, envOf(old.Spec.ValueFrom) != envOf(k.Spec.ValueFrom), "spec.valueFrom.env")
		_, set := k.Spec.Value()
		paths = appendIf(paths, set, "spec.value")
		_, hashed := k.Spec.ValueSHA256()
		paths = appendIf(paths, hashed, "spec.valueSHA256")
		if err := immutable(paths); err != nil {
			return nil, nil, err
		}
	}

	// Stage 6: a spend amount needs its window.
	if sp := k.Spec.Limits.Spend; sp != nil && sp.Window == "" {
		return nil, nil, refuse(CodeMissingField, "a spend limit needs a window: a duration, month, or none", "spec.limits.spend.window")
	}
	return k, warnings, nil
}

// checkCeilings holds the Key's effective limits to the authorizer's.
func checkCeilings(k *v1.Key, l Limits, now, createdAt time.Time) error {
	s := &k.Spec
	if l.MaxRequestsPerMinute > 0 && aboveCeiling(*s.Limits.RequestsPerMinute, l.MaxRequestsPerMinute) {
		return refuse(CodeCeilingExceeded, rateDetail("requestsPerMinute", *s.Limits.RequestsPerMinute, l.MaxRequestsPerMinute), "spec.limits.requestsPerMinute")
	}
	if l.MaxTokensPerMinute > 0 && aboveCeiling(*s.Limits.TokensPerMinute, l.MaxTokensPerMinute) {
		return refuse(CodeCeilingExceeded, rateDetail("tokensPerMinute", *s.Limits.TokensPerMinute, l.MaxTokensPerMinute), "spec.limits.tokensPerMinute")
	}
	if l.MaxSpend > 0 {
		if s.Limits.Spend == nil {
			return refuse(CodeCeilingExceeded, "no spend limit is set, and the ceiling is "+l.MaxSpend.String(), "spec.limits.spend.amount")
		}
		if *s.Limits.Spend.Amount > l.MaxSpend {
			return refuse(CodeCeilingExceeded, "spend amount "+s.Limits.Spend.Amount.String()+" is above the ceiling "+l.MaxSpend.String(), "spec.limits.spend.amount")
		}
	}
	if l.MaxTTL > 0 {
		path := "spec.ttl"
		var effective time.Duration
		switch {
		case s.TTL != "":
			effective, _ = s.TTL.Parse()
		case !k.Status.ExpiresAt.IsZero():
			path = "spec.expiresAt"
			if createdAt.IsZero() {
				createdAt = now
			}
			effective = k.Status.ExpiresAt.Sub(createdAt)
		default:
			return refuse(CodeCeilingExceeded, "the key never expires, and the ceiling is "+string(v1.DurationOf(l.MaxTTL)), path)
		}
		if effective > l.MaxTTL {
			return refuse(CodeCeilingExceeded, "the key lives "+string(v1.DurationOf(effective))+", above the ceiling "+string(v1.DurationOf(l.MaxTTL)), path)
		}
	}
	return nil
}

func aboveCeiling(v, ceiling int) bool { return v == 0 || v > ceiling }

func rateDetail(field string, v, ceiling int) string {
	if v == 0 {
		return field + " 0 is no limit, above the ceiling " + strconv.Itoa(ceiling)
	}
	return field + " " + strconv.Itoa(v) + " is above the ceiling " + strconv.Itoa(ceiling)
}

func resolveBudget(in *v1.Budget, o Options) (*v1.Budget, []string, error) {
	if in == nil {
		in = &v1.Budget{}
	}
	if err := checkMeta(v1.KindBudget, in.Metadata); err != nil {
		return nil, nil, err
	}
	if err := checkBudget(in); err != nil {
		return nil, nil, err
	}
	b, err := cloneAs(in)
	if err != nil {
		return nil, nil, err
	}
	b.Status = v1.BudgetStatus{}
	if err := defaultName(&b.Metadata, o); err != nil {
		return nil, nil, err
	}
	defaultBudget(b)
	old, err := existing[*v1.Budget](o)
	if err != nil {
		return nil, nil, err
	}
	if old != nil {
		var paths []string
		paths = appendIf(paths, old.Metadata.Name != b.Metadata.Name, "metadata.name")
		paths = appendIf(paths, old.Spec.Currency != b.Spec.Currency, "spec.currency")
		paths = appendIf(paths, !sameWindow(old.Spec.Window, b.Spec.Window), "spec.window")
		if err := immutable(paths); err != nil {
			return nil, nil, err
		}
	}
	return b, nil, nil
}

// existing returns Options.Existing as the kind being resolved, nil when
// there is none, and a plain error when it is another kind, which is a
// caller's mistake and not a refusal of the manifest.
func existing[T v1.Object](o Options) (T, error) {
	var zero T
	if o.Existing == nil {
		return zero, nil
	}
	e, ok := o.Existing.(T)
	if !ok {
		return zero, fmt.Errorf("manifest: Options.Existing is a %s, not a %s", o.Existing.Kind(), zero.Kind())
	}
	return e, nil
}

// immutable is the one error of stage 5, naming every changed path.
func immutable(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return refuse(CodeImmutableField, "an update changed "+strconv.Itoa(len(paths))+" immutable field(s); delete and re-create the object to change them", paths...)
}

func appendIf(paths []string, cond bool, path string) []string {
	if cond {
		return append(paths, path)
	}
	return paths
}

func credentialFrom(p *v1.Provider) *v1.ValueFrom {
	if p.Spec.Credential == nil {
		return nil
	}
	return p.Spec.Credential.ValueFrom
}

func envOf(v *v1.ValueFrom) string {
	if v == nil {
		return ""
	}
	return v.Env
}

// sameDuration compares two durations by value, so "1h" and "60m" are one.
func sameDuration(a, b v1.Duration) bool {
	if a == "" || b == "" {
		return a == b
	}
	da, ea := a.Parse()
	db, eb := b.Parse()
	return ea == nil && eb == nil && da == db
}

// sameWindow compares two windows by value.
func sameWindow(a, b v1.Window) bool {
	da, oka := a.Duration()
	db, okb := b.Duration()
	if oka && okb {
		return da == db
	}
	return a == b
}

// lookupError turns a Lookup's answer into the refusal at the field: a
// not_found or authorizer_unavailable Error, or the two sentinels, become
// that refusal at the path. Any other error is the Lookup's own failure,
// its catalog or its store, and is returned wrapped and unchanged in
// kind, so a caller that maps refusals by *Error sees no refusal and
// answers with its own code for a store it cannot reach.
func lookupError(err error, path string) error {
	var e *Error
	if errors.As(err, &e) && (e.Code == CodeNotFound || e.Code == CodeAuthorizerUnavailable) {
		return refuse(e.Code, e.Detail, path)
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return refuse(CodeNotFound, err.Error(), path)
	case errors.Is(err, ErrAuthorizerUnavailable):
		return refuse(CodeAuthorizerUnavailable, err.Error(), path)
	}
	return fmt.Errorf("manifest: %s: the Lookup failed: %w", path, err)
}

// cloneAs copies one kind through its JSON form. The write-only values do
// not survive the trip and the callers carry them across by hand.
func cloneAs[T any](in *T) (*T, error) {
	out := new(T)
	data, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("manifest: encoding the object: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("manifest: decoding the object: %w", err)
	}
	return out, nil
}

// setWarnings writes the one status member every kind gets from Resolve.
func setWarnings(obj v1.Object, warnings []string) {
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.Warnings = warnings
	case *v1.Model:
		x.Status.Warnings = warnings
	case *v1.Key:
		x.Status.Warnings = warnings
	case *v1.Budget:
		x.Status.Warnings = warnings
	}
}
