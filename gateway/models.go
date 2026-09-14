// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The list shapes of GET /v1/models per door, exactly as spec 004's
// table writes them, so an SDK's model picker reads its own dialect.

type openaiModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type openaiModelList struct {
	Object string        `json:"object"`
	Data   []openaiModel `json:"data"`
}

type anthropicModel struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

type anthropicModelList struct {
	Data    []anthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	FirstID *string          `json:"first_id"`
	LastID  *string          `json:"last_id"`
}

type geminiModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type geminiModelList struct {
	Models []geminiModel `json:"models"`
}

// The fixed timestamps of an entry: a Model has no creation time the
// dialects agree on, so every entry carries the epoch.
const (
	openaiCreated      = 0
	anthropicCreatedAt = "1970-01-01T00:00:00Z"
)

func openaiEntry(name string) openaiModel {
	return openaiModel{ID: name, Object: "model", Created: openaiCreated, OwnedBy: "lux"}
}

func anthropicEntry(name string) anthropicModel {
	return anthropicModel{Type: "model", ID: name, DisplayName: name, CreatedAt: anthropicCreatedAt}
}

func geminiEntry(name string) geminiModel {
	return geminiModel{Name: "models/" + name, DisplayName: name, SupportedGenerationMethods: []string{"generateContent", "countTokens"}}
}

// modelList renders the sorted names in the door's list shape.
func modelList(door v1.Dialect, names []string) []byte {
	var body any
	switch door {
	case v1.DialectOpenAI, v1.DialectLux:
		list := openaiModelList{Object: "list", Data: make([]openaiModel, 0, len(names))}
		for _, n := range names {
			list.Data = append(list.Data, openaiEntry(n))
		}
		body = list
	case v1.DialectAnthropic:
		list := anthropicModelList{Data: make([]anthropicModel, 0, len(names))}
		for _, n := range names {
			list.Data = append(list.Data, anthropicEntry(n))
		}
		if len(names) > 0 {
			list.FirstID, list.LastID = &names[0], &names[len(names)-1]
		}
		body = list
	case v1.DialectGemini:
		list := geminiModelList{Models: make([]geminiModel, 0, len(names))}
		for _, n := range names {
			list.Models = append(list.Models, geminiEntry(n))
		}
		body = list
	}
	out, _ := json.Marshal(body) // structs of strings and integers always marshal
	return append(out, '\n')
}

// modelEntry renders one Model in the door's entry shape.
func modelEntry(door v1.Dialect, name string) []byte {
	var body any
	switch door {
	case v1.DialectOpenAI, v1.DialectLux:
		body = openaiEntry(name)
	case v1.DialectAnthropic:
		body = anthropicEntry(name)
	case v1.DialectGemini:
		body = geminiEntry(name)
	}
	out, _ := json.Marshal(body) // as above
	return append(out, '\n')
}
