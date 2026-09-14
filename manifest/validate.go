// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Stage 1, structural validation: the field rules that need no defaults
// and no lookups. Each check returns the first refusal in field order.

// checkMeta applies the metadata rules of every kind: the kind's name
// rule behind the reserved prefixes, label syntax, annotation sizes, and
// the reserved label prefix.
func checkMeta(kind string, meta v1.ObjectMeta) error {
	if meta.Name != "" {
		if hasKindPrefix(meta.Name) {
			return refuse(CodeReservedPrefix, "a name may not begin with an id prefix; "+strconv.Quote(meta.Name)+" reads as an id", "metadata.name")
		}
		if !validName(kind, meta.Name) {
			return refuse(CodeInvalidField, strconv.Quote(meta.Name)+": "+nameRule(kind), "metadata.name")
		}
	}
	for _, k := range slices.Sorted(maps.Keys(meta.Labels)) {
		path := keyPath("metadata.labels", k)
		if strings.HasPrefix(k, v1.LabelPrefix) {
			return refuse(CodeReservedPrefix, "labels under "+v1.LabelPrefix+" are the gateway's", path)
		}
		if err := checkLabelKey(k); err != nil {
			return refuse(CodeInvalidField, err.Error(), path)
		}
		if err := checkLabelValue(meta.Labels[k]); err != nil {
			return refuse(CodeInvalidField, err.Error(), path)
		}
	}
	total := 0
	for _, k := range slices.Sorted(maps.Keys(meta.Annotations)) {
		path := keyPath("metadata.annotations", k)
		if strings.HasPrefix(k, v1.LabelPrefix) {
			return refuse(CodeReservedPrefix, "annotations under "+v1.LabelPrefix+" are the gateway's", path)
		}
		if err := checkLabelKey(k); err != nil {
			return refuse(CodeInvalidField, err.Error(), path)
		}
		v := meta.Annotations[k]
		if len(v) > maxAnnotationValue {
			return refuse(CodeInvalidField, "an annotation value is at most 4096 bytes; this one is "+strconv.Itoa(len(v)), path)
		}
		total += len(k) + len(v)
	}
	if total > maxAnnotations {
		return refuse(CodeInvalidField, "annotations total "+strconv.Itoa(total)+" bytes, at most 65536", "metadata.annotations")
	}
	return nil
}

