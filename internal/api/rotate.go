// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// rotate is POST /v1/keys/{id-or-name}/rotate: a new lux_ value, the
// id, name, spec, owner, and windows kept, the hash replaced and the
// prefix rewritten in one transaction with the key.rotated row, and 200
// with status.value. A repeat is a second rotation; the route takes no
// idempotency key by design.
func (c *call) rotate(ref string) *Error {
	if err := c.authenticate(); err != nil {
		return err
	}
	pre, err := parsePrecondition(c.r.Header)
	if err != nil {
		return err
	}
	if pre.free {
		return refuse(CodeInvalidField, "If-None-Match has no meaning on a rotate", "If-None-Match")
	}
	k := kindOf(v1.KindKey)
	obj, version, err := c.load(k, ref)
	if err != nil {
		return err
	}
	key := obj.(*v1.Key)
	res, _ := auth.ResourceFor(k.update, key)
	if _, err := c.authorize(k.update, res); err != nil {
		return err
	}
	if err := pre.check(true, version); err != nil {
		return err
	}
	mint := c.h.o.MintKeyValue
	if mint == nil {
		mint = serve.MintKeyValue
	}
	value, merr := mint()
	if merr != nil {
		return refuse(CodeInternal, "")
	}
	previous := key.Status.Prefix
	key.Status.Prefix = serve.KeyPrefix(value, false)
	key.Status.UpdatedAt = c.start
	ctx := c.r.Context()
	terr := c.h.o.Store.Transact(ctx, func(tx store.Store) error {
		if err := tx.Keys().Put(ctx, key.Status.ID, serve.HashKeyValue(value)); err != nil {
			return err
		}
		if _, err := tx.Objects().Put(ctx, key, version); err != nil {
			return err
		}
		return c.journal(ctx, tx, "key.rotated", key, map[string]any{"prefix": key.Status.Prefix, "previousPrefix": previous})
	})
	if terr != nil {
		return mapError(terr)
	}
	if err := c.render(ctx, key); err != nil {
		return err
	}
	key.Status.Value = value
	return c.writeJSON(http.StatusOK, key, key.Status.Version)
}
