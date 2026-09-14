// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// delete is DELETE /v1/{kind}s/{id-or-name}: load, authorize the kind's
// delete on what was loaded, hold If-Match, refuse a Budget a Key names
// and a Provider a declared Model targets, then one transaction for the
// row, a Key's hash, a Provider's discovered Models and credential, and
// the journal row; 204 with no body.
func (c *call) delete(ctx context.Context, k kind, ref string) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	pre, err := parsePrecondition(c.r.Header)
	if err != nil {
		return err
	}
	if pre.free {
		return refuse(CodeInvalidField, "If-None-Match has no meaning on a delete", "If-None-Match")
	}
	obj, version, err := c.load(ctx, k, ref)
	if err != nil {
		return err
	}
	res, _ := auth.ResourceFor(k.del, obj)
	if _, err := c.authorize(ctx, k.del, res); err != nil {
		return err
	}
	if err := pre.check(true, version); err != nil {
		return err
	}
	if err := c.checkInUse(ctx, obj); err != nil {
		return err
	}
	terr := c.h.o.Store.Transact(ctx, func(tx store.Store) error {
		if err := c.deleteDependents(ctx, tx, obj); err != nil {
			return err
		}
		if err := tx.Objects().Delete(ctx, k.name, obj.ID()); err != nil {
			return err
		}
		return c.journal(ctx, tx, eventPrefix(k.name)+".deleted", obj, deletedData(obj))
	})
	if terr != nil {
		return mapError(terr)
	}
	if p, ok := obj.(*v1.Provider); ok && c.h.o.Clients != nil {
		c.h.o.Clients.Revoke(p.Status.ID)
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}

// checkInUse is budget_in_use while a Key names the Budget and
// provider_in_use while a declared Model targets the Provider.
func (c *call) checkInUse(ctx context.Context, obj v1.Object) *Error {
	objects := c.h.o.Store.Objects()
	switch x := obj.(type) {
	case *v1.Budget:
		keys, _, err := objects.List(ctx, v1.KindKey, store.Filter{}, store.Page{})
		if err != nil {
			return mapError(err)
		}
		n := 0
		for _, o := range keys {
			if k, ok := o.(*v1.Key); ok && k.Status.Budget != nil && k.Status.Budget.ID == x.Status.ID {
				n++
			}
		}
		if n > 0 {
			return refuse(CodeBudgetInUse, strconv.Itoa(n)+" live Key(s) draw from Budget "+x.Status.ID+"; move or delete them first")
		}
	case *v1.Provider:
		models, _, err := objects.List(ctx, v1.KindModel, store.Filter{Provider: x.Status.ID, Source: string(v1.SourceDeclared)}, store.Page{})
		if err != nil {
			return mapError(err)
		}
		if len(models) > 0 {
			return refuse(CodeProviderInUse, strconv.Itoa(len(models))+" declared Model(s) target Provider "+x.Status.ID+"; edit their targets first")
		}
	}
	return nil
}

// deleteDependents removes what goes with the object in its
// transaction: a Key's hash, a Provider's discovered Models and its
// credential row.
func (c *call) deleteDependents(ctx context.Context, tx store.Store, obj v1.Object) error {
	switch x := obj.(type) {
	case *v1.Key:
		if err := tx.Keys().Delete(ctx, x.Status.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	case *v1.Provider:
		models, _, err := tx.Objects().List(ctx, v1.KindModel, store.Filter{Provider: x.Status.ID, Source: string(v1.SourceDiscovered)}, store.Page{})
		if err != nil {
			return err
		}
		for _, m := range models {
			if err := tx.Objects().Delete(ctx, v1.KindModel, m.ID()); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if err := tx.Credentials().Delete(ctx, x.Status.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// deletedData is the data of a <kind>.deleted row: a Key's prefix,
// nothing for the other kinds.
func deletedData(obj v1.Object) map[string]any {
	if k, ok := obj.(*v1.Key); ok {
		return map[string]any{"prefix": k.Status.Prefix}
	}
	return map[string]any{}
}