// checkProvider applies the Provider.spec table and returns the warning
// the host rule may add.
func checkProvider(p *v1.Provider, o Options) ([]string, error) {
	s := &p.Spec
	if s.Dialect == "" {
		return nil, refuse(CodeMissingField, "dialect is required: openai, anthropic, gemini, or lux", "spec.dialect")
	}
	if !s.Dialect.Valid() {
		return nil, refuse(CodeInvalidField, strconv.Quote(string(s.Dialect))+" is not openai, anthropic, gemini, or lux", "spec.dialect")
	}
	var warnings []string
	if s.Tunnel {
		if s.BaseURL != "" {
			return nil, refuse(CodeExclusiveFields, "a tunnelled Provider has no address the gateway dials", "spec.tunnel", "spec.baseURL")
		}
		if s.Credential != nil {
			return nil, refuse(CodeExclusiveFields, "the tunnel is the credential of a tunnelled Provider", "spec.tunnel", "spec.credential")
		}
		if !o.TunnelEnabled {
			return nil, refuse(CodeInvalidField, "tunnel: true needs LUX_TUNNEL_ENABLED on this gateway", "spec.tunnel")
		}
	} else {
		if s.BaseURL == "" {
			return nil, refuse(CodeMissingField, "baseURL is required unless tunnel is true", "spec.baseURL")
		}
		warning, err := checkBaseURL(s.BaseURL, o)
		if err != nil {
			return nil, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if c := s.Credential; c != nil {
		value, set := c.Value()
		if set && c.ValueFrom != nil {
			return nil, refuse(CodeExclusiveFields, "a credential is a value or a variable, not both", "spec.credential.value", "spec.credential.valueFrom.env")
		}
		if set && (len(value) < 1 || len(value) > maxCredential) {
			return nil, refuse(CodeInvalidField, "a credential value is 1 to 4096 bytes; this one is "+strconv.Itoa(len(value)), "spec.credential.value")
		}
		if c.ValueFrom != nil {
			if err := checkEnvName(c.ValueFrom.Env); err != nil {
				return nil, refuse(CodeInvalidField, err.Error(), "spec.credential.valueFrom.env")
			}
			if !o.FileMode {
				return nil, refuse(CodeInvalidField, "valueFrom.env is read by the file mode; in server mode the value is applied and sealed", "spec.credential.valueFrom.env")
			}
		}
		if c.Header != "" {
			if err := checkHeaderName(c.Header); err != nil {
				return nil, refuse(CodeInvalidField, err.Error(), "spec.credential.header")
			}
		}
		if c.Scheme != "" && !c.Scheme.Valid() {
			return nil, refuse(CodeInvalidField, strconv.Quote(string(c.Scheme))+" is not bearer or raw", "spec.credential.scheme")
		}
	}
	if len(s.Headers) > maxHeaders {
		return nil, refuse(CodeInvalidField, "at most 16 static headers; "+strconv.Itoa(len(s.Headers))+" given", "spec.headers")
	}
	credentialHeader := strings.ToLower(s.Dialect.CredentialHeader())
	if s.Credential != nil && s.Credential.Header != "" {
		credentialHeader = strings.ToLower(s.Credential.Header)
	}
	for _, h := range slices.Sorted(maps.Keys(s.Headers)) {
		path := keyPath("spec.headers", h)
		if err := checkHeaderName(h); err != nil {
			return nil, refuse(CodeInvalidField, err.Error(), path)
		}
		if lower := strings.ToLower(h); lower == credentialHeader || reservedHeaders[lower] {
			return nil, refuse(CodeReservedPrefix, h+" is the credential header, a framing header, or hop-by-hop, and is the gateway's to write", path)
		}
		if err := checkHeaderValue(s.Headers[h]); err != nil {
			return nil, refuse(CodeInvalidField, err.Error(), path)
		}
	}
	if s.Discovery.Mode != "" && !s.Discovery.Mode.Valid() {
		return nil, refuse(CodeInvalidField, strconv.Quote(string(s.Discovery.Mode))+" is not auto or none", "spec.discovery.mode")
	}
	for i, g := range s.Discovery.Include {
		if err := checkGlob(g); err != nil {
			return nil, refuse(CodeInvalidField, err.Error(), indexPath("spec.discovery.include", i))
		}
	}
	for i, g := range s.Discovery.Exclude {
		if err := checkGlob(g); err != nil {
			return nil, refuse(CodeInvalidField, err.Error(), indexPath("spec.discovery.exclude", i))
		}
	}
	if s.Health.Mode != "" && !s.Health.Mode.Valid() {
		return nil, refuse(CodeInvalidField, strconv.Quote(string(s.Health.Mode))+" is not probe, passive, or none", "spec.health.mode")
	}
	if s.Timeout != "" {
		d, err := s.Timeout.Parse()
		if err != nil {
			return nil, refuse(CodeInvalidField, strconv.Quote(string(s.Timeout))+" is not a Go duration", "spec.timeout")
		}
		if d < time.Second || d > time.Hour {
			return nil, refuse(CodeInvalidField, "timeout is between 1s and 1h; "+string(s.Timeout)+" is outside", "spec.timeout")
		}
	}
	if s.Concurrency < 0 {
		return nil, refuse(CodeInvalidField, "concurrency is 0 or more", "spec.concurrency")
	}
	return warnings, nil
}

// checkModel applies the Model.spec table.
func checkModel(m *v1.Model) error {
	s := &m.Spec
	if len(s.Targets) == 0 {
		return refuse(CodeMissingField, "targets is required, with at least one entry", "spec.targets")
	}
	type pair struct{ provider, model string }
	seen := map[pair]bool{}
	weighted := false
	for i, t := range s.Targets {
		path := indexPath("spec.targets", i)
		if t.Provider == "" {
			return refuse(CodeMissingField, "provider is required", path+".provider")
		}
		if err := checkReference(t.Provider, providerRef, "Provider"); err != nil {
			return refuse(CodeInvalidField, err.Error(), path+".provider")
		}
		if t.Model != "" {
			if err := checkUpstreamName(t.Model); err != nil {
				return refuse(CodeInvalidField, err.Error(), path+".model")
			}
		}
		if t.Weight != nil && (*t.Weight < 0 || *t.Weight > 1000) {
			return refuse(CodeInvalidField, "weight is 0 to 1000", path+".weight")
		}
		if t.Priority < 0 || t.Priority > 9 {
			return refuse(CodeInvalidField, "priority is 0 to 9", path+".priority")
		}
		key := pair{t.Provider, t.Model}
		if key.model == "" {
			key.model = m.Metadata.Name
		}
		if seen[key] {
			return refuse(CodeDuplicateTarget, "provider "+strconv.Quote(key.provider)+" and model "+strconv.Quote(key.model)+" appear twice", path)
		}
		seen[key] = true
		if t.Weight == nil || *t.Weight > 0 {
			weighted = true
		}
	}
	if !weighted {
		return refuse(CodeInvalidField, "every target has weight 0; at least one must be above 0", "spec.targets")
	}
	if s.Fallback != "" && !s.Fallback.Valid() {
		return refuse(CodeInvalidField, strconv.Quote(string(s.Fallback))+" is not onError or never", "spec.fallback")
	}
	if p := s.Pricing; p != nil {
		if p.Input == nil {
			return refuse(CodeMissingField, "pricing needs input and output", "spec.pricing.input")
		}
		if p.Output == nil {
			return refuse(CodeMissingField, "pricing needs input and output", "spec.pricing.output")
		}
		if p.Currency != "" {
			if err := checkCurrency(p.Currency); err != nil {
				return refuse(CodeInvalidField, err.Error(), "spec.pricing.currency")
			}
		}
		if p.Per != 0 && p.Per != 1 && p.Per != 1000 && p.Per != 1_000_000 {
			return refuse(CodeInvalidField, "per is 1, 1000, or 1000000", "spec.pricing.per")
		}
	}
	if err := checkModalities(s.Modalities.Input, "spec.modalities.input"); err != nil {
		return err
	}
	if err := checkModalities(s.Modalities.Output, "spec.modalities.output"); err != nil {
		return err
	}
	if s.ContextWindow < 0 {
		return refuse(CodeInvalidField, "contextWindow is positive", "spec.contextWindow")
	}
	if s.MaxOutputTokens < 0 {
		return refuse(CodeInvalidField, "maxOutputTokens is positive", "spec.maxOutputTokens")
	}
	return nil
}

// checkModalities holds a present list to non-empty, valid, and unique.
func checkModalities(list []v1.Modality, path string) error {
	if list == nil {
		return nil
	}
	if len(list) == 0 {
		return refuse(CodeInvalidField, "a modalities list is non-empty", path)
	}
	seen := map[v1.Modality]bool{}
	for i, m := range list {
		if !m.Valid() {
			return refuse(CodeInvalidField, strconv.Quote(string(m))+" is not text, image, audio, video, file, or embedding", indexPath(path, i))
		}
		if seen[m] {
			return refuse(CodeInvalidField, string(m)+" appears twice", indexPath(path, i))
		}
		seen[m] = true
	}
	return nil
}

// checkKey applies the Key.spec table.
func checkKey(k *v1.Key, o Options, now time.Time) error {
	s := &k.Spec
	if len(s.Models) == 0 {
		return refuse(CodeMissingField, "models is required, with at least one selector", "spec.models")
	}
	if len(s.Models) > maxSelectors {
		return refuse(CodeInvalidField, "at most 64 selectors; "+strconv.Itoa(len(s.Models))+" given", "spec.models")
	}
	seen := map[string]bool{}
	for i, sel := range s.Models {
		if err := checkSelector(sel); err != nil {
			return refuse(CodeInvalidField, err.Error(), indexPath("spec.models", i))
		}
		if seen[sel] {
			return refuse(CodeInvalidField, strconv.Quote(sel)+" appears twice", indexPath("spec.models", i))
		}
		seen[sel] = true
	}
	value, set := s.Value()
	if set && s.ValueFrom != nil {
		return refuse(CodeExclusiveFields, "a Key's value is supplied or read from a variable, not both", "spec.value", "spec.valueFrom.env")
	}
	if set {
		if o.FileMode {
			return refuse(CodeInvalidField, "a supplied value is server mode's; the file mode reads a Key's value from valueFrom.env", "spec.value")
		}
		// On an update the presence alone is refused at stage 5, whatever
		// the value.
		if o.Existing == nil && (len(value) < minSuppliedValue || len(value) > maxSuppliedValue) {
			return refuse(CodeInvalidField, "a supplied value is 32 to 4096 bytes; this one is "+strconv.Itoa(len(value)), "spec.value")
		}
	}
	if s.ValueFrom != nil {
		if err := checkEnvName(s.ValueFrom.Env); err != nil {
			return refuse(CodeInvalidField, err.Error(), "spec.valueFrom.env")
		}
		if !o.FileMode {
			return refuse(CodeInvalidField, "valueFrom.env is the file mode's; in server mode the server mints the value or the caller supplies one", "spec.valueFrom.env")
		}
	}
	if r := s.Limits.RequestsPerMinute; r != nil && *r < 0 {
		return refuse(CodeInvalidField, "requestsPerMinute is 0 or more", "spec.limits.requestsPerMinute")
	}
	if r := s.Limits.TokensPerMinute; r != nil && *r < 0 {
		return refuse(CodeInvalidField, "tokensPerMinute is 0 or more", "spec.limits.tokensPerMinute")
	}
	if sp := s.Limits.Spend; sp != nil {
		if sp.Amount == nil {
			return refuse(CodeMissingField, "a spend limit needs an amount", "spec.limits.spend.amount")
		}
		if *sp.Amount <= 0 {
			return refuse(CodeInvalidField, "a spend amount is positive", "spec.limits.spend.amount")
		}
		if sp.Currency != "" {
			if err := checkCurrency(sp.Currency); err != nil {
				return refuse(CodeInvalidField, err.Error(), "spec.limits.spend.currency")
			}
		}
		if sp.Window != "" {
			if err := sp.Window.Validate(); err != nil {
				return refuse(CodeInvalidField, err.Error(), "spec.limits.spend.window")
			}
		}
	}
	if s.Budget != "" {
		if err := checkReference(s.Budget, budgetRef, "Budget"); err != nil {
			return refuse(CodeInvalidField, err.Error(), "spec.budget")
		}
	}
	if s.TTL != "" && !s.ExpiresAt.IsZero() {
		return refuse(CodeExclusiveFields, "an expiry is a ttl or an instant, not both", "spec.ttl", "spec.expiresAt")
	}
	if s.TTL != "" {
		d, err := s.TTL.Parse()
		if err != nil {
			return refuse(CodeInvalidField, strconv.Quote(string(s.TTL))+" is not a Go duration", "spec.ttl")
		}
		if d < time.Minute {
			return refuse(CodeInvalidField, "ttl is at least 1m", "spec.ttl")
		}
	}
	if !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now) {
		return refuse(CodeInvalidField, "expiresAt "+s.ExpiresAt.UTC().Format(time.RFC3339)+" is not after now, "+now.UTC().Format(time.RFC3339), "spec.expiresAt")
	}
	return nil
}

// checkBudget applies the Budget.spec table.
func checkBudget(b *v1.Budget) error {
	s := &b.Spec
	if s.Amount == nil {
		return refuse(CodeMissingField, "amount is required", "spec.amount")
	}
	if *s.Amount <= 0 {
		return refuse(CodeInvalidField, "amount is positive", "spec.amount")
	}
	if s.Currency != "" {
		if err := checkCurrency(s.Currency); err != nil {
			return refuse(CodeInvalidField, err.Error(), "spec.currency")
		}
	}
	if s.Window != "" {
		if err := s.Window.Validate(); err != nil {
			return refuse(CodeInvalidField, err.Error(), "spec.window")
		}
	}
	return nil
}
