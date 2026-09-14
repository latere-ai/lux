// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// Factory constructs one empty store per case and cleans it up through
// t when the case ends.
type Factory func(t *testing.T) store.Store

// Run holds a store to the contract of spec 010, one case per acceptance
// row the contract can prove without a restart or a schema, each a
// subtest named as the row names its test.
func Run(t *testing.T, newStore Factory) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"TestOptimisticConcurrency", optimisticConcurrency},
		{"TestNamesAreUniqueAmongLiveObjects", namesAreUnique},
		{"TestDeclaredReplacesDiscoveredInPlace", declaredReplacesDiscovered},
		{"TestStatusHalvesAreSeparate", statusHalvesAreSeparate},
		{"TestTransactIsAtomic", transactIsAtomic},
		{"TestTransactRefusesNesting", transactRefusesNesting},
		{"TestListOrderAndCursor", listOrderAndCursor},
		{"TestFilterByProvider", filterByProvider},
		{"TestDeleteAndPrune", deleteAndPrune},
		{"TestPutKeepsNoValue", putKeepsNoValue},
		{"TestCountersAreAtomic", countersAreAtomic},
		{"TestNoneWindowIsNeverPruned", noneWindowIsNeverPruned},
		{"TestLeases", leases},
		{"TestJournalTail", journalTail},
		{"TestPendingIsOnePerObject", pendingIsOnePerObject},
		{"TestJournalByObjectPages", journalByObjectPages},
		{"TestKeyHashReplacesOnRotate", keyHashReplacesOnRotate},
		{"TestRewrapTouchesTheWrapColumnsOnly", rewrapTouchesTheWrapColumnsOnly},
		{"TestStoreCannotDecrypt", storeCannotDecrypt},
		{"TestTunnelRegistry", tunnelRegistry},
		{"TestAggregatesMatchTheRecords", aggregatesMatchTheRecords},
		{"TestNoCurrencyIsSummed", noCurrencyIsSummed},
		{"TestRecordsRingIsBounded", recordsRingIsBounded},
		{"TestReadyAndClose", readyAndClose},
		{"TestEndedContextIsAnswered", endedContextIsAnswered},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newStore(t)) })
	}
}

func optimisticConcurrency(t *testing.T, s store.Store) {
	ctx := t.Context()
	p := provider("openai")
	v, err := s.Objects().Put(ctx, p, 0)
	noErr(t, err, "create")
	equal(t, v, 1, "version after create")
	equal(t, p.Status.Version, 1, "status.version written back")
	truth(t, !p.Status.CreatedAt.IsZero() && p.Status.UpdatedAt.Equal(p.Status.CreatedAt), "timestamps written back on create")

	p.Spec.BaseURL = "https://api.example.com/v2"
	v, err = s.Objects().Put(ctx, p, 1)
	noErr(t, err, "update at the current version")
	equal(t, v, 2, "version after update")

	_, err = s.Objects().Put(ctx, p, 1)
	wantErr(t, err, store.ErrVersionConflict, "update at a stale version")

	got, gv, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	noErr(t, err, "Get")
	equal(t, gv, 2, "Get version")
	equal(t, as[*v1.Provider](t, got).Spec.BaseURL, "https://api.example.com/v2", "Get spec")
	equal(t, as[*v1.Provider](t, got).Status.Version, 2, "Get status.version")

	// Two writers at one version: exactly one succeeds.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			w := provider("openai")
			w.Status.ID = p.Status.ID
			_, err := s.Objects().Put(ctx, w, 2)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent Put: %v", err)
		}
	}
	equal(t, ok, 1, "successful writers")
	equal(t, conflicts, 1, "conflicting writers")

	// An id already used is never a create; a missing id is never an update.
	again := provider("other")
	again.Status.ID = p.Status.ID
	_, err = s.Objects().Put(ctx, again, 0)
	wantErr(t, err, store.ErrVersionConflict, "create over a used id")
	_, err = s.Objects().Put(ctx, provider("ghost"), 5)
	wantErr(t, err, store.ErrNotFound, "update of an unknown id")
	_, _, err = s.Objects().Get(ctx, v1.KindKey, p.Status.ID)
	wantErr(t, err, store.ErrNotFound, "Get under another kind")

	// A caller's mistakes are plain errors, none of the named ones.
	noID := provider("no-id")
	noID.Status.ID = ""
	_, err = s.Objects().Put(ctx, noID, 0)
	truth(t, err != nil && !isNamed(err), "Put with no id is a plain error")
	_, err = s.Objects().Put(ctx, provider(""), 0)
	truth(t, err != nil && !isNamed(err), "Put with no name is a plain error")
	_, err = s.Objects().Put(ctx, provider("negative"), -1)
	truth(t, err != nil && !isNamed(err), "Put at a negative version is a plain error")
	_, err = s.Objects().Put(ctx, nil, 0)
	truth(t, err != nil && !isNamed(err), "Put of nil is a plain error")
}

// isNamed reports whether err is one of the contract's errors.
func isNamed(err error) bool {
	for _, e := range store.Errors() {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func namesAreUnique(t *testing.T, s store.Store) {
	ctx := t.Context()
	first := provider("openai")
	_, err := s.Objects().Put(ctx, first, 0)
	noErr(t, err, "create")
	_, err = s.Objects().Put(ctx, provider("openai"), 0)
	wantErr(t, err, store.ErrNameTaken, "second create of one name")
	_, err = s.Objects().Put(ctx, key("openai"), 0)
	noErr(t, err, "the same name under another kind")

	// A rename onto a live name is refused; onto a free one moves the index.
	other := provider("azure")
	_, err = s.Objects().Put(ctx, other, 0)
	noErr(t, err, "create azure")
	other.Metadata.Name = "openai"
	_, err = s.Objects().Put(ctx, other, 1)
	wantErr(t, err, store.ErrNameTaken, "rename onto a live name")
	other.Metadata.Name = "gemini"
	_, err = s.Objects().Put(ctx, other, 1)
	noErr(t, err, "rename onto a free name")
	_, _, err = s.Objects().ByName(ctx, v1.KindProvider, "azure")
	wantErr(t, err, store.ErrNotFound, "the old name after a rename")

	noErr(t, s.Objects().Delete(ctx, v1.KindProvider, first.Status.ID), "delete")
	second := provider("openai")
	v, err := s.Objects().Put(ctx, second, 0)
	noErr(t, err, "create after delete")
	equal(t, v, 1, "a re-created name starts again at 1")
	got, _, err := s.Objects().ByName(ctx, v1.KindProvider, "openai")
	noErr(t, err, "ByName")
	equal(t, got.ID(), second.Status.ID, "ByName answers the live row")
	_, _, err = s.Objects().Get(ctx, v1.KindProvider, first.Status.ID)
	wantErr(t, err, store.ErrNotFound, "the deleted row by id")
}

func declaredReplacesDiscovered(t *testing.T, s store.Store) {
	ctx := t.Context()
	discovered := model("openai/gpt-5", "openai")
	discovered.Status.Source = v1.SourceDiscovered
	discovered.Status.Owner = "https://login.example.com|discovery"
	_, err := s.Objects().Put(ctx, discovered, 0)
	noErr(t, err, "discovery creates")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindModel, discovered.Status.ID, store.ModelObserved{Available: new(true)}), "observed member on the discovered row")

	declared := model("openai/gpt-5", "openai")
	newID := declared.Status.ID
	v, err := s.Objects().Put(ctx, declared, 0)
	noErr(t, err, "declared over discovered")
	equal(t, v, 2, "the version advances")
	equal(t, declared.Status.ID, discovered.Status.ID, "the id is kept and written back")
	truth(t, declared.Status.ID != newID, "the caller's fresh id is not used")

	got, gv, err := s.Objects().Get(ctx, v1.KindModel, discovered.Status.ID)
	noErr(t, err, "Get the kept id")
	m := as[*v1.Model](t, got)
	equal(t, gv, 2, "version")
	equal(t, m.Status.Source, v1.SourceDeclared, "source")
	equal(t, m.Status.Owner, subject, "owner is the actor's")
	truth(t, m.Status.Available != nil && *m.Status.Available, "the observed half survives the replacement")
	_, _, err = s.Objects().Get(ctx, v1.KindModel, newID)
	wantErr(t, err, store.ErrNotFound, "the fresh id names nothing")

	// The other way round, and discovered over discovered, are ErrNameTaken.
	again := model("openai/gpt-5", "openai")
	again.Status.Source = v1.SourceDiscovered
	_, err = s.Objects().Put(ctx, again, 0)
	wantErr(t, err, store.ErrNameTaken, "discovered over declared")
	other := model("openai/o3", "openai")
	other.Status.Source = v1.SourceDiscovered
	_, err = s.Objects().Put(ctx, other, 0)
	noErr(t, err, "another discovered Model")
	twice := model("openai/o3", "openai")
	twice.Status.Source = v1.SourceDiscovered
	_, err = s.Objects().Put(ctx, twice, 0)
	wantErr(t, err, store.ErrNameTaken, "discovered over discovered")

	// A Model with no source is stored as declared.
	blank := model("blank", "openai")
	blank.Status.Source = ""
	_, err = s.Objects().Put(ctx, blank, 0)
	noErr(t, err, "no source")
	equal(t, blank.Status.Source, v1.SourceDeclared, "the default source is written back")
}

