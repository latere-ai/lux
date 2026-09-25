// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"time"
)

// Model is one routable name: the targets it reaches on which Providers
// with which upstream names, the fallback order, pricing, and modalities.
type Model struct {
	Metadata ObjectMeta  `json:"metadata"`
	Spec     ModelSpec   `json:"spec"`
	Status   ModelStatus `json:"status"`
}

// Kind implements Object.
func (m *Model) Kind() string { return KindModel }

// ID implements Object.
func (m *Model) ID() string { return m.Status.ID }

// Owner implements Object.
func (m *Model) Owner() string { return m.Status.Owner }

// Name implements Object.
func (m *Model) Name() string { return m.Metadata.Name }

// MarshalJSON writes the envelope before the object.
func (m Model) MarshalJSON() ([]byte, error) {
	type plain Model
	return json.Marshal(struct {
		envelope
		plain
	}{envelope{APIVersion, KindModel}, plain(m)})
}

// ModelSpec is the desired state of a Model.
type ModelSpec struct {
	Targets         []Target   `json:"targets,omitempty"`
	Fallback        Fallback   `json:"fallback,omitempty"`
	Pricing         *Pricing   `json:"pricing,omitempty"`
	Modalities      Modalities `json:"modalities,omitzero"`
	ContextWindow   int        `json:"contextWindow,omitempty"`
	MaxOutputTokens int        `json:"maxOutputTokens,omitempty"`
	// Disabled refuses every call to the Model with model_disabled and
	// leaves it out of every model list while true (spec 039); the Keys
	// that name it are unchanged, so setting it back restores them.
	Disabled bool `json:"disabled,omitempty"`
}

// Target is one Provider the Model reaches and the upstream's own name for
// it. Weight is a pointer because an explicit 0, fallback only, differs
// from an absent weight, which defaults to 100.
type Target struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Weight   *int   `json:"weight,omitempty"`
	Priority int    `json:"priority"`
}

// Pricing quotes money per Per tokens. The four prices are pointers so an
// absent price is told from an explicit "0", which a free model may
// declare; after Resolve every one of them is set.
type Pricing struct {
	Currency    string `json:"currency,omitempty"`
	Per         int    `json:"per,omitempty"`
	Input       *Money `json:"input,omitempty"`
	Output      *Money `json:"output,omitempty"`
	CachedInput *Money `json:"cachedInput,omitempty"`
	CacheWrite  *Money `json:"cacheWrite,omitempty"`
}

// Modalities are what the Model takes in and gives out.
type Modalities struct {
	Input  []Modality `json:"input,omitempty"`
	Output []Modality `json:"output,omitempty"`
}

// ModelStatus is written by the server and ignored on apply. Resolve fills
// Warnings; the API, discovery, and the health job fill the rest.
type ModelStatus struct {
	ID        string         `json:"id,omitempty"`
	Version   int64          `json:"version,omitempty"`
	Owner     string         `json:"owner,omitempty"`
	Source    Source         `json:"source,omitempty"`
	Available *bool          `json:"available,omitempty"`
	Targets   []TargetStatus `json:"targets,omitempty"`
	CreatedAt time.Time      `json:"createdAt,omitzero"`
	UpdatedAt time.Time      `json:"updatedAt,omitzero"`
	Warnings  []string       `json:"warnings"`
}

// TargetStatus is one target and its Provider's published health.
type TargetStatus struct {
	Provider string      `json:"provider"`
	Model    string      `json:"model"`
	Health   HealthState `json:"health"`
}

// Fallback is what happens after a retryable failure on one target.
type Fallback string

// The fallback modes.
const (
	FallbackOnError Fallback = "onError"
	FallbackNever   Fallback = "never"
)

// Valid reports whether f is one of the modes.
func (f Fallback) Valid() bool {
	switch f {
	case FallbackOnError, FallbackNever:
		return true
	default:
		return false
	}
}

// Modality is one kind of content a Model takes or gives.
type Modality string

// The modalities.
const (
	ModalityText      Modality = "text"
	ModalityImage     Modality = "image"
	ModalityAudio     Modality = "audio"
	ModalityVideo     Modality = "video"
	ModalityFile      Modality = "file"
	ModalityEmbedding Modality = "embedding"
)

// Valid reports whether m is one of the modalities.
func (m Modality) Valid() bool {
	switch m {
	case ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ModalityFile, ModalityEmbedding:
		return true
	default:
		return false
	}
}

// Source is whether an operator declared the Model or discovery found it.
type Source string

// The sources.
const (
	SourceDeclared   Source = "declared"
	SourceDiscovered Source = "discovered"
)

// Valid reports whether s is one of the sources.
func (s Source) Valid() bool {
	switch s {
	case SourceDeclared, SourceDiscovered:
		return true
	default:
		return false
	}
}
