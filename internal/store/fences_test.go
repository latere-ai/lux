// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

func TestFenceAssertionsAndDisableOnly(t *testing.T) {
	f := KeyFence{Name: "key", Owner: "owner", Labels: map[string]string{"tenant": "one"}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []KeyFence{{}, {Name: "key"}, {Owner: "owner"}} {
		if bad.Validate() == nil {
			t.Fatal("incomplete assertion accepted")
		}
	}
	copy := f.Clone()
	copy.Labels["tenant"] = "two"
	if !f.Matches("owner", map[string]string{"tenant": "one"}) || f.Matches("other", f.Labels) || f.Matches("owner", copy.Labels) {
		t.Fatal("assertion changed")
	}
	old := &v1.Key{Metadata: v1.ObjectMeta{Name: "key"}, Status: v1.KeyStatus{ID: "key_id", Owner: "owner"}}
	next := *old
	if DisableOnly(nil, &next) || DisableOnly(old, nil) || DisableOnly(old, &next) {
		t.Fatal("invalid disable accepted")
	}
	next.Spec.Disabled = true
	if !DisableOnly(old, &next) {
		t.Fatal("exact disable rejected")
	}
	next.Status.Owner = "other"
	if DisableOnly(old, &next) {
		t.Fatal("owner replacement accepted")
	}
}