func ptr[T any](v T) *T { return &v }

// statusHalvesAreSeparate is table-driven over every member of spec
// 010's table: the observed member is not written by Put and is by
// PutStatus, the control member is not written by PutStatus, and a
// read merges both.
func statusHalvesAreSeparate(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	rows := []struct {
		name     string
		obj      v1.Object
		observed any
		check    func(t *testing.T, before, after v1.Object)
	}{
		{"Provider", func() v1.Object {
			p := provider("openai")
			p.Status.Credential = &v1.CredentialStatus{Set: true, Version: 1}
			p.Status.Health = &v1.HealthStatus{State: v1.HealthHealthy}
			p.Status.Discovered = &v1.DiscoveredStatus{Count: 9}
			p.Status.Tunnel = &v1.TunnelStatus{State: v1.TunnelConnected}
			return p
		}(), store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthUnreachable, Since: at}, Discovered: &v1.DiscoveredStatus{Count: 3, At: at}, Tunnel: &v1.TunnelStatus{State: v1.TunnelDisconnected}}, func(t *testing.T, before, after v1.Object) {
			b, a := as[*v1.Provider](t, before), as[*v1.Provider](t, after)
			truth(t, b.Status.Health == nil && b.Status.Discovered == nil && b.Status.Tunnel == nil, "Put wrote no observed Provider member")
			truth(t, b.Status.Credential != nil && b.Status.Credential.Version == 1, "Put wrote credential")
			truth(t, a.Status.Health != nil && a.Status.Health.State == v1.HealthUnreachable && a.Status.Health.Since.Equal(at), "PutStatus wrote health")
			truth(t, a.Status.Discovered != nil && a.Status.Discovered.Count == 3, "PutStatus wrote discovered")
			truth(t, a.Status.Tunnel != nil && a.Status.Tunnel.State == v1.TunnelDisconnected, "PutStatus wrote tunnel")
			truth(t, a.Status.Credential != nil && a.Status.Credential.Version == 1, "PutStatus left credential")
		}},
		{"Model", func() v1.Object {
			m := model("gpt-5", "openai")
			m.Status.Available = new(true)
			m.Status.Targets = []v1.TargetStatus{{Provider: "openai", Model: "gpt-5", Health: v1.HealthHealthy}}
			return m
		}(), store.ModelObserved{Available: new(false), Targets: []v1.TargetStatus{{Provider: "openai", Model: "gpt-5", Health: v1.HealthDegraded}}}, func(t *testing.T, before, after v1.Object) {
			b, a := as[*v1.Model](t, before), as[*v1.Model](t, after)
			truth(t, b.Status.Available == nil && b.Status.Targets == nil, "Put wrote no observed Model member")
			equal(t, b.Status.Source, v1.SourceDeclared, "Put wrote source")
			truth(t, a.Status.Available != nil && !*a.Status.Available, "PutStatus wrote available")
			truth(t, len(a.Status.Targets) == 1 && a.Status.Targets[0].Health == v1.HealthDegraded, "PutStatus wrote targets")
			equal(t, a.Status.Source, v1.SourceDeclared, "PutStatus left source")
		}},
		{"Key", func() v1.Object {
			k := key("run-42")
			k.Status.ExpiresAt = at
			k.Status.Budget = &v1.BudgetRef{Name: "team"}
			k.Status.State = v1.KeyDisabled
			k.Status.Usage = &v1.KeyUsage{Total: v1.UsageTotal{Requests: 5}}
			k.Status.LastUsedAt = at
			return k
		}(), store.KeyObserved{State: v1.KeyExhausted, Usage: &v1.KeyUsage{Window: &v1.UsageWindow{Requests: 2}, Total: v1.UsageTotal{Requests: 7}}, LastUsedAt: at}, func(t *testing.T, before, after v1.Object) {
			b, a := as[*v1.Key](t, before), as[*v1.Key](t, after)
			truth(t, b.Status.State == "" && b.Status.Usage == nil && b.Status.LastUsedAt.IsZero(), "Put wrote no observed Key member")
			truth(t, b.Status.Prefix == "lux_ab12cd34" && b.Status.ExpiresAt.Equal(at) && b.Status.Budget != nil && len(b.Status.Selectors) == 1, "Put wrote prefix, expiresAt, budget, selectors")
			equal(t, a.Status.State, v1.KeyExhausted, "PutStatus wrote state")
			truth(t, a.Status.Usage != nil && a.Status.Usage.Total.Requests == 7 && a.Status.Usage.Window != nil && a.Status.Usage.Window.Requests == 2, "PutStatus wrote usage")
			truth(t, a.Status.LastUsedAt.Equal(at), "PutStatus wrote lastUsedAt")
			truth(t, a.Status.Prefix == "lux_ab12cd34" && a.Status.Budget != nil, "PutStatus left the control members")
		}},
		{"Budget", func() v1.Object {
			b := budget("team")
			b.Status.State = v1.BudgetOpen
			b.Status.Spent = ptr(v1.Money(1))
			b.Status.Remaining = ptr(v1.Money(2))
			b.Status.ResetsAt = at
			b.Status.Keys = new(4)
			return b
		}(), store.BudgetObserved{State: v1.BudgetExhausted, Spent: ptr(v1.Money(10)), Remaining: ptr(v1.Money(0)), ResetsAt: at, Keys: new(2)}, func(t *testing.T, before, after v1.Object) {
			b, a := as[*v1.Budget](t, before), as[*v1.Budget](t, after)
			truth(t, b.Status.State == "" && b.Status.Spent == nil && b.Status.Remaining == nil && b.Status.ResetsAt.IsZero() && b.Status.Keys == nil, "Put wrote no observed Budget member")
			equal(t, a.Status.State, v1.BudgetExhausted, "PutStatus wrote state")
			truth(t, a.Status.Spent != nil && *a.Status.Spent == 10 && a.Status.Remaining != nil && *a.Status.Remaining == 0, "PutStatus wrote spent and remaining")
			truth(t, a.Status.ResetsAt.Equal(at) && a.Status.Keys != nil && *a.Status.Keys == 2, "PutStatus wrote resetsAt and keys")
			equal(t, a.Status.Owner, subject, "PutStatus left owner")
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			kind, id := row.obj.Kind(), row.obj.ID()
			_, err := s.Objects().Put(ctx, row.obj, 0)
			noErr(t, err, "Put")
			before, _, err := s.Objects().Get(ctx, kind, id)
			noErr(t, err, "Get before PutStatus")
			noErr(t, s.Objects().PutStatus(ctx, kind, id, row.observed), "PutStatus")
			// A second Put changes no observed member, and the write-back
			// leaves them as the read shows them.
			_, err = s.Objects().Put(ctx, row.obj, 1)
			noErr(t, err, "Put again")
			after, v, err := s.Objects().Get(ctx, kind, id)
			noErr(t, err, "Get after PutStatus")
			equal(t, v, 2, "version after two Puts")
			equal(t, after.Owner(), subject, "owner")
			row.check(t, before, after)
			// The same struct by pointer, with zero members, changes nothing.
			noErr(t, s.Objects().PutStatus(ctx, kind, id, zeroObserved(kind)), "PutStatus with zero members")
			same, _, err := s.Objects().Get(ctx, kind, id)
			noErr(t, err, "Get after the zero PutStatus")
			row.check(t, before, same)
			truth(t, s.Objects().PutStatus(ctx, kind, id, struct{}{}) != nil, "PutStatus with another type is refused")
		})
	}
	// Two writers of two members on one row leave both.
	p := provider("azure")
	_, err := s.Objects().Put(ctx, p, 0)
	noErr(t, err, "Put azure")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, &store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy}}), "health job")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, &store.ProviderObserved{Discovered: &v1.DiscoveredStatus{Count: 12}}), "discovery job")
	got, _, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	noErr(t, err, "Get azure")
	ap := as[*v1.Provider](t, got)
	truth(t, ap.Status.Health != nil && ap.Status.Health.State == v1.HealthHealthy && ap.Status.Discovered != nil && ap.Status.Discovered.Count == 12, "both members are kept")
	// A cleared list, a deleted row, an unknown kind.
	m := model("o3", "openai")
	_, err = s.Objects().Put(ctx, m, 0)
	noErr(t, err, "Put o3")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Targets: []v1.TargetStatus{{Provider: "openai"}}}), "targets")
	noErr(t, s.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Targets: []v1.TargetStatus{}}), "clear targets")
	got, _, err = s.Objects().Get(ctx, v1.KindModel, m.Status.ID)
	noErr(t, err, "Get o3")
	equal(t, len(as[*v1.Model](t, got).Status.Targets), 0, "an empty list clears")
	noErr(t, s.Objects().Delete(ctx, v1.KindModel, m.Status.ID), "delete")
	wantErr(t, s.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{}), store.ErrNotFound, "PutStatus on a deleted row")
	truth(t, s.Objects().PutStatus(ctx, "Thing", m.Status.ID, nil) != nil, "PutStatus of an unknown kind")
}

