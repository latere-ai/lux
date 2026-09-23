// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// timeRef carries a time by value through the kind switches.
type timeRef struct{ t time.Time }

// applyRetries is how many times a read-modify-write without a
// precondition is retried on a version conflict before conflict is
// returned.
const applyRetries = 3

// apply is PUT /v1/{kind}s/{name}: create when no object of the name is
// live, update when one is. The order is spec 011's: the id-prefix rule
// on the path, the bearer and the subject's bucket, the preconditions'
// syntax, the body within LUX_MAX_MANIFEST_BYTES, Decode with the
// route's hint, then, up to applyRetries times on a version conflict
// without a precondition, the read of the existing object, the
// authorizer's decision on the request's own action, the preconditions
// against what was read, Resolve with Existing, the status the API
// fills, and one Transact for the object, its journal row, and a Key's
// hash or a Provider's sealed credential.
func (c *call) apply(ctx context.Context, k kind, name string) *Error {
	if name == "" {
		return refuse(CodeNotFound, "the path names no "+k.name)
	}
	if hasKindPrefix(name) {
		return refuse(CodeInvalidField, "the path segment "+strconv.Quote(name)+" begins with an id prefix, and an apply is by name: read the object by its id and apply the name it returns", "metadata.name")
	}
	if err := c.authenticate(); err != nil {
		return err
	}
	pre, err := parsePrecondition(c.r.Header)
	if err != nil {
		return err
	}
	body, err := c.readBody()
	if err != nil {
		return err
	}
	in, derr := manifest.Decode(body, c.r.Header.Get("Content-Type"), manifest.Hint{APIVersion: v1.APIVersion, Kind: k.name, Name: name})
	if derr != nil {
		return mapError(derr)
	}
	for attempt := 1; ; attempt++ {
		status, obj, version, err := c.applyOnce(ctx, k, name, in, pre)
		if err != nil && err.Code == CodeConflict && pre.none() && attempt < applyRetries {
			continue
		}
		if err != nil {
			return err
		}
		return c.writeJSON(status, obj, version)
	}
}

// applyOnce is one read-resolve-write; a version conflict from the store
// is the conflict Error the caller retries.
func (c *call) applyOnce(ctx context.Context, k kind, name string, in v1.Object, pre precondition) (int, v1.Object, int64, *Error) {
	existing, version, err := c.loadByName(ctx, k, name)
	if err != nil {
		return 0, nil, 0, err
	}
	action, subject := k.create, in
	if existing != nil {
		action, subject = k.update, existing
	}
	owner, ownerErr := c.requestedOwner(existing)
	if ownerErr != nil {
		return 0, nil, 0, ownerErr
	}
	proposed, proposalErr := authorizer.Proposal(in, owner)
	if proposalErr != nil {
		return 0, nil, 0, refuse(CodeInvalidField, "the mutation proposal could not be encoded")
	}
	res, _ := authorizer.ResourceFor(action, subject)
	res.Fields["proposed"] = proposed
	decision, err := c.authorize(ctx, action, res)
	if err != nil {
		return 0, nil, 0, err
	}
	if existing != nil && owner != existing.Owner() {
		return 0, nil, 0, refuse(CodeInvalidField, "an existing object's owner cannot change", "Lux-Owner")
	}
	if existing == nil && owner != c.caller.Subject {
		// This extra check must not replace the ordinary action's limits or the
		// writer's rate memo, and reference checks continue as the real caller.
		_, assignErr := c.h.o.Authorizer.Decide(ctx, c.caller, authorizer.ActionOwnerAssign, authorizer.OwnerAssignment(k.name, name, owner, proposed), c.info())
		if assignErr != nil {
			return 0, nil, 0, mapError(assignErr)
		}
	}
	if err := pre.check(existing != nil, version); err != nil {
		return 0, nil, 0, err
	}
	refs := &references{objects: c.h.o.Store.Objects()}
	resolvedAt := c.h.o.Now()
	obj := exactKeyDisable(in, existing)
	if obj == nil {
		resolved, rerr := manifest.Resolve(ctx, in, manifest.Options{
			Actor:                 manifest.Actor{Subject: c.caller.Subject},
			Lookup:                c.lookup(refs).ForMutation(k.name, proposed),
			Defaults:              c.h.o.Defaults,
			Limits:                decision.Limits.Key,
			Existing:              existing,
			AllowPrivateUpstreams: c.h.o.AllowPrivateUpstreams,
			TunnelEnabled:         c.h.o.TunnelEnabled,
			PublicURL:             c.h.o.PublicURL,
			Now:                   func() time.Time { return resolvedAt },
			NewName:               c.h.o.NewName,
		})
		if rerr != nil {
			return 0, nil, 0, mapError(rerr)
		}
		obj = resolved.Object
	}
	w, err := c.prepareWrite(obj, existing, refs, owner, resolvedAt)
	if err != nil {
		return 0, nil, 0, err
	}
	terr := c.h.o.Store.Transact(ctx, func(tx store.Store) error {
		if existing == nil && k.name == v1.KindKey && decision.Limits.MaxKeys > 0 {
			if err := c.checkMaxKeys(ctx, tx, owner, decision.Limits.MaxKeys); err != nil {
				return err
			}
		}
		if _, err := tx.Objects().Put(ctx, obj, version); err != nil {
			return err
		}
		if err := w.write(ctx, tx); err != nil {
			return err
		}
		typ, data := eventPrefix(k.name)+".created", createdData(obj)
		if existing != nil {
			typ, data = eventPrefix(k.name)+".updated", map[string]any{"paths": events.ChangedPaths(existing, obj)}
		}
		return c.journal(ctx, tx, typ, obj, data)
	})
	if terr != nil {
		return 0, nil, 0, mapError(terr)
	}
	c.committed(ctx)
	if err := c.render(ctx, obj); err != nil {
		return 0, nil, 0, err
	}
	w.after(obj)
	status := http.StatusOK
	if existing == nil {
		status = http.StatusCreated
	}
	return status, obj, versionOf(obj), nil
}

