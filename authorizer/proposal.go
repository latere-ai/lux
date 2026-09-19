// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"latere.ai/x/pkg/authz"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Proposal is the desired owner, metadata and spec of a mutation, with status,
// credential values, supplied Key hashes, and Provider header values excluded.
// It is additive to the existing-object fields of an update resource. Header
// names and credential presence remain visible for admission policy.
func Proposal(obj v1.Object, owner string) (map[string]any, error) {
	var metadata v1.ObjectMeta
	var spec any
	var headerNames []string
	var credentialSupplied bool
	switch x := obj.(type) {
	case *v1.Provider:
		metadata = x.Metadata
		copy := x.Spec
		headerNames = slices.Sorted(maps.Keys(copy.Headers))
		copy.Headers = nil
		if copy.Credential != nil {
			credential := *copy.Credential
			_, credentialSupplied = credential.Value()
			credential.ClearValue()
			copy.Credential = &credential
		}
		spec = copy
	case *v1.Model:
		metadata, spec = x.Metadata, x.Spec
	case *v1.Key:
		metadata = x.Metadata
		copy := x.Spec
		copy.ClearValue()
		copy.ClearValueSHA256()
		spec = copy
	case *v1.Budget:
		metadata, spec = x.Metadata, x.Spec
	default:
		return nil, fmt.Errorf("authorizer: no proposal for %T", obj)
	}
	raw, err := json.Marshal(map[string]any{"owner": owner, "metadata": metadata, "spec": spec})
	if err != nil {
		return nil, err
	}
	var proposal map[string]any
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return nil, err
	}
	if key, ok := obj.(*v1.Key); ok {
		proposal["credential"] = keyCredential(key.Spec)
	}
	if obj.Kind() == v1.KindProvider {
		fields, _ := proposal["spec"].(map[string]any)
		if headerNames == nil {
			headerNames = []string{}
		}
		fields["headerNames"] = headerNames
		fields["credentialSupplied"] = credentialSupplied
	}
	return proposal, nil
}

// OwnerAssignment asks permission to choose the immutable owner of a new
// object. The caller remains the authenticated writer, never the target.
func OwnerAssignment(kind, name, owner string, proposed map[string]any) authz.Resource {
	return authz.NewResource(KindOwnership, "", map[string]any{"target_kind": kind, "name": name, "owner": owner, "proposed": proposed})
}

// keyCredential identifies the input without forwarding a usable credential or
// its verifier. The commitment hashes the exact verifier string, not its bytes
// decoded from hex, so the contract has no normalization ambiguity.
func keyCredential(spec v1.KeySpec) map[string]any {
	descriptor := map[string]any{"mode": "absent"}
	count := 0
	if _, ok := spec.Value(); ok {
		count++
		descriptor["mode"] = "inline"
	}
	if hash, ok := spec.ValueSHA256(); ok {
		count++
		descriptor["mode"] = "sha256"
		digest := sha256.Sum256([]byte(hash))
		descriptor["commitment"] = hex.EncodeToString(digest[:])
	}
	if spec.ValueFrom != nil {
		count++
		descriptor["mode"] = "reference"
	}
	if count > 1 {
		descriptor = map[string]any{"mode": "conflict"}
	}
	return descriptor
}