// zeroObserved is the kind's struct with every member zero, by pointer.
func zeroObserved(kind string) any {
	switch kind {
	case v1.KindProvider:
		return &store.ProviderObserved{}
	case v1.KindModel:
		return &store.ModelObserved{}
	case v1.KindKey:
		return &store.KeyObserved{}
	default:
		return &store.BudgetObserved{}
	}
}

func transactIsAtomic(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("run-42")
	p := provider("openai")
	sealed := store.Sealed{Version: 1, WrappedKey: []byte("wk"), WrappedNonce: []byte("wn"), Ciphertext: []byte("ct"), Nonce: []byte("n")}
	boom := errors.New("the apply failed after the object was written")
	err := s.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, k, 0); err != nil {
			return err
		}
		if err := tx.Keys().Put(ctx, k.Status.ID, hash(1)); err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: k.Status.ID, Type: "key.created"}); err != nil {
			return err
		}
		if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
			return err
		}
		if err := tx.Credentials().Put(ctx, p.Status.ID, sealed); err != nil {
			return err
		}
		return boom
	})
	wantErr(t, err, boom, "Transact returns fn's error")
	_, _, err = s.Objects().Get(ctx, v1.KindKey, k.Status.ID)
	wantErr(t, err, store.ErrNotFound, "the object after a failed Transact")
	_, err = s.Keys().ByHash(ctx, hash(1))
	wantErr(t, err, store.ErrNotFound, "the hash after a failed Transact")
	pending, err := s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending")
	equal(t, len(pending), 0, "journal rows after a failed Transact")
	_, err = s.Credentials().Get(ctx, p.Status.ID)
	wantErr(t, err, store.ErrNotFound, "the credential after a failed Transact")

	// The id the failed Transact used is free again, so the retry the
	// API makes with the same resolved object is a create.
	k2 := key("run-42")
	k2.Status.ID = k.Status.ID
	err = s.Transact(ctx, func(tx store.Store) error {
		if _, err := tx.Objects().Put(ctx, k2, 0); err != nil {
			return err
		}
		if err := tx.Keys().Put(ctx, k2.Status.ID, hash(1)); err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: k2.Status.ID, Type: "key.created"}); err != nil {
			return err
		}
		if _, err := tx.Objects().Put(ctx, p, 0); err != nil {
			return err
		}
		return tx.Credentials().Put(ctx, p.Status.ID, sealed)
	})
	noErr(t, err, "the Transact that commits")
	_, v, err := s.Objects().Get(ctx, v1.KindKey, k2.Status.ID)
	noErr(t, err, "the object after the commit")
	equal(t, v, 1, "the object's version")
	id, err := s.Keys().ByHash(ctx, hash(1))
	noErr(t, err, "the hash after the commit")
	equal(t, id, k2.Status.ID, "the hash names the Key")
	pending, err = s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending after the commit")
	equal(t, len(pending), 1, "one journal row")
	got, err := s.Credentials().Get(ctx, p.Status.ID)
	noErr(t, err, "the credential after the commit")
	sameBytes(t, got.Ciphertext, sealed.Ciphertext, "ciphertext")

	// A context that has ended runs nothing.
	ended, cancel := context.WithCancel(ctx)
	cancel()
	ran := false
	err = s.Transact(ended, func(store.Store) error { ran = true; return nil })
	wantErr(t, err, context.Canceled, "Transact on an ended context")
	truth(t, !ran, "fn did not run")
}

func transactRefusesNesting(t *testing.T, s store.Store) {
	ctx := t.Context()
	var inner error
	err := s.Transact(ctx, func(tx store.Store) error {
		inner = tx.Transact(ctx, func(store.Store) error { return nil })
		return inner
	})
	wantErr(t, inner, store.ErrNested, "the nested Transact")
	wantErr(t, err, store.ErrNested, "the outer Transact carries it")
}

