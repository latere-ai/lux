// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestObjectOwnersAnswerTheOwnerPolicy: a live object exists with its
// owner, a missing or deleted one does not exist and is no error, and
// the store's own failure is returned.
func TestObjectOwnersAnswerTheOwnerPolicy(t *testing.T) {
	st := memory.New()
	ctx := t.Context()
	b := &v1.Budget{Metadata: v1.ObjectMeta{Name: "team"}, Status: v1.BudgetStatus{ID: v1.NewID(v1.PrefixBudget, now(), nil), Owner: subject, Warnings: []string{}}}
	if _, err := st.Objects().Put(ctx, b, 0); err != nil {
		t.Fatal(err)
	}
	owners := &ObjectOwners{Objects: st.Objects()}
	obj, err := owners.Object(ctx, v1.KindBudget, b.Status.ID)
	if err != nil || !obj.Exists || obj.Owner != subject {
		t.Fatalf("a live Budget: %+v, %v", obj, err)
	}
	if obj, err := owners.Object(ctx, v1.KindKey, "key_01J9NOSUCHKEY00000000000000"); err != nil || obj.Exists {
		t.Fatalf("a missing Key: %+v, %v", obj, err)
	}
	if err := st.Objects().Delete(ctx, v1.KindBudget, b.Status.ID); err != nil {
		t.Fatal(err)
	}
	if obj, err := owners.Object(ctx, v1.KindBudget, b.Status.ID); err != nil || obj.Exists {
		t.Fatalf("a deleted Budget: %+v, %v", obj, err)
	}
	failing := &ObjectOwners{Objects: failingObjects{}}
	if _, err := failing.Object(ctx, v1.KindBudget, b.Status.ID); err == nil || !errors.Is(err, errStoreDown) {
		t.Fatalf("a failing store: %v", err)
	}
}

var errStoreDown = errors.New("store down")

// failingObjects is a store.Objects whose every read fails.
type failingObjects struct{ store.Objects }

func (failingObjects) Get(context.Context, string, string) (v1.Object, int64, error) {
	return nil, 0, errStoreDown
}
