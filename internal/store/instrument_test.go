// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

func labels(op, result string) map[string]string {
	return map[string]string{"op": op, "result": result}
}

// TestStoreOperationsAreCounted drives every method once through
// Instrument and reads the counter: one increment per call, with the
// method's name and the result the error classifies to.
func TestStoreOperationsAreCounted(t *testing.T) {
	ctx := t.Context()
	reg := metrics.NewRegistry()
	s := store.Instrument(memory.New(), reg)
	counter := reg.Counter(store.MetricOperations, "")

	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "openai"},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1"},
		Status:   v1.ProviderStatus{ID: "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y", Owner: "https://login.example.com|alice"},
	}
	hash := strings.Repeat("ab", 32)
	calls := []struct {
		op     string
		result string
		call   func() error
	}{
		{"Objects.Put", store.ResultOK, func() error { _, err := s.Objects().Put(ctx, p, 0); return err }},
		{"Objects.Put", store.ResultConflict, func() error { _, err := s.Objects().Put(ctx, p, 9); return err }},
		{"Objects.Get", store.ResultOK, func() error { _, _, err := s.Objects().Get(ctx, v1.KindProvider, "prv_none"); return err }},
		{"Objects.ByName", store.ResultOK, func() error { _, _, err := s.Objects().ByName(ctx, v1.KindProvider, "openai"); return err }},
		{"Objects.List", store.ResultConflict, func() error {
			_, _, err := s.Objects().List(ctx, v1.KindProvider, store.Filter{}, store.Page{Cursor: "x"})
			return err
		}},
		{"Objects.PutStatus", store.ResultError, func() error { return s.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, 1) }},
		{"Objects.Delete", store.ResultOK, func() error { return s.Objects().Delete(ctx, v1.KindProvider, p.Status.ID) }},
		{"Objects.Prune", store.ResultOK, func() error { _, err := s.Objects().Prune(ctx, time.Now()); return err }},
		{"Keys.Put", store.ResultError, func() error { return s.Keys().Put(ctx, "key_1", "short") }},
		{"Keys.Put", store.ResultOK, func() error { return s.Keys().Put(ctx, "key_1", hash) }},
		{"Keys.Put", store.ResultConflict, func() error { return s.Keys().Put(ctx, "key_2", hash) }},
		{"Keys.ByHash", store.ResultOK, func() error { _, err := s.Keys().ByHash(ctx, hash); return err }},
		{"Keys.Delete", store.ResultOK, func() error { return s.Keys().Delete(ctx, "key_1") }},
		{"Credentials.Put", store.ResultOK, func() error { return s.Credentials().Put(ctx, "prv_1", store.Sealed{Version: 1}) }},
		{"Credentials.Rewrap", store.ResultConflict, func() error { return s.Credentials().Rewrap(ctx, "prv_1", 2, nil, nil) }},
		{"Credentials.Get", store.ResultOK, func() error { _, err := s.Credentials().Get(ctx, "prv_1"); return err }},
		{"Credentials.List", store.ResultOK, func() error { _, err := s.Credentials().List(ctx); return err }},
		{"Credentials.Delete", store.ResultOK, func() error { return s.Credentials().Delete(ctx, "prv_1") }},
		{"Counters.Add", store.ResultOK, func() error { _, err := s.Counters().Add(ctx, "k", 1, time.Time{}); return err }},
		{"Counters.Read", store.ResultOK, func() error { _, err := s.Counters().Read(ctx, []string{"k"}); return err }},
		{"Counters.Prune", store.ResultOK, func() error { _, err := s.Counters().Prune(ctx, time.Now()); return err }},
		{"Leases.Acquire", store.ResultOK, func() error { _, err := s.Leases().Acquire(ctx, "discovery", "a", time.Second); return err }},
		{"Leases.Release", store.ResultOK, func() error { return s.Leases().Release(ctx, "discovery", "a") }},
		{"Journal.Append", store.ResultOK, func() error {
			_, err := s.Journal().Append(ctx, store.Event{ID: "evt_1", ObjectID: "key_1"})
			return err
		}},
		{"Journal.Pending", store.ResultOK, func() error { _, err := s.Journal().Pending(ctx, 0); return err }},
		{"Journal.Defer", store.ResultOK, func() error { return s.Journal().Defer(ctx, "evt_1", 1, time.Now()) }},
		{"Journal.ByObject", store.ResultOK, func() error { _, _, err := s.Journal().ByObject(ctx, "key_1", store.Page{}); return err }},
		{"Journal.Since", store.ResultOK, func() error { _, err := s.Journal().Since(ctx, 0, 0); return err }},
		{"Journal.Acknowledge", store.ResultOK, func() error { return s.Journal().Acknowledge(ctx, "evt_1") }},
		{"Journal.Prune", store.ResultOK, func() error { _, err := s.Journal().Prune(ctx, time.Now().Add(time.Hour)); return err }},
		{"Journal.Drop", store.ResultOK, func() error { return s.Journal().Drop(ctx, "evt_none") }},
		{"Tunnels.Register", store.ResultOK, func() error {
			return s.Tunnels().Register(ctx, store.Tunnel{ProviderID: "prv_1", Session: "tun_1"}, time.Minute)
		}},
		{"Tunnels.Heartbeat", store.ResultOK, func() error { _, err := s.Tunnels().Heartbeat(ctx, "prv_1", "tun_1", time.Minute); return err }},
		{"Tunnels.Get", store.ResultOK, func() error { _, err := s.Tunnels().Get(ctx, "prv_1"); return err }},
		{"Tunnels.Unregister", store.ResultOK, func() error { return s.Tunnels().Unregister(ctx, "prv_1", "tun_1") }},
		{"Ready", store.ResultOK, func() error { return s.Ready(ctx) }},
		{"Transact", store.ResultError, func() error {
			return s.Transact(ctx, func(tx store.Store) error { return errors.New("fn failed") })
		}},
	}
	seen := map[string]uint64{}
	for _, c := range calls {
		before := counter.Value(labels(c.op, c.result))
		_ = c.call()
		if got := counter.Value(labels(c.op, c.result)); got != before+1 {
			t.Errorf("%s %s: counted %d, want %d", c.op, c.result, got, before+1)
		}
		seen[c.op]++
	}
	// Every method of every collection was driven.
	for _, iface := range []reflect.Type{
		reflect.TypeFor[store.Objects](), reflect.TypeFor[store.Keys](), reflect.TypeFor[store.Credentials](),
		reflect.TypeFor[store.Counters](), reflect.TypeFor[store.Leases](), reflect.TypeFor[store.Journal](), reflect.TypeFor[store.Tunnels](),
	} {
		name := strings.TrimPrefix(iface.String(), "store.")
		for m := range iface.Methods() {
			if seen[name+"."+m.Name] == 0 {
				t.Errorf("%s.%s was not driven", name, m.Name)
			}
		}
	}

	// The Store handed to fn counts too, and a nested Transact is an error.
	before := counter.Value(labels("Objects.Get", store.ResultOK))
	err := s.Transact(ctx, func(tx store.Store) error {
		_, _, _ = tx.Objects().Get(ctx, v1.KindProvider, "prv_none")
		if err := tx.Transact(ctx, func(store.Store) error { return nil }); !errors.Is(err, store.ErrNested) {
			t.Errorf("nested Transact = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := counter.Value(labels("Objects.Get", store.ResultOK)); got != before+1 {
		t.Errorf("an operation inside a Transact was not counted: %d, want %d", got, before+1)
	}
	if got := counter.Value(labels("Transact", store.ResultError)); got != 2 {
		t.Errorf("Transact errors = %d, want 2 (the failing fn and the nested one)", got)
	}
	if got := counter.Value(labels("Transact", store.ResultOK)); got != 1 {
		t.Errorf("Transact ok = %d, want 1", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Two instrumented stores on one registry share the series.
	other := store.Instrument(memory.New(), reg)
	if err := other.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counter.Value(labels("Ready", store.ResultOK)); got != 2 {
		t.Errorf("Ready ok across two stores = %d, want 2", got)
	}
}