func listOrderAndCursor(t *testing.T, s store.Store) {
	ctx := t.Context()
	other := "https://login.example.com|bob"
	for _, spec := range []struct {
		name, owner string
		labels      map[string]string
		source      v1.Source
	}{
		{"c", subject, map[string]string{"tier": "gold", "team": "research"}, v1.SourceDeclared},
		{"a", subject, map[string]string{"tier": "gold"}, v1.SourceDeclared},
		{"e", other, nil, v1.SourceDiscovered},
		{"b", subject, map[string]string{"tier": "silver"}, v1.SourceDeclared},
		{"d", other, map[string]string{"tier": "gold"}, v1.SourceDiscovered},
	} {
		m := model(spec.name, "openai")
		m.Metadata.Labels = spec.labels
		m.Status.Owner, m.Status.Source = spec.owner, spec.source
		_, err := s.Objects().Put(ctx, m, 0)
		noErr(t, err, "Put "+spec.name)
	}
	// A Key with a Model's name is another kind and never listed here.
	_, err := s.Objects().Put(ctx, key("a"), 0)
	noErr(t, err, "Put Key a")

	all, next, err := s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	noErr(t, err, "List all")
	sameNames(t, all, "a", "b", "c", "d", "e")
	equal(t, next, "", "no cursor when every row was returned")

	page1, next, err := s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{Limit: 2})
	noErr(t, err, "page 1")
	sameNames(t, page1, "a", "b")
	truth(t, next != "", "a cursor while rows remain")
	page2, next2, err := s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{Limit: 2, Cursor: next})
	noErr(t, err, "page 2")
	sameNames(t, page2, "c", "d")
	page3, next3, err := s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{Limit: 2, Cursor: next2})
	noErr(t, err, "page 3")
	sameNames(t, page3, "e")
	equal(t, next3, "", "the last page has no cursor")
	exact, nextExact, err := s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{Limit: 1, Cursor: next2})
	noErr(t, err, "a page that fills its limit with the last row")
	sameNames(t, exact, "e")
	equal(t, nextExact, "", "no cursor when the limit lands on the last row")

	_, _, err = s.Objects().List(ctx, v1.KindKey, store.Filter{}, store.Page{Cursor: next})
	wantErr(t, err, store.ErrInvalidCursor, "a cursor from another kind")
	_, _, err = s.Objects().List(ctx, v1.KindModel, store.Filter{Owner: subject}, store.Page{Cursor: next})
	wantErr(t, err, store.ErrInvalidCursor, "a cursor from another filter")
	_, _, err = s.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{Cursor: "not-a-cursor"})
	wantErr(t, err, store.ErrInvalidCursor, "a string that is not a cursor")

	// The filters, each alone and then together, paging under one.
	byOwner, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Owner: other}, store.Page{})
	noErr(t, err, "owner")
	sameNames(t, byOwner, "d", "e")
	byLabel, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Labels: map[string]string{"tier": "gold"}}, store.Page{})
	noErr(t, err, "one label")
	sameNames(t, byLabel, "a", "c", "d")
	byLabels, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Labels: map[string]string{"tier": "gold", "team": "research"}}, store.Page{})
	noErr(t, err, "two labels")
	sameNames(t, byLabels, "c")
	bySource, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Source: string(v1.SourceDiscovered)}, store.Page{})
	noErr(t, err, "source")
	sameNames(t, bySource, "d", "e")
	byIDs, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{IDs: []string{all[1].ID(), all[3].ID(), "mdl_none"}}, store.Page{})
	noErr(t, err, "ids")
	sameNames(t, byIDs, "b", "d")
	f := store.Filter{Owner: subject, Labels: map[string]string{"tier": "gold"}}
	p1, n1, err := s.Objects().List(ctx, v1.KindModel, f, store.Page{Limit: 1})
	noErr(t, err, "filtered page 1")
	sameNames(t, p1, "a")
	p2, n2, err := s.Objects().List(ctx, v1.KindModel, f, store.Page{Limit: 1, Cursor: n1})
	noErr(t, err, "filtered page 2")
	sameNames(t, p2, "c")
	equal(t, n2, "", "filtered list ends")
	none, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Source: string(v1.SourceDiscovered), Owner: subject}, store.Page{})
	noErr(t, err, "an empty answer")
	sameNames(t, none)
	keys, _, err := s.Objects().List(ctx, v1.KindKey, store.Filter{Source: string(v1.SourceDeclared)}, store.Page{})
	noErr(t, err, "source on a kind without one")
	sameNames(t, keys)
}

func filterByProvider(t *testing.T, s store.Store) {
	ctx := t.Context()
	openai, azure := provider("openai"), provider("azure")
	for _, p := range []*v1.Provider{openai, azure} {
		_, err := s.Objects().Put(ctx, p, 0)
		noErr(t, err, "Put "+p.Metadata.Name)
	}
	byID := "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y"
	for _, m := range []*v1.Model{
		model("gpt-5", "openai", "azure"),
		model("claude", byID),
		model("llama", "azure"),
		model("orphan", "gone"),
	} {
		_, err := s.Objects().Put(ctx, m, 0)
		noErr(t, err, "Put "+m.Metadata.Name)
	}
	for _, tc := range []struct {
		provider string
		want     []string
	}{
		{openai.Status.ID, []string{"gpt-5"}},
		{azure.Status.ID, []string{"gpt-5", "llama"}},
		{byID, []string{"claude"}},
		{"prv_unknown", nil},
	} {
		got, _, err := s.Objects().List(ctx, v1.KindModel, store.Filter{Provider: tc.provider}, store.Page{})
		noErr(t, err, "List by provider "+tc.provider)
		sameNames(t, got, tc.want...)
	}
	// A Provider filter selects Models alone.
	got, _, err := s.Objects().List(ctx, v1.KindProvider, store.Filter{Provider: openai.Status.ID}, store.Page{})
	noErr(t, err, "provider filter on Providers")
	sameNames(t, got)
	// A deleted Provider no longer resolves a target's name.
	noErr(t, s.Objects().Delete(ctx, v1.KindProvider, azure.Status.ID), "delete azure")
	got, _, err = s.Objects().List(ctx, v1.KindModel, store.Filter{Provider: azure.Status.ID}, store.Page{})
	noErr(t, err, "List by a deleted provider")
	sameNames(t, got)
}

