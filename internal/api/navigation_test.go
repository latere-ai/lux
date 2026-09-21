// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOpenAPINavigationLabels(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodGet, "/v1/openapi.json", "", "Authorization", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("OpenAPI status = %d", rec.Code)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ summary, description string }{
		"listProviders":  {"List providers", "List Providers under the authorizer's filter, paged by limit and cursor."},
		"applyProvider":  {"Apply provider", "Create the Provider when no object of the name exists, update it when one does; 201 on create, 200 on update. Apply is by name and only by name."},
		"readProvider":   {"Read provider", "Read one Provider by id or name, with its status."},
		"deleteProvider": {"Delete provider", "Delete one Provider by id or name; 204 with no body."},
		"listModels":     {"List models", "List Models under the authorizer's filter, paged by limit and cursor."},
		"applyModel":     {"Apply model", "Create the Model when no object of the name exists, update it when one does; 201 on create, 200 on update. Apply is by name and only by name."},
		"readModel":      {"Read model", "Read one Model by id or name, with its status."},
		"deleteModel":    {"Delete model", "Delete one Model by id or name; 204 with no body."},
		"listKeys":       {"List keys", "List Keys under the authorizer's filter, paged by limit and cursor."},
		"applyKey":       {"Apply key", "Create the Key when no object of the name exists, update it when one does; 201 on create, 200 on update. Apply is by name and only by name."},
		"readKey":        {"Read key", "Read one Key by id or name, with its status."},
		"deleteKey":      {"Delete key", "Delete one Key by id or name; 204 with no body."},
		"fenceKey":       {"Fence key name", "Permanently close the name to credential changes. Idempotent for the same owner and exact labels; existing Keys still need conditional disable and cache drainage."},
		"readKeyFence":   {"Read key fence", "Read an immutable fence by Key name; authorization precedes existence lookup."},
		"rotateKey":      {"Rotate key", "Mint a new value for the Key, keeping its id, name, spec, owner, and windows; 200 with status.value. A repeat is a second rotation."},
		"listBudgets":    {"List budgets", "List Budgets under the authorizer's filter, paged by limit and cursor."},
		"applyBudget":    {"Apply budget", "Create the Budget when no object of the name exists, update it when one does; 201 on create, 200 on update. Apply is by name and only by name."},
		"readBudget":     {"Read budget", "Read one Budget by id or name, with its status."},
		"deleteBudget":   {"Delete budget", "Delete one Budget by id or name; 204 with no body."},
		"readUsage":      {"Aggregate usage", "Usage aggregated over a range, grouped by at most three dimensions and bucketed by an interval; unpaged, and no row sums two currencies. A filter outside the authorizer's own is an empty items."},
		"listRequests":   {"List requests", "Usage records over a range, newest first, paged by limit and cursor, with the record set that answered beside them."},
		"readSelf":       {"Read caller identity", "The caller's identity, who decides permission, and what this replica remembers granting the subject."},
		"readOpenAPI":    {"Read OpenAPI document", "This document as JSON; no bearer."},
		"readWellKnown":  {"Read server identity", "The server's identity: the build, the API and the doors under LUX_PUBLIC_URL, the issuers, the audience, and the mode; no bearer."},
	}
	seen := make(map[string]bool)
	for path, item := range doc.Paths {
		for method, raw := range item {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options", "trace":
			default:
				continue
			}
			var op struct {
				OperationID string `json:"operationId"`
				Summary     string `json:"summary"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatal(err)
			}
			words := strings.Fields(op.Summary)
			if len(words) < 2 || len(words) > 4 {
				t.Errorf("%s %s: summary must have 2–4 words: %q", method, path, op.Summary)
			}
			expected, ok := want[op.OperationID]
			if !ok {
				t.Errorf("%s %s: missing action label expectation for %s", method, path, op.OperationID)
				continue
			}
			if op.Summary != expected.summary {
				t.Errorf("%s summary = %q, want %q", op.OperationID, op.Summary, expected.summary)
			}
			if op.Description != expected.description {
				t.Errorf("%s lost operation detail: %q", op.OperationID, op.Description)
			}
			seen[op.OperationID] = true
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("missing operation %s", id)
		}
	}
}
