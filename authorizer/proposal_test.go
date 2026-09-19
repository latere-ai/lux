// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

func TestMutationProposalExcludesSecretsAndIncludesPolicy(t *testing.T) {
	key := fixtureKey()
	key.Spec.SetValue("key-secret-canary")
	key.Spec.SetValueSHA256(strings.Repeat("abcdef01", 8))
	key.Status.Value = "returned-key-canary"
	key.Spec.Disabled = true
	key.Spec.Passthrough = true
	key.Spec.AllowUnpriced = true
	provider := fixtureProvider()
	provider.Spec.Credential = &v1.Credential{Header: "X-API-Key"}
	provider.Spec.Credential.SetValue("provider-secret-canary")
	provider.Spec.Headers = map[string]string{"X-Private": "header-secret-canary"}
	for _, obj := range []v1.Object{key, provider, fixtureModel(), fixtureBudget(t)} {
		proposal, err := Proposal(obj, "issuer|target")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(proposal)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"key-secret-canary", "returned-key-canary", "provider-secret-canary", "header-secret-canary", strings.Repeat("abcdef01", 8), `"status"`} {
			if strings.Contains(string(raw), canary) {
				t.Fatalf("proposal exposes %q", canary)
			}
		}
		if proposal["owner"] != "issuer|target" || proposal["metadata"] == nil || proposal["spec"] == nil {
			t.Fatal("incomplete proposal")
		}
		spec := proposal["spec"].(map[string]any)
		switch obj.Kind() {
		case v1.KindKey:
			if spec["disabled"] != true || spec["passthrough"] != true || spec["allowUnpriced"] != true || spec["models"] == nil || spec["budget"] == nil {
				t.Fatal("key policy missing")
			}
		case v1.KindProvider:
			if spec["credentialSupplied"] != true || spec["headers"] != nil {
				t.Fatal("provider redaction missing")
			}
			names := spec["headerNames"].([]string)
			if len(names) != 1 || names[0] != "X-Private" {
				t.Fatal("header names missing")
			}
		}
	}
	if got, _ := provider.Spec.Credential.Value(); got != "provider-secret-canary" {
		t.Fatal("proposal mutated credential")
	}
	if got, _ := key.Spec.Value(); got != "key-secret-canary" {
		t.Fatal("proposal mutated key")
	}
	if _, err := Proposal(nil, "owner"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	key.Spec.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Proposal(key, "owner"); err == nil {
		t.Fatal("unencodable proposal accepted")
	}
	empty, err := Proposal(&v1.Provider{}, "owner")
	if err != nil || empty["spec"].(map[string]any)["headerNames"] == nil {
		t.Fatal("empty header names must be an array", err)
	}
}
