// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"time"
)

// Provider is one upstream: the dialect it speaks, its base URL, the
// credential the gateway holds for it, and how its model list is
// discovered and its health probed.
type Provider struct {
	Metadata ObjectMeta     `json:"metadata"`
	Spec     ProviderSpec   `json:"spec"`
	Status   ProviderStatus `json:"status"`
}

// Kind implements Object.
func (p *Provider) Kind() string { return KindProvider }

// ID implements Object.
func (p *Provider) ID() string { return p.Status.ID }

// Owner implements Object.
func (p *Provider) Owner() string { return p.Status.Owner }

// Name implements Object.
func (p *Provider) Name() string { return p.Metadata.Name }

// MarshalJSON writes the envelope before the object.
func (p Provider) MarshalJSON() ([]byte, error) {
	type plain Provider
	return json.Marshal(struct {
		envelope
		plain
	}{envelope{APIVersion, KindProvider}, plain(p)})
}

// ProviderSpec is the desired state of a Provider. The bool fields carry
// no omitempty so a default of false is visible in a resolved object, as
// every default is.
type ProviderSpec struct {
	Dialect     Dialect           `json:"dialect,omitempty"`
	Tunnel      bool              `json:"tunnel"`
	BaseURL     string            `json:"baseURL,omitempty"`
	Credential  *Credential       `json:"credential,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Discovery   Discovery         `json:"discovery,omitzero"`
	Health      Health            `json:"health,omitzero"`
	Timeout     Duration          `json:"timeout,omitempty"`
	Concurrency int               `json:"concurrency"`
}

// Credential is how the gateway authenticates toward the upstream. The
// value itself is write-only: it is decoded into an unexported member the
// encoders skip and read through Value, so no encoding of a Provider can
// carry it.
type Credential struct {
	value     writeOnly  `writeonly:"value"`
	ValueFrom *ValueFrom `json:"valueFrom,omitempty"`
	Header    string     `json:"header,omitempty"`
	Scheme    Scheme     `json:"scheme,omitempty"`
}

// Value returns the credential value the manifest carried and whether it
// carried one. The store reads it here once and takes it out.
func (c *Credential) Value() (string, bool) { return c.value.get() }

// SetValue records a value the decoder read, or one an importer supplies.
func (c *Credential) SetValue(v string) { c.value.put(v) }

// ClearValue drops the value, which the store does after sealing it.
func (c *Credential) ClearValue() { c.value.clear() }

// ValueFrom names where a value is read from in the file mode.
type ValueFrom struct {
	Env string `json:"env,omitempty"`
}

// Discovery is how the Provider's model list becomes Models.
type Discovery struct {
	Mode    DiscoveryMode `json:"mode,omitempty"`
	Include []string      `json:"include,omitempty"`
	Exclude []string      `json:"exclude,omitempty"`
}

// Health is how the Provider's reachability is observed.
type Health struct {
	Mode HealthMode `json:"mode,omitempty"`
}

// ProviderStatus is written by the server and ignored on apply. Resolve
// fills Warnings; the API and the jobs fill the rest.
type ProviderStatus struct {
	ID         string            `json:"id,omitempty"`
	Version    int64             `json:"version,omitempty"`
	Owner      string            `json:"owner,omitempty"`
	Credential *CredentialStatus `json:"credential,omitempty"`
	Health     *HealthStatus     `json:"health,omitempty"`
	Discovered *DiscoveredStatus `json:"discovered,omitempty"`
	Tunnel     *TunnelStatus     `json:"tunnel,omitempty"`
	CreatedAt  time.Time         `json:"createdAt,omitzero"`
	UpdatedAt  time.Time         `json:"updatedAt,omitzero"`
	Warnings   []string          `json:"warnings"`
}

// CredentialStatus says whether a value is stored and which one, and never
// anything of the value itself.
type CredentialStatus struct {
	Set       bool      `json:"set"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// HealthStatus is the published health of a Provider.
type HealthStatus struct {
	State       HealthState `json:"state"`
	Since       time.Time   `json:"since,omitzero"`
	LastProbeAt time.Time   `json:"lastProbeAt,omitzero"`
	LastError   string      `json:"lastError"`
}

// DiscoveredStatus is the last successful model list: how many Models it
// left, when, and one warning per upstream name the schema refused,
// naming the name and the rule, so a name that cannot be a Model is
// visible without being stored.
type DiscoveredStatus struct {
	Count    int       `json:"count"`
	At       time.Time `json:"at,omitzero"`
	Warnings []string  `json:"warnings,omitempty"`
}

// TunnelStatus is the session of a tunneled Provider.
type TunnelStatus struct {
	State           TunnelState `json:"state"`
	Session         string      `json:"session,omitempty"`
	Subject         string      `json:"subject,omitempty"`
	Agent           string      `json:"agent,omitempty"`
	Since           time.Time   `json:"since,omitzero"`
	LastHeartbeatAt time.Time   `json:"lastHeartbeatAt,omitzero"`
}

// Dialect is the wire API an upstream speaks and the door a caller uses.
type Dialect string

// The dialects.
const (
	DialectOpenAI    Dialect = "openai"
	DialectAnthropic Dialect = "anthropic"
	DialectGemini    Dialect = "gemini"
	DialectLux       Dialect = "lux"
)

// Valid reports whether d is one of the dialects.
func (d Dialect) Valid() bool {
	switch d {
	case DialectOpenAI, DialectAnthropic, DialectGemini, DialectLux:
		return true
	default:
		return false
	}
}

// CredentialHeader is the header the dialect's credential travels in by
// default: Authorization for openai and lux, x-api-key for anthropic,
// x-goog-api-key for gemini. An unknown dialect has none.
func (d Dialect) CredentialHeader() string {
	switch d {
	case DialectOpenAI, DialectLux:
		return "Authorization"
	case DialectAnthropic:
		return "x-api-key"
	case DialectGemini:
		return "x-goog-api-key"
	default:
		return ""
	}
}

// CredentialScheme is the default scheme for the dialect's header: bearer
// on Authorization, raw elsewhere.
func (d Dialect) CredentialScheme() Scheme {
	switch d {
	case DialectOpenAI, DialectLux:
		return SchemeBearer
	case DialectAnthropic, DialectGemini:
		return SchemeRaw
	default:
		return ""
	}
}

// Scheme is how a credential value is written into its header.
type Scheme string

// The schemes: bearer prefixes "Bearer ", raw writes the value verbatim.
const (
	SchemeBearer Scheme = "bearer"
	SchemeRaw    Scheme = "raw"
)

// Valid reports whether s is one of the schemes.
func (s Scheme) Valid() bool {
	switch s {
	case SchemeBearer, SchemeRaw:
		return true
	default:
		return false
	}
}

// DiscoveryMode is whether the model list is read.
type DiscoveryMode string

// The discovery modes.
const (
	DiscoveryAuto DiscoveryMode = "auto"
	DiscoveryNone DiscoveryMode = "none"
)

// Valid reports whether m is one of the modes.
func (m DiscoveryMode) Valid() bool {
	switch m {
	case DiscoveryAuto, DiscoveryNone:
		return true
	default:
		return false
	}
}

// HealthMode is the source of the published health.
type HealthMode string

// The health modes.
const (
	HealthProbe   HealthMode = "probe"
	HealthPassive HealthMode = "passive"
	HealthNone    HealthMode = "none"
)

// Valid reports whether m is one of the modes.
func (m HealthMode) Valid() bool {
	switch m {
	case HealthProbe, HealthPassive, HealthNone:
		return true
	default:
		return false
	}
}

// HealthState is a Provider's published reachability.
type HealthState string

// The health states, in the order a worse one follows a better one.
const (
	HealthUnknown     HealthState = "Unknown"
	HealthHealthy     HealthState = "Healthy"
	HealthDegraded    HealthState = "Degraded"
	HealthUnreachable HealthState = "Unreachable"
)

// Valid reports whether s is one of the states.
func (s HealthState) Valid() bool {
	switch s {
	case HealthUnknown, HealthHealthy, HealthDegraded, HealthUnreachable:
		return true
	default:
		return false
	}
}

// TunnelState is whether a tunneled Provider has a live session.
type TunnelState string

// The tunnel states.
const (
	TunnelConnected    TunnelState = "Connected"
	TunnelDisconnected TunnelState = "Disconnected"
)

// Valid reports whether s is one of the states.
func (s TunnelState) Valid() bool {
	switch s {
	case TunnelConnected, TunnelDisconnected:
		return true
	default:
		return false
	}
}