// checkMaxKeys is the max_keys ceiling of spec 006 at key.create, inside
// the write's transaction: the subject's live Keys against the cap.
func (c *call) checkMaxKeys(ctx context.Context, tx store.Store, owner string, maxKeys int) error {
	keys, _, err := tx.Objects().List(ctx, v1.KindKey, store.Filter{Owner: owner}, store.Page{})
	if err != nil {
		return err
	}
	if len(keys) >= maxKeys {
		return refuse(CodeCeilingExceeded, "the subject holds "+strconv.Itoa(len(keys))+" live Key(s), and the authorizer's max_keys is "+strconv.Itoa(maxKeys))
	}
	return nil
}

// write is what one apply writes beside the object, and what it sets on
// the response after the row is committed.
type write struct {
	write func(ctx context.Context, tx store.Store) error
	after func(obj v1.Object)
}

// prepareWrite fills the status members the API owns before Put, id,
// owner, and createdAt for every kind and the kind's own, and returns
// the transaction's other writes: a Key's hash, a Provider's sealed
// credential. The Key's value, minted or supplied, leaves the object
// here; a minted one returns on the response alone.
func (c *call) prepareWrite(obj, existing v1.Object, refs *references, owner string, resolvedAt time.Time) (write, *Error) {
	w := write{write: func(context.Context, store.Store) error { return nil }, after: func(v1.Object) {}}
	id, createdAt := c.h.o.NewID(kindOf(obj.Kind()).prefix), timeRef{resolvedAt}
	if existing != nil {
		id, owner, createdAt = existing.ID(), existing.Owner(), createdAtOf(existing)
	}
	setIdentity(obj, id, owner, createdAt)
	switch x := obj.(type) {
	case *v1.Key:
		return c.prepareKey(x, existing, refs)
	case *v1.Provider:
		return c.prepareProvider(x, existing)
	}
	return w, nil
}

// prepareKey is the Key's half: status.budget from the Lookup, the
// prefix kept on an update, and on a create one hash registered in the
// same transaction, whether the caller supplied the value, supplied the
// hash of a value it holds alone, or left the gateway to mint one. A
// hash the caller supplied is the hash its value would have produced,
// so the three forms write one row and the door has one lookup.
func (c *call) prepareKey(k *v1.Key, existing v1.Object, refs *references) (write, *Error) {
	w := write{write: func(context.Context, store.Store) error { return nil }, after: func(v1.Object) {}}
	if k.Spec.Budget != "" && refs.budget != nil {
		k.Status.Budget = &v1.BudgetRef{Name: refs.budget.Metadata.Name, ID: refs.budget.Status.ID}
	}
	if old, ok := existing.(*v1.Key); ok {
		k.Status.Prefix = old.Status.Prefix
		k.Spec.ClearValue()
		k.Spec.ClearValueSHA256()
		return w, nil
	}
	value, supplied := k.Spec.Value()
	hash, hashed := k.Spec.ValueSHA256()
	k.Spec.ClearValueSHA256()
	if !supplied && !hashed {
		mint := c.h.o.MintKeyValue
		if mint == nil {
			mint = serve.MintKeyValue
		}
		var err error
		if value, err = mint(); err != nil {
			return w, refuse(CodeInternal, "")
		}
	}
	k.Spec.ClearValue()
	// The field the value arrived in, which the refusal of a hash
	// another Key already holds names.
	path := "spec.value"
	if hashed {
		// Resolve held the hash to the index's shape, so it is the row a
		// value of the caller's would have written.
		path = "spec.valueSHA256"
		k.Status.Prefix = serve.SuppliedKeyPrefix(hash)
	} else {
		hash = serve.HashKeyValue(value)
		k.Status.Prefix = serve.KeyPrefix(value, supplied)
	}
	w.write = func(ctx context.Context, tx store.Store) error {
		return hashTakenAt(tx.Keys().Put(ctx, k.Status.ID, hash), path)
	}
	if !supplied && !hashed {
		w.after = func(obj v1.Object) {
			if key, ok := obj.(*v1.Key); ok {
				key.Status.Value = value
			}
		}
	}
	return w, nil
}

