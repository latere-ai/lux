// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Stage 2, defaulting: every absent field with a default is set from the
// tables and from Defaults, so the resolved object shows what the gateway
// acts on. A field the caller set is never overwritten.

// defaultName fills an absent name from NewName, or refuses when there is
// no generator, which is the file mode's case.
func defaultName(meta *v1.ObjectMeta, o Options) error {
	if meta.Name != "" {
		return nil
	}
	if o.NewName == nil {
		return refuse(CodeMissingField, "metadata.name is required where no name generator is configured", "metadata.name")
	}
	meta.Name = o.NewName()
	return nil
}

func defaultProvider(p *v1.Provider, o Options) {
	s := &p.Spec
	if !s.Tunnel {
		if s.Credential == nil {
			s.Credential = &v1.Credential{}
		}
		if s.Credential.Header == "" {
			s.Credential.Header = s.Dialect.CredentialHeader()
		}
		if s.Credential.Scheme == "" {
			s.Credential.Scheme = s.Dialect.CredentialScheme()
		}
	}
	if s.Discovery.Mode == "" {
		s.Discovery.Mode = v1.DiscoveryAuto
	}
	if s.Health.Mode == "" {
		s.Health.Mode = v1.HealthProbe
	}
	if s.Timeout == "" && o.Defaults.Timeout > 0 {
		s.Timeout = v1.DurationOf(o.Defaults.Timeout)
	}
}

func defaultModel(m *v1.Model) {
	s := &m.Spec
	for i := range s.Targets {
		t := &s.Targets[i]
		if t.Model == "" {
			t.Model = m.Metadata.Name
		}
		if t.Weight == nil {
			t.Weight = ptr(100)
		}
	}
	if s.Fallback == "" {
		s.Fallback = v1.FallbackOnError
	}
	if p := s.Pricing; p != nil {
		if p.Currency == "" {
			p.Currency = "USD"
		}
		if p.Per == 0 {
			p.Per = 1_000_000
		}
		if p.CachedInput == nil {
			p.CachedInput = ptr(*p.Input)
		}
		if p.CacheWrite == nil {
			p.CacheWrite = ptr(*p.Input)
		}
	}
	if s.Modalities.Input == nil {
		s.Modalities.Input = []v1.Modality{v1.ModalityText}
	}
	if s.Modalities.Output == nil {
		s.Modalities.Output = []v1.Modality{v1.ModalityText}
	}
}

// defaultKey fills the Key's defaults and the effective expiry in status:
// spec.expiresAt when given, or createdAt plus ttl, where createdAt is the
// existing object's on an update and now on a create, so an update never
// moves an expiry the ttl fixed.
func defaultKey(k *v1.Key, o Options, now, createdAt time.Time) {
	s := &k.Spec
	if s.Limits.RequestsPerMinute == nil {
		s.Limits.RequestsPerMinute = ptr(o.Defaults.RequestsPerMinute)
	}
	if s.Limits.TokensPerMinute == nil {
		s.Limits.TokensPerMinute = ptr(o.Defaults.TokensPerMinute)
	}
	if s.Limits.Spend != nil && s.Limits.Spend.Currency == "" {
		s.Limits.Spend.Currency = "USD"
	}
	switch {
	case !s.ExpiresAt.IsZero():
		k.Status.ExpiresAt = s.ExpiresAt.UTC()
	case s.TTL != "":
		d, _ := s.TTL.Parse() // validated at stage 1
		if createdAt.IsZero() {
			createdAt = now
		}
		k.Status.ExpiresAt = createdAt.Add(d).UTC()
	}
}

func defaultBudget(b *v1.Budget) {
	s := &b.Spec
	if s.Currency == "" {
		s.Currency = "USD"
	}
	if s.Window == "" {
		s.Window = v1.WindowMonth
	}
	if s.Hard == nil {
		s.Hard = ptr(true)
	}
}

func ptr[T any](v T) *T { return &v }
