// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestStoreSpansOpenUnderAParent is spec 019's lux.store: a method
// called under a span opens one child carrying the operation, the kind
// where there is one, and the result; a method called with no span in
// its context, a job's tick, opens none and still counts; a Transact is
// one span, and the operations inside it run within its time.
func TestStoreSpansOpenUnderAParent(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	reg := metrics.NewRegistry()
	s := store.Instrument(memory.New(), reg)
	counter := reg.Counter(store.MetricOperations, "")

	// No parent: a count and no span.
	if _, _, err := s.Objects().ByName(t.Context(), v1.KindProvider, "openai"); err == nil {
		t.Fatal("a missing Provider was found")
	}
	if len(sr.Ended()) != 0 {
		t.Fatalf("%d spans with no parent", len(sr.Ended()))
	}
	if counter.Value(labels("Objects.ByName", store.ResultOK)) != 1 {
		t.Error("the operation was not counted without a parent")
	}

	// A parent: one child per operation with the three attributes.
	ctx, parent := tp.Tracer("test").Start(t.Context(), "lux.api")
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "openai"},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1"},
		Status:   v1.ProviderStatus{ID: "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y", Owner: "https://login.example.com|alice"},
	}
	if _, err := s.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Objects().ByName(ctx, v1.KindProvider, "nobody"); err == nil {
		t.Fatal("a missing Provider was found")
	}
	if _, err := s.Objects().Put(ctx, p, 7); err == nil {
		t.Fatal("a stale version was accepted")
	}
	if err := s.Transact(ctx, func(tx store.Store) error {
		_, err := tx.Keys().ByHash(ctx, "0000000000000000000000000000000000000000000000000000000000000000")
		return err
	}); err == nil {
		t.Fatal("a missing hash was found")
	}
	parent.End()
	spans := sr.Ended()
	got := map[string][]string{}
	var transact, inner sdktrace.ReadOnlySpan
	for _, sp := range spans {
		if sp.Name() != store.SpanStore {
			continue
		}
		attrs := map[string]string{}
		for _, kv := range sp.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		got[attrs[store.AttrOp]] = append(got[attrs[store.AttrOp]], attrs[store.AttrKind]+"/"+attrs[store.AttrResult])
		if sp.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("%s is not the parent's child", attrs[store.AttrOp])
		}
		switch attrs[store.AttrOp] {
		case "Transact":
			transact = sp
		case "Keys.ByHash":
			inner = sp
		}
	}
	want := map[string][]string{
		"Objects.Put":    {"Provider/ok", "Provider/conflict"},
		"Objects.ByName": {"Provider/ok"},
		"Transact":       {"/ok"},
		"Keys.ByHash":    {"/ok"},
	}
	for op, w := range want {
		if len(got[op]) != len(w) {
			t.Errorf("%s: spans %v, want %v", op, got[op], w)
			continue
		}
		for i := range w {
			if got[op][i] != w[i] {
				t.Errorf("%s: span %d is %s, want %s", op, i, got[op][i], w[i])
			}
		}
	}
	// Transact's fn takes no context, so an operation inside it runs
	// under the caller's and is the caller's child beside the
	// transaction's span, and the two spans overlap in time.
	if transact == nil || inner == nil || inner.StartTime().Before(transact.StartTime()) || inner.EndTime().After(transact.EndTime()) {
		t.Error("the operation inside Transact did not run inside the transaction span's time")
	}
	if counter.Value(labels("Objects.Put", store.ResultConflict)) != 1 || counter.Value(labels("Transact", store.ResultOK)) != 1 {
		t.Error("the operations under a parent were not counted")
	}
}
