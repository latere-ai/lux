// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/internal/store"
)

// ObjectOwners answers the owner policy's question over the store: does
// an id of a kind name a live object, and who created it. It is the
// ObjectLookup spec 006's Authorizer takes, over the store's read path,
// which carries no credential value in any encoding.
type ObjectOwners struct {
	Objects store.Objects
}

// Object reports whether kind/id names a live object and its owner. A
// row that does not exist or is deleted does not exist to the policy,
// which denies every action on it but a create; the store's own failure
// is returned, and the policy reports it as no decision.
func (o *ObjectOwners) Object(ctx context.Context, kind, id string) (authz.Object, error) {
	obj, _, err := o.Objects.Get(ctx, kind, id)
	if errors.Is(err, store.ErrNotFound) {
		return authz.Object{}, nil
	}
	if err != nil {
		return authz.Object{}, fmt.Errorf("%s %s: %w", kind, id, err)
	}
	return authz.Object{Exists: true, Owner: obj.Owner()}, nil
}