func deleteAndPrune(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("run-42")
	_, err := s.Objects().Put(ctx, k, 0)
	noErr(t, err, "Put")
	_, err = s.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: k.Status.ID, Type: "key.created"})
	noErr(t, err, "Append")
	wantErr(t, s.Objects().Delete(ctx, v1.KindBudget, k.Status.ID), store.ErrNotFound, "Delete under another kind")
	noErr(t, s.Objects().Delete(ctx, v1.KindKey, k.Status.ID), "Delete")
	wantErr(t, s.Objects().Delete(ctx, v1.KindKey, k.Status.ID), store.ErrNotFound, "Delete twice")
	_, _, err = s.Objects().ByName(ctx, v1.KindKey, "run-42")
	wantErr(t, err, store.ErrNotFound, "ByName after Delete")
	live, _, err := s.Objects().List(ctx, v1.KindKey, store.Filter{}, store.Page{})
	noErr(t, err, "List after Delete")
	sameNames(t, live)

	future := time.Now().Add(time.Hour)
	n, err := s.Objects().Prune(ctx, future)
	noErr(t, err, "Prune with a pending row")
	equal(t, n, 0, "a row with a pending event stays")
	rows, _, err := s.Journal().ByObject(ctx, k.Status.ID, store.Page{})
	noErr(t, err, "ByObject after Delete")
	equal(t, len(rows), 1, "the journal still names the deleted object")
	noErr(t, s.Journal().Acknowledge(ctx, "evt_1"), "Acknowledge")
	n, err = s.Objects().Prune(ctx, time.Now().Add(-time.Hour))
	noErr(t, err, "Prune before the deletion")
	equal(t, n, 0, "a deletion after before stays")
	n, err = s.Objects().Prune(ctx, future)
	noErr(t, err, "Prune")
	equal(t, n, 1, "the settled row goes")
	_, _, err = s.Objects().Get(ctx, v1.KindKey, k.Status.ID)
	wantErr(t, err, store.ErrNotFound, "Get after Prune")

	n, err = s.Journal().Prune(ctx, future)
	noErr(t, err, "Journal.Prune")
	equal(t, n, 1, "the acknowledged row goes")
	rows, _, err = s.Journal().ByObject(ctx, k.Status.ID, store.Page{})
	noErr(t, err, "ByObject after Journal.Prune")
	equal(t, len(rows), 0, "no rows remain")
}

func putKeepsNoValue(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("run-42")
	k.Spec.SetValue("lux_supplied_value_that_must_never_be_a_row_0000000")
	k.Status.Value = "lux_minted_value_that_is_the_response_alone_000000"
	_, err := s.Objects().Put(ctx, k, 0)
	noErr(t, err, "Put Key")
	got, _, err := s.Objects().Get(ctx, v1.KindKey, k.Status.ID)
	noErr(t, err, "Get Key")
	_, set := as[*v1.Key](t, got).Spec.Value()
	truth(t, !set, "the supplied value is not stored")
	equal(t, as[*v1.Key](t, got).Status.Value, "", "status.value is not stored")

	p := provider("openai")
	p.Spec.Credential = &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer}
	p.Spec.Credential.SetValue("sk-live-credential-that-must-never-be-a-row")
	_, err = s.Objects().Put(ctx, p, 0)
	noErr(t, err, "Put Provider")
	got, _, err = s.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	noErr(t, err, "Get Provider")
	_, set = as[*v1.Provider](t, got).Spec.Credential.Value()
	truth(t, !set, "the credential value is not stored")
	equal(t, as[*v1.Provider](t, got).Spec.Credential.Header, "Authorization", "the credential's other members are")

	// What a read returns is the caller's: changing it changes no row.
	as[*v1.Provider](t, got).Spec.BaseURL = "https://changed.example.com"
	again, _, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	noErr(t, err, "Get again")
	equal(t, as[*v1.Provider](t, again).Spec.BaseURL, "https://api.example.com/v1", "a returned object is a copy")
}

func countersAreAtomic(t *testing.T, s store.Store) {
	ctx := t.Context()
	const callers = 1000
	var wg sync.WaitGroup
	totals := make(chan int64, callers)
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			total, err := s.Counters().Add(ctx, "key:key_1:requests:0", 1, time.Time{})
			totals <- total
			errs <- err
		})
	}
	wg.Wait()
	close(totals)
	close(errs)
	for err := range errs {
		noErr(t, err, "Add")
	}
	seen := map[int64]bool{}
	for total := range totals {
		seen[total] = true
	}
	equal(t, len(seen), callers, "every Add saw a distinct total")
	read, err := s.Counters().Read(ctx, []string{"key:key_1:requests:0", "key:key_1:tokens:0"})
	noErr(t, err, "Read")
	equal(t, read["key:key_1:requests:0"], int64(callers), "the total")
	_, present := read["key:key_1:tokens:0"]
	truth(t, !present, "a key with no row is left out")
	total, err := s.Counters().Add(ctx, "key:key_1:requests:0", -1000, time.Time{})
	noErr(t, err, "Add a negative delta")
	equal(t, total, 0, "the running total")
	_, err = s.Counters().Add(ctx, "", 1, time.Time{})
	truth(t, err != nil, "Add with no key is refused")
}

func noneWindowIsNeverPruned(t *testing.T, s store.Store) {
	ctx := t.Context()
	past := time.Now().Add(-time.Minute)
	_, err := s.Counters().Add(ctx, "key:k:spend:none", 5, time.Time{})
	noErr(t, err, "Add none")
	_, err = s.Counters().Add(ctx, "key:k:spend:window", 7, past)
	noErr(t, err, "Add window")
	// A later Add does not move the window's end.
	_, err = s.Counters().Add(ctx, "key:k:spend:window", 1, time.Now().Add(time.Hour))
	noErr(t, err, "Add window again")
	n, err := s.Counters().Prune(ctx, past.Add(-time.Second))
	noErr(t, err, "Prune before the window")
	equal(t, n, 0, "nothing lapsed yet")
	n, err = s.Counters().Prune(ctx, time.Now())
	noErr(t, err, "Prune")
	equal(t, n, 1, "the lapsed window goes")
	read, err := s.Counters().Read(ctx, []string{"key:k:spend:none", "key:k:spend:window"})
	noErr(t, err, "Read")
	equal(t, read["key:k:spend:none"], 5, "the none window stays")
	_, present := read["key:k:spend:window"]
	truth(t, !present, "the pruned window is gone")
	n, err = s.Counters().Prune(ctx, time.Now().Add(24*365*time.Hour))
	noErr(t, err, "Prune far ahead")
	equal(t, n, 0, "a zero expiresAt is never pruned")
}

func leases(t *testing.T, s store.Store) {
	ctx := t.Context()
	held, err := s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-a", lapse)
	noErr(t, err, "Acquire a")
	truth(t, held, "a acquires a free lease")
	held, err = s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-b", lapse)
	noErr(t, err, "Acquire b")
	truth(t, !held, "b does not acquire a held lease")
	held, err = s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-a", lapse)
	noErr(t, err, "Acquire a again")
	truth(t, held, "the holder renews")
	held, err = s.Leases().Acquire(ctx, store.LeaseHealth, "replica-b", lapse)
	noErr(t, err, "Acquire another lease")
	truth(t, held, "another name is another lease")
	noErr(t, s.Leases().Release(ctx, store.LeaseDiscovery, "replica-b"), "Release by a non-holder")
	held, err = s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-b", lapse)
	noErr(t, err, "Acquire b after a non-holder's Release")
	truth(t, !held, "a non-holder's Release changed nothing")

	time.Sleep(2 * lapse)
	held, err = s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-b", lapse)
	noErr(t, err, "Acquire b after the lapse")
	truth(t, held, "b acquires once the row lapsed")
	held, err = s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-a", lapse)
	noErr(t, err, "Acquire a after losing it")
	truth(t, !held, "a lost the lease")
	noErr(t, s.Leases().Release(ctx, store.LeaseDiscovery, "replica-b"), "Release by the holder")
	held, err = s.Leases().Acquire(ctx, store.LeaseDiscovery, "replica-a", lapse)
	noErr(t, err, "Acquire a after the Release")
	truth(t, held, "a released lease is free at once")
	noErr(t, s.Leases().Release(ctx, "never-held", "replica-a"), "Release of a lease that was never held")
	_, err = s.Leases().Acquire(ctx, "", "replica-a", lapse)
	truth(t, err != nil, "Acquire with no name is refused")
	_, err = s.Leases().Acquire(ctx, store.LeaseUsage, "replica-a", 0)
	truth(t, err != nil, "Acquire with no ttl is refused")
}

