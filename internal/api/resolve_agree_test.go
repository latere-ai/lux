// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// importerLookup is the manifest.Lookup an importer builds: the store's
// objects read straight, with no authorizer in the path. It gives the
// same answers the stub authorizer does when it allows everything, so a
// Resolve over it and one through the API's authorizer-backed Lookup
// resolve the same references. references already reads the store for a
// Provider and a Budget; Models is adapted to the ModelRefs Resolve
// wants from the *v1.Model the catalog returns.
type importerLookup struct{ *references }

func (l importerLookup) Models(ctx context.Context, selector string) ([]v1.ModelRef, error) {
	ms, err := l.references.Models(ctx, selector)
	if err != nil {
		return nil, err
	}
	refs := make([]v1.ModelRef, 0, len(ms))
	for _, m := range ms {
		refs = append(refs, v1.ModelRef{ID: m.Status.ID, Name: m.Metadata.Name, Owner: m.Status.Owner, Labels: maps.Clone(m.Metadata.Labels)})
	}
	return refs, nil
}

// TestAPIAndImporterResolveAgree is [[001-architecture]]'s invariant 1,
// one resolver: a manifest applied through the API's PUT handler and the
// same bytes handed to manifest.Resolve with the options an importer uses
// resolve to a byte-identical body. The API returns exactly Resolve's
// output with the status it owns merged in, so apiVersion, kind,
// metadata, and the whole defaulted spec are byte-for-byte identical
// between the two surfaces, and the only member the API adds is status,
// the id, owner, timestamps, and read-time counters the control plane
// fills. The test proves that by comparing the two responses with status
// excluded, and by checking that status is where the difference lives:
// the API fills status.id and Resolve leaves it empty.
func TestAPIAndImporterResolveAgree(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()

	base, err := url.Parse(publicURL)
	if err != nil {
		t.Fatal(err)
	}

	// One manifest of each kind, applied under a fresh name so the create
	// path runs. Each names only the seeded objects, so both surfaces
	// resolve the same references: the Model's target is the seeded
	// Provider, the Key's selector the seeded Model and its Budget the
	// seeded Budget.
	cases := []struct {
		kind, path, body string
	}{
		{v1.KindProvider, "/v1/providers/backup", providerJSON},
		{v1.KindModel, "/v1/models/gpt-5-mini", modelJSON},
		{v1.KindBudget, "/v1/budgets/research", budgetJSON},
		{v1.KindKey, "/v1/keys/team-key", keyJSON},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			name := tc.path[len(pathPrefixOf(tc.kind)):]

			rec := h.request(http.MethodPut, tc.path, tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("PUT %s: %d %s", tc.path, rec.Code, rec.Body.String())
			}
			apiBody := bytes.Clone(rec.Body.Bytes())

			in, derr := manifest.Decode([]byte(tc.body), manifest.MediaJSON, manifest.Hint{APIVersion: v1.APIVersion, Kind: tc.kind, Name: name})
			if derr != nil {
				t.Fatalf("Decode: %v", derr)
			}
			// The options an importer resolves under, matching the ones the
			// API's apply passes: the same Defaults, PublicURL, and clock,
			// a create (Existing nil), server mode, no authorizer ceilings
			// (the stub grants none). The Actor is read for nothing Resolve
			// puts in the object.
			resolved, rerr := manifest.Resolve(context.Background(), in, manifest.Options{
				Actor:     manifest.Actor{Subject: h.subject()},
				Lookup:    importerLookup{&references{objects: h.st.Objects()}},
				Defaults:  manifest.Defaults{RequestsPerMinute: 60, TokensPerMinute: 1000, Timeout: 10 * time.Minute},
				PublicURL: base,
				Now:       h.clock,
			})
			if rerr != nil {
				t.Fatalf("Resolve: %v", rerr)
			}
			importerBody, err := json.Marshal(resolved.Object)
			if err != nil {
				t.Fatal(err)
			}

			assertResolvedBodiesAgree(t, tc.kind, apiBody, importerBody)
		})
	}
}

// pathPrefixOf is /v1/{plural}/ for a kind, so the trailing segment of a
// route is the name.
func pathPrefixOf(kind string) string { return "/v1/" + kindOf(kind).plural + "/" }

// assertResolvedBodiesAgree holds the two responses byte-identical over
// every top-level member but status. It first proves status is where the
// surfaces differ, so excluding it cannot hide a disagreement: the API
// fills status.id and Resolve does not.
func assertResolvedBodiesAgree(t *testing.T, kind string, apiBody, importerBody []byte) {
	t.Helper()
	api := topLevel(t, apiBody)
	imp := topLevel(t, importerBody)

	if idOf(t, api["status"]) == nil {
		t.Errorf("%s: the API response fills no status.id; the control plane owns it", kind)
	}
	if idOf(t, imp["status"]) != nil {
		t.Errorf("%s: Resolve fills status.id, which only the control plane should", kind)
	}

	delete(api, "status")
	delete(imp, "status")
	if a, i := keysOf(api), keysOf(imp); !slices.Equal(a, i) {
		t.Fatalf("%s: top-level members differ (excluding status): API %v, importer %v", kind, a, i)
	}
	for k, want := range imp {
		if got := api[k]; !bytes.Equal(got, want) {
			t.Errorf("%s: %q is not byte-identical between the surfaces\n  API:      %s\n  importer: %s", kind, k, got, want)
		}
	}
}

// topLevel splits a response into its top-level members, their raw bytes
// kept so the comparison is byte-exact and no number loses precision.
func topLevel(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	return m
}

// idOf reads status.id from a raw status object, nil when there is none.
func idOf(t *testing.T, status json.RawMessage) any {
	t.Helper()
	if len(status) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(status, &m); err != nil {
		t.Fatalf("status is not an object: %v\n%s", err, status)
	}
	return m["id"]
}

// keysOf is the sorted member names of a top-level object.
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
