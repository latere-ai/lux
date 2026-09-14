// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"net/http"
	"testing"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestStateChangeEventIsRaisedOnce is spec 012's once-per-transition
// rule over the two mechanisms: six Limiters on one store each observe
// one Budget crossing its amount and the marker counter lets exactly one
// raise budget.exhausted; six health jobs on one store each tick three
// times over one failing Provider and the health lease lets exactly one
// raise provider.unreachable.
func TestStateChangeEventIsRaisedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// Six replicas' Limiters against one hard Budget.
	p := priced("p", "0.01", "0", "USD")
	hard := h.budget(t, "hard", "0.05", "USD", "1h", true)
	k, _ := h.key(t, "free", draws(hard))
	ls := make([]*Limiter, 6)
	for i := range ls {
		ls[i] = h.limiter(h.st, nil, manifest.Defaults{})
	}
	refused := 0
	for n := 0; refused < len(ls) && n < 100; n++ {
		if ref := spendOne(t, ls[n%len(ls)], k, p); ref != nil {
			if ref.Code != gateway.CodeBudgetExhausted {
				t.Fatalf("refused %s, want budget_exhausted", ref.Code)
			}
			refused++
		}
		for _, l := range ls {
			l.Flush(ctx)
		}
	}
	if refused < len(ls) {
		t.Fatalf("only %d of six replicas refused", refused)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted events from six replicas, want 1", n)
	}

	// Six replicas' health jobs over one Provider that fails every probe.
	down := &stub{}
	down.set(http.StatusInternalServerError, `{"error":"down"}`)
	h.provider(t, "down", v1.DialectOpenAI, serveStub(t, down)+"/v1", nil)
	jobs := make([]*Health, 6)
	for i := range jobs {
		jobs[i] = h.health("replica-" + string(rune('a'+i)))
		jobs[i].acquire(ctx)
	}
	holders := 0
	for _, j := range jobs {
		if j.Held() {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("%d replicas hold the health lease, want 1", holders)
	}
	for range 3 {
		for _, j := range jobs {
			j.Tick(ctx)
		}
	}
	if n := len(h.events(t, eventProviderUnreachable)); n != 1 {
		t.Fatalf("%d provider.unreachable events from six replicas, want 1", n)
	}
	if n := len(h.events(t, eventProviderHealthy)); n != 0 {
		t.Fatalf("%d provider.healthy events for a Provider that never recovered", n)
	}
}