func journalTail(t *testing.T, s store.Store) {
	ctx := t.Context()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	for i, e := range []store.Event{
		{ID: "evt_1", ObjectID: "key_a", Type: "key.created", Payload: []byte(`{"n":1}`)},
		{ID: "evt_2", ObjectID: "key_b", Type: "key.created", At: at},
		{ID: "evt_3", ObjectID: "key_a", Type: "key.updated"},
		{ID: "evt_4", ObjectID: "prv_c", Type: "provider.created"},
		{ID: "evt_5", ObjectID: "key_a", Type: "key.deleted"},
	} {
		seq, err := s.Journal().Append(ctx, e)
		noErr(t, err, "Append "+e.ID)
		want := map[int]int64{0: 1, 1: 1, 2: 2, 3: 1, 4: 3}[i]
		equal(t, seq, want, "Seq of "+e.ID)
	}
	all, err := s.Journal().Since(ctx, 0, 0)
	noErr(t, err, "Since 0")
	equal(t, len(all), 5, "every row")
	for i, e := range all {
		equal(t, e.GSeq, int64(i+1), "GSeq is dense and ordered")
	}
	equal(t, all[0].Type, "key.created", "the first row")
	sameBytes(t, all[0].Payload, []byte(`{"n":1}`), "the payload")
	truth(t, !all[0].At.IsZero() && all[0].NextAttemptAt.Equal(all[0].At), "At is filled and NextAttemptAt follows it")
	truth(t, all[1].At.Equal(at), "a given At is kept")
	all[0].Payload[0] = '['
	again, err := s.Journal().Since(ctx, 0, 1)
	noErr(t, err, "Since with a limit")
	equal(t, len(again), 1, "the limit")
	sameBytes(t, again[0].Payload, []byte(`{"n":1}`), "a returned payload is a copy")

	// A tailing replica reads in pages and misses none.
	var tail []store.Event
	after := int64(0)
	for {
		page, err := s.Journal().Since(ctx, after, 2)
		noErr(t, err, "tail page")
		if len(page) == 0 {
			break
		}
		tail = append(tail, page...)
		after = page[len(page)-1].GSeq
	}
	equal(t, len(tail), 5, "the tail saw every row")
	equal(t, tail[4].ID, "evt_5", "in order")

	_, err = s.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: "key_z"})
	truth(t, err != nil && !isNamed(err), "a duplicate id is a plain error")
	_, err = s.Journal().Append(ctx, store.Event{ObjectID: "key_z"})
	truth(t, err != nil && !isNamed(err), "no id is a plain error")
	_, err = s.Journal().Append(ctx, store.Event{ID: "evt_9"})
	truth(t, err != nil && !isNamed(err), "no object id is a plain error")
}

func pendingIsOnePerObject(t *testing.T, s store.Store) {
	ctx := t.Context()
	for _, e := range []store.Event{
		{ID: "evt_a1", ObjectID: "key_a", Type: "key.created"},
		{ID: "evt_a2", ObjectID: "key_a", Type: "key.updated"},
		{ID: "evt_b1", ObjectID: "key_b", Type: "key.created"},
	} {
		_, err := s.Journal().Append(ctx, e)
		noErr(t, err, "Append "+e.ID)
	}
	ids := func(events []store.Event) []string {
		out := make([]string, 0, len(events))
		for _, e := range events {
			out = append(out, e.ID)
		}
		return out
	}
	pending, err := s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending")
	equal(t, len(pending), 2, "one per object")
	equal(t, pending[0].ID+" "+pending[1].ID, "evt_a1 evt_b1", "the oldest of each, oldest first")
	one, err := s.Journal().Pending(ctx, 1)
	noErr(t, err, "Pending 1")
	equal(t, len(one), 1, "the limit")

	noErr(t, s.Journal().Acknowledge(ctx, "evt_a1"), "Acknowledge a1")
	pending, err = s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending after the acknowledgement")
	equal(t, ids(pending)[0]+" "+ids(pending)[1], "evt_a2 evt_b1", "the next row of a follows")

	noErr(t, s.Journal().Defer(ctx, "evt_b1", 3, time.Now().Add(time.Hour)), "Defer b1")
	pending, err = s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending after the deferral")
	equal(t, len(pending), 1, "a deferred row is not due")
	equal(t, pending[0].ID, "evt_a2", "a2 alone")
	rows, _, err := s.Journal().ByObject(ctx, "key_b", store.Page{})
	noErr(t, err, "ByObject b")
	truth(t, rows[0].Attempts == 3 && rows[0].NextAttemptAt.After(time.Now()), "Defer wrote attempts and next")
	_, err = s.Journal().Append(ctx, store.Event{ID: "evt_b2", ObjectID: "key_b", Type: "key.updated"})
	noErr(t, err, "Append b2")
	pending, err = s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending with b held back")
	equal(t, len(pending), 1, "b's deferred row holds b2 back")

	noErr(t, s.Journal().Drop(ctx, "evt_a2"), "Drop a2")
	noErr(t, s.Journal().Defer(ctx, "evt_b1", 4, time.Now().Add(-time.Second)), "Defer b1 to the past")
	pending, err = s.Journal().Pending(ctx, 0)
	noErr(t, err, "Pending after Drop")
	equal(t, ids(pending)[0], "evt_b1", "b1 is due again and a2 is gone")
	rows, _, err = s.Journal().ByObject(ctx, "key_a", store.Page{})
	noErr(t, err, "ByObject a")
	equal(t, len(rows), 1, "a dropped row is removed")
	truth(t, !rows[0].AckedAt.IsZero(), "the acknowledged row carries AckedAt")

	wantErr(t, s.Journal().Acknowledge(ctx, "evt_none"), store.ErrNotFound, "Acknowledge an unknown id")
	wantErr(t, s.Journal().Defer(ctx, "evt_none", 1, time.Now()), store.ErrNotFound, "Defer an unknown id")
	wantErr(t, s.Journal().Drop(ctx, "evt_none"), store.ErrNotFound, "Drop an unknown id")
}

