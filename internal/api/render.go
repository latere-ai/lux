// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"latere.ai/x/lux/internal/serve"
	v1 "latere.ai/x/lux/manifest/v1"
)

// render fills the read-time status of a Key or a Budget from the
// counters, spec 007's RenderKey and RenderBudget, and leaves the other
// kinds as the store returned them. No object carries a value: a Key's
// status.value is the create and rotate responses' alone and is set
// after this, and a Provider's credential value has no encoding.
func (c *call) render(ctx context.Context, obj v1.Object) *Error {
	var err error
	switch x := obj.(type) {
	case *v1.Key:
		x.Status.Value = ""
		err = serve.RenderKey(ctx, c.h.o.Store, x, c.h.o.Now())
	case *v1.Budget:
		err = serve.RenderBudget(ctx, c.h.o.Store, x, c.h.o.Now())
	}
	if err != nil {
		return mapError(err)
	}
	return nil
}

// setIdentity writes id, owner, and createdAt into the object's status,
// the three members the API fills before Put beside the kind's own.
func setIdentity(obj v1.Object, id, owner string, createdAt timeRef) {
	switch x := obj.(type) {
	case *v1.Provider:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt.t
	case *v1.Model:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt.t
	case *v1.Key:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt.t
	case *v1.Budget:
		x.Status.ID, x.Status.Owner, x.Status.CreatedAt = id, owner, createdAt.t
	}
}

// versionOf is the status.version the store wrote back.
func versionOf(obj v1.Object) int64 {
	switch x := obj.(type) {
	case *v1.Provider:
		return x.Status.Version
	case *v1.Model:
		return x.Status.Version
	case *v1.Key:
		return x.Status.Version
	default:
		return obj.(*v1.Budget).Status.Version
	}
}

// createdAtOf is the object's status.createdAt.
func createdAtOf(obj v1.Object) timeRef {
	switch x := obj.(type) {
	case *v1.Provider:
		return timeRef{x.Status.CreatedAt}
	case *v1.Model:
		return timeRef{x.Status.CreatedAt}
	case *v1.Key:
		return timeRef{x.Status.CreatedAt}
	default:
		return timeRef{obj.(*v1.Budget).Status.CreatedAt}
	}
}