// prepareProvider is the Provider's half: a value carried by the body
// is sealed under the first key encryption key and written in the same
// transaction, status.credential counting the values applied; a body
// without a value keeps the stored credential and its status, as spec
// 003's update rule says, so a manifest read back re-applies without
// touching the secret, and a Provider that never had one reads set
// false. Resolve fills the credential block for every Provider that is
// not tunneled, so the block's absence carries no meaning here.
func (c *call) prepareProvider(p *v1.Provider, existing v1.Object) (write, *Error) {
	w := write{write: func(context.Context, store.Store) error { return nil }, after: func(v1.Object) {}}
	old, _ := existing.(*v1.Provider)
	var stored *v1.CredentialStatus
	if old != nil {
		stored = old.Status.Credential
	}
	value, ok := "", false
	if p.Spec.Credential != nil {
		value, ok = p.Spec.Credential.Value()
		p.Spec.Credential.ClearValue()
	}
	if !ok {
		if stored != nil {
			p.Status.Credential = stored
		} else {
			p.Status.Credential = &v1.CredentialStatus{}
		}
		return w, nil
	}
	version := 1
	if stored != nil {
		version = stored.Version + 1
	}
	if c.h.o.Keys == nil {
		return w, refuse(CodeInternal, "")
	}
	sealed, err := c.h.o.Keys.Seal(p.Status.ID, version, []byte(value))
	if err != nil {
		return w, refuse(CodeInternal, "")
	}
	p.Status.Credential = &v1.CredentialStatus{Set: true, Version: version, UpdatedAt: c.start}
	w.write = func(ctx context.Context, tx store.Store) error { return tx.Credentials().Put(ctx, p.Status.ID, sealed) }
	return w, nil
}

// readBody reads the manifest within LUX_MAX_MANIFEST_BYTES: a
// Content-Length above it is refused before a byte is read, and a body
// that crosses it while streaming is refused where it crossed.
func (c *call) readBody() ([]byte, *Error) {
	limit := c.h.o.MaxManifestBytes
	if c.r.ContentLength > limit {
		return nil, refuse(CodeBodyTooLarge, "Content-Length "+strconv.FormatInt(c.r.ContentLength, 10)+" is above LUX_MAX_MANIFEST_BYTES "+strconv.FormatInt(limit, 10))
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.w, c.r.Body, limit))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, refuse(CodeBodyTooLarge, "the body crossed LUX_MAX_MANIFEST_BYTES "+strconv.FormatInt(limit, 10))
		}
		return nil, refuse(CodeMalformedBody, "reading the body: "+err.Error())
	}
	return body, nil
}

// requestedOwner preserves the existing owner unless an explicit header asks
// for another. The caller's ordinary permission is checked before rejecting
// an attempted transfer, so foreign object existence is not exposed by it.
func (c *call) requestedOwner(existing v1.Object) (string, *Error) {
	owner := c.caller.Subject
	if existing != nil {
		owner = existing.Owner()
	}
	values := c.r.Header.Values("Lux-Owner")
	if len(values) == 0 {
		return owner, nil
	}
	if len(values) != 1 {
		return "", refuse(CodeInvalidField, "Lux-Owner must occur once", "Lux-Owner")
	}
	requested := values[0]
	issuer, sub, ok := authz.SplitSubject(requested)
	if !ok || issuer == "" || sub == "" || len(requested) > 2048 || strings.TrimSpace(requested) != requested {
		return "", refuse(CodeInvalidField, "Lux-Owner must name a rendered issuer|subject", "Lux-Owner")
	}
	return requested, nil
}

// exactKeyDisable retains resolved identity while reducing authority. The normal
// key.update decision and optimistic write still apply; reference access and
// current issuance ceilings cannot prevent this exact cleanup operation.
func exactKeyDisable(in, existing v1.Object) v1.Object {
	next, ok := in.(*v1.Key)
	old, had := existing.(*v1.Key)
	if !ok || !had || next == nil || old == nil {
		return nil
	}
	candidate := *next
	candidate.Status = old.Status
	if !store.DisableOnly(old, &candidate) {
		return nil
	}
	return &candidate
}