func journalByObjectPages(t *testing.T, s store.Store) {
	ctx := t.Context()
	for i := range 5 {
		_, err := s.Journal().Append(ctx, store.Event{ID: "evt_" + string(rune('a'+i)), ObjectID: "key_a", Type: "key.updated"})
		noErr(t, err, "Append")
	}
	_, err := s.Journal().Append(ctx, store.Event{ID: "evt_other", ObjectID: "key_b", Type: "key.created"})
	noErr(t, err, "Append another object")
	var seqs []int64
	cursor := ""
	for range 10 {
		page, next, err := s.Journal().ByObject(ctx, "key_a", store.Page{Limit: 2, Cursor: cursor})
		noErr(t, err, "ByObject page")
		for _, e := range page {
			seqs = append(seqs, e.Seq)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	equal(t, len(seqs), 5, "every row of the object, no other's")
	for i, seq := range seqs {
		equal(t, seq, int64(i+1), "Seq ascending")
	}
	_, _, err = s.Journal().ByObject(ctx, "key_b", store.Page{Cursor: cursor})
	wantErr(t, err, store.ErrInvalidCursor, "a cursor from another object")
	_, _, err = s.Journal().ByObject(ctx, "key_a", store.Page{Cursor: store.EncodeCursor(store.JournalKind, store.Filter{IDs: []string{"key_a"}}, "x")})
	wantErr(t, err, store.ErrInvalidCursor, "a cursor whose last is not a sequence")
}

func keyHashReplacesOnRotate(t *testing.T, s store.Store) {
	ctx := t.Context()
	noErr(t, s.Keys().Put(ctx, "key_1", hash(1)), "Put")
	id, err := s.Keys().ByHash(ctx, hash(1))
	noErr(t, err, "ByHash")
	equal(t, id, "key_1", "the key id")
	noErr(t, s.Keys().Put(ctx, "key_1", hash(1)), "Put the same hash again")
	noErr(t, s.Keys().Put(ctx, "key_1", hash(2)), "rotate")
	_, err = s.Keys().ByHash(ctx, hash(1))
	wantErr(t, err, store.ErrNotFound, "the old hash at once")
	id, err = s.Keys().ByHash(ctx, hash(2))
	noErr(t, err, "the new hash")
	equal(t, id, "key_1", "the new hash names the key")
	wantErr(t, s.Keys().Put(ctx, "key_2", hash(2)), store.ErrHashTaken, "a hash registered to another key")
	_, err = s.Keys().ByHash(ctx, hash(3))
	wantErr(t, err, store.ErrNotFound, "an unknown hash")
	noErr(t, s.Keys().Delete(ctx, "key_1"), "Delete")
	_, err = s.Keys().ByHash(ctx, hash(2))
	wantErr(t, err, store.ErrNotFound, "the hash after Delete")
	wantErr(t, s.Keys().Delete(ctx, "key_1"), store.ErrNotFound, "Delete twice")
	noErr(t, s.Keys().Put(ctx, "key_2", hash(2)), "the freed hash on another key")
	for _, bad := range []string{"", "abc", hash(1)[:63] + "G", hash(1)[:63] + "A"} {
		err := s.Keys().Put(ctx, "key_3", bad)
		truth(t, err != nil && !isNamed(err), "a hash of another shape is a plain error: "+bad)
	}
	truth(t, s.Keys().Put(ctx, "", hash(4)) != nil, "Put with no key id is refused")
}

func rewrapTouchesTheWrapColumnsOnly(t *testing.T, s store.Store) {
	ctx := t.Context()
	sealed := store.Sealed{Version: 2, WrappedKey: []byte("wrapped-key-48"), WrappedNonce: []byte("wrap-nonce"), Ciphertext: []byte("ciphertext-plus-tag"), Nonce: []byte("value-nonce")}
	noErr(t, s.Credentials().Put(ctx, "prv_1", sealed), "Put")
	noErr(t, s.Credentials().Put(ctx, "prv_0", store.Sealed{Version: 1}), "Put another")
	ids, err := s.Credentials().List(ctx)
	noErr(t, err, "List")
	truth(t, len(ids) == 2 && ids[0] == "prv_0" && ids[1] == "prv_1", "List is ascending")

	wantErr(t, s.Credentials().Rewrap(ctx, "prv_1", 1, []byte("new-key"), []byte("new-nonce")), store.ErrVersionConflict, "Rewrap at a stale version")
	wantErr(t, s.Credentials().Rewrap(ctx, "prv_9", 1, []byte("new-key"), []byte("new-nonce")), store.ErrNotFound, "Rewrap of an unknown row")
	noErr(t, s.Credentials().Rewrap(ctx, "prv_1", 2, []byte("new-key"), []byte("new-nonce")), "Rewrap")
	got, err := s.Credentials().Get(ctx, "prv_1")
	noErr(t, err, "Get")
	sameBytes(t, got.WrappedKey, []byte("new-key"), "wrapped key")
	sameBytes(t, got.WrappedNonce, []byte("new-nonce"), "wrapped nonce")
	sameBytes(t, got.Ciphertext, sealed.Ciphertext, "ciphertext untouched")
	sameBytes(t, got.Nonce, sealed.Nonce, "nonce untouched")
	equal(t, got.Version, 2, "version untouched")

	// A new value replaces every column.
	noErr(t, s.Credentials().Put(ctx, "prv_1", store.Sealed{Version: 3, WrappedKey: []byte("k3"), WrappedNonce: []byte("n3"), Ciphertext: []byte("c3"), Nonce: []byte("v3")}), "Put a new value")
	got, err = s.Credentials().Get(ctx, "prv_1")
	noErr(t, err, "Get the new value")
	truth(t, got.Version == 3 && string(got.Ciphertext) == "c3" && string(got.WrappedKey) == "k3", "every column replaced")

	noErr(t, s.Credentials().Delete(ctx, "prv_1"), "Delete")
	_, err = s.Credentials().Get(ctx, "prv_1")
	wantErr(t, err, store.ErrNotFound, "Get after Delete")
	wantErr(t, s.Credentials().Delete(ctx, "prv_1"), store.ErrNotFound, "Delete twice")
	truth(t, s.Credentials().Put(ctx, "", sealed) != nil, "Put with no provider id is refused")
}

// storeCannotDecrypt is the half of that criterion a store proves: what
// goes in as ciphertext comes out as the same ciphertext, unshared, and
// no other bytes.
func storeCannotDecrypt(t *testing.T, s store.Store) {
	ctx := t.Context()
	in := store.Sealed{Version: 1, WrappedKey: []byte("wk"), WrappedNonce: []byte("wn"), Ciphertext: []byte("ct"), Nonce: []byte("n")}
	noErr(t, s.Credentials().Put(ctx, "prv_1", in), "Put")
	in.Ciphertext[0] = 'X'
	out, err := s.Credentials().Get(ctx, "prv_1")
	noErr(t, err, "Get")
	sameBytes(t, out.Ciphertext, []byte("ct"), "the store holds its own copy")
	out.WrappedKey[0] = 'Y'
	again, err := s.Credentials().Get(ctx, "prv_1")
	noErr(t, err, "Get again")
	sameBytes(t, again.WrappedKey, []byte("wk"), "a returned row is a copy")
}

func tunnelRegistry(t *testing.T, s store.Store) {
	ctx := t.Context()
	first := store.Tunnel{ProviderID: "prv_1", Session: "tun_1", Replica: "10.0.0.1:8081", Subject: subject, Agent: "lux/1.0"}
	noErr(t, s.Tunnels().Register(ctx, first, time.Hour), "Register")
	got, err := s.Tunnels().Get(ctx, "prv_1")
	noErr(t, err, "Get")
	truth(t, got.Session == "tun_1" && got.Replica == first.Replica && got.Subject == subject && got.Agent == "lux/1.0", "the row")
	truth(t, !got.ConnectedAt.IsZero() && got.ExpiresAt.After(time.Now().Add(30*time.Minute)), "ConnectedAt is filled and ExpiresAt is now plus the ttl")
	held, err := s.Tunnels().Heartbeat(ctx, "prv_1", "tun_1", time.Hour)
	noErr(t, err, "Heartbeat")
	truth(t, held, "the session holds the row")
	held, err = s.Tunnels().Heartbeat(ctx, "prv_1", "tun_0", time.Hour)
	noErr(t, err, "Heartbeat by another session")
	truth(t, !held, "another session does not hold it")
	held, err = s.Tunnels().Heartbeat(ctx, "prv_9", "tun_1", time.Hour)
	noErr(t, err, "Heartbeat on no row")
	truth(t, !held, "no row is not held")

	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	second := store.Tunnel{ProviderID: "prv_1", Session: "tun_2", Replica: "10.0.0.2:8081", Subject: subject, ConnectedAt: at}
	noErr(t, s.Tunnels().Register(ctx, second, time.Hour), "Register a new session")
	got, err = s.Tunnels().Get(ctx, "prv_1")
	noErr(t, err, "Get the new session")
	truth(t, got.Session == "tun_2" && got.ConnectedAt.Equal(at), "the newest session wins and keeps its ConnectedAt")
	held, err = s.Tunnels().Heartbeat(ctx, "prv_1", "tun_1", time.Hour)
	noErr(t, err, "Heartbeat by the superseded session")
	truth(t, !held, "the superseded session learns it was replaced")
	noErr(t, s.Tunnels().Unregister(ctx, "prv_1", "tun_1"), "Unregister by the superseded session")
	_, err = s.Tunnels().Get(ctx, "prv_1")
	noErr(t, err, "the live session survives the old one's Unregister")
	noErr(t, s.Tunnels().Unregister(ctx, "prv_1", "tun_2"), "Unregister")
	_, err = s.Tunnels().Get(ctx, "prv_1")
	wantErr(t, err, store.ErrNotFound, "Get after Unregister")
	noErr(t, s.Tunnels().Unregister(ctx, "prv_1", "tun_2"), "Unregister twice")

	noErr(t, s.Tunnels().Register(ctx, store.Tunnel{ProviderID: "prv_2", Session: "tun_3"}, lapse), "Register with a short ttl")
	time.Sleep(2 * lapse)
	_, err = s.Tunnels().Get(ctx, "prv_2")
	wantErr(t, err, store.ErrNotFound, "a lapsed row is not live")
	truth(t, s.Tunnels().Register(ctx, store.Tunnel{ProviderID: "prv_3"}, time.Hour) != nil, "Register with no session is refused")
	truth(t, s.Tunnels().Register(ctx, store.Tunnel{ProviderID: "prv_3", Session: "tun_4"}, 0) != nil, "Register with no ttl is refused")
	_, err = s.Tunnels().Heartbeat(ctx, "prv_3", "tun_4", 0)
	truth(t, err != nil, "Heartbeat with no ttl is refused")
}

func readyAndClose(t *testing.T, s store.Store) {
	ctx := t.Context()
	noErr(t, s.Ready(ctx), "Ready")
	noErr(t, s.Close(), "Close")
	truth(t, s.Ready(ctx) != nil, "Ready after Close")
}

// endedContextIsAnswered holds every method to answering an ended
// context with its error rather than touching a row.
func endedContextIsAnswered(t *testing.T, s store.Store) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := provider("openai")
	calls := map[string]func() error{
		"Objects.Put":    func() error { _, err := s.Objects().Put(ctx, p, 0); return err },
		"Objects.Get":    func() error { _, _, err := s.Objects().Get(ctx, v1.KindProvider, p.Status.ID); return err },
		"Objects.ByName": func() error { _, _, err := s.Objects().ByName(ctx, v1.KindProvider, "openai"); return err },
		"Objects.List": func() error {
			_, _, err := s.Objects().List(ctx, v1.KindProvider, store.Filter{}, store.Page{})
			return err
		},
		"Objects.Delete": func() error { return s.Objects().Delete(ctx, v1.KindProvider, p.Status.ID) },
		"Objects.PutStatus": func() error {
			return s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{})
		},
		"Objects.Prune":      func() error { _, err := s.Objects().Prune(ctx, time.Now()); return err },
		"Keys.Put":           func() error { return s.Keys().Put(ctx, "key_1", hash(1)) },
		"Keys.ByHash":        func() error { _, err := s.Keys().ByHash(ctx, hash(1)); return err },
		"Keys.Delete":        func() error { return s.Keys().Delete(ctx, "key_1") },
		"Credentials.Put":    func() error { return s.Credentials().Put(ctx, "prv_1", store.Sealed{}) },
		"Credentials.Rewrap": func() error { return s.Credentials().Rewrap(ctx, "prv_1", 1, nil, nil) },
		"Credentials.Get":    func() error { _, err := s.Credentials().Get(ctx, "prv_1"); return err },
		"Credentials.Delete": func() error { return s.Credentials().Delete(ctx, "prv_1") },
		"Credentials.List":   func() error { _, err := s.Credentials().List(ctx); return err },
		"Counters.Add":       func() error { _, err := s.Counters().Add(ctx, "k", 1, time.Time{}); return err },
		"Counters.Read":      func() error { _, err := s.Counters().Read(ctx, []string{"k"}); return err },
		"Counters.Prune":     func() error { _, err := s.Counters().Prune(ctx, time.Now()); return err },
		"Leases.Acquire":     func() error { _, err := s.Leases().Acquire(ctx, "discovery", "a", time.Second); return err },
		"Leases.Release":     func() error { return s.Leases().Release(ctx, "discovery", "a") },
		"Journal.Append": func() error {
			_, err := s.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: "key_1"})
			return err
		},
		"Journal.Pending":     func() error { _, err := s.Journal().Pending(ctx, 0); return err },
		"Journal.Acknowledge": func() error { return s.Journal().Acknowledge(ctx, "evt_1") },
		"Journal.Defer":       func() error { return s.Journal().Defer(ctx, "evt_1", 1, time.Now()) },
		"Journal.Drop":        func() error { return s.Journal().Drop(ctx, "evt_1") },
		"Journal.ByObject":    func() error { _, _, err := s.Journal().ByObject(ctx, "key_1", store.Page{}); return err },
		"Journal.Since":       func() error { _, err := s.Journal().Since(ctx, 0, 0); return err },
		"Journal.Prune":       func() error { _, err := s.Journal().Prune(ctx, time.Now()); return err },
		"Tunnels.Register": func() error {
			return s.Tunnels().Register(ctx, store.Tunnel{ProviderID: "prv_1", Session: "tun_1"}, time.Second)
		},
		"Tunnels.Heartbeat":  func() error { _, err := s.Tunnels().Heartbeat(ctx, "prv_1", "tun_1", time.Second); return err },
		"Tunnels.Get":        func() error { _, err := s.Tunnels().Get(ctx, "prv_1"); return err },
		"Tunnels.Unregister": func() error { return s.Tunnels().Unregister(ctx, "prv_1", "tun_1") },
		"Usage.AddRows": func() error {
			return s.Usage().AddRows(ctx, []metering.Aggregate{{Bucket: time.Now(), KeyID: "key_1"}})
		},
		"Usage.QueryRows":    func() error { _, err := s.Usage().QueryRows(ctx, metering.Query{}); return err },
		"Usage.AppendRecord": func() error { return s.Usage().AppendRecord(ctx, metering.Record{ID: "req_1"}) },
		"Usage.Records": func() error {
			_, _, err := s.Usage().Records(ctx, metering.RecordQuery{}, store.Page{})
			return err
		},
		"Ready": func() error { return s.Ready(ctx) },
	}
	for name, call := range calls {
		wantErr(t, call(), context.Canceled, name)
	}
}
