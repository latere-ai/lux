// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The read-time half of a Key's and a Budget's status: what the
// counters say now, rendered at every read rather than written by a
// job, so two replicas reading one store render the same answer. The
// arithmetic, the window keys, and the boundaries are metering's.

// render fills the status members that are a function of the counters.
func (p *Plane) render(obj v1.Object) v1.Object {
	switch x := obj.(type) {
	case *v1.Key:
		p.renderKey(x)
	case *v1.Budget:
		p.renderBudget(x)
	}
	return obj
}

// renderKey fills the Key's usage and its state: disabled from the
// spec, expired from the clock, exhausted when its window or its hard
// Budget's is at the amount, and active otherwise.
func (p *Plane) renderKey(k *v1.Key) {
	now := p.now()
	id, created := k.Status.ID, k.Status.CreatedAt
	total := v1.UsageTotal{
		Requests: p.limiter.counters.Total(metering.TotalKey(metering.ScopeKeyRequests, id)),
		Tokens:   p.limiter.counters.Total(metering.TotalKey(metering.ScopeKeyTokens, id)),
		Spend:    v1.Money(p.limiter.counters.Total(metering.TotalKey(metering.ScopeKeySpend, id))),
	}
	usage := &v1.KeyUsage{Total: total}
	exhausted := false
	if spend := spendOf(k); spend != nil {
		_, resetsAt := metering.Window(spend.Window, now, created)
		window := v1.UsageWindow{
			Requests: p.limiter.counters.Total(metering.CounterKey(metering.ScopeKeyRequests, id, spend.Window, now, created)),
			Tokens:   p.limiter.counters.Total(metering.CounterKey(metering.ScopeKeyTokens, id, spend.Window, now, created)),
			Spend:    v1.Money(p.limiter.counters.Total(metering.CounterKey(metering.ScopeKeySpend, id, spend.Window, now, created))),
			ResetsAt: resetsAt,
		}
		usage.Window = &window
		exhausted = int64(window.Spend) >= int64(*spend.Amount)
	}
	for _, b := range p.hardBudgetsOf(k) {
		spent := p.limiter.counters.Total(metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, now))
		if b.Spec.Amount != nil && spent >= int64(*b.Spec.Amount) {
			exhausted = true
		}
	}
	k.Status.Usage = usage
	switch {
	case k.Spec.Disabled:
		k.Status.State = v1.KeyDisabled
	case !k.Status.ExpiresAt.IsZero() && !now.Before(k.Status.ExpiresAt):
		k.Status.State = v1.KeyExpired
	case exhausted:
		k.Status.State = v1.KeyExhausted
	default:
		k.Status.State = v1.KeyActive
	}
}

// renderBudget fills what the Budget has spent in its window, what is
// left, how many Keys draw from it, and its state.
func (p *Plane) renderBudget(b *v1.Budget) {
	now := p.now()
	_, resetsAt := metering.BudgetWindow(b, now)
	spent := v1.Money(p.limiter.counters.Total(metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, now)))
	amount := v1.Money(0)
	if b.Spec.Amount != nil {
		amount = *b.Spec.Amount
	}
	remaining := max(amount-spent, 0)
	keys := 0
	for _, obj := range p.store.list(v1.KindKey) {
		if k, ok := obj.(*v1.Key); ok && slices.ContainsFunc(v1.KeyBudgets(k), func(r v1.BudgetRef) bool { return r.ID == b.Status.ID }) {
			keys++
		}
	}
	b.Status.Spent, b.Status.Remaining, b.Status.Keys = &spent, &remaining, &keys
	b.Status.ResetsAt = resetsAt
	b.Status.State = v1.BudgetOpen
	if spent >= amount {
		b.Status.State = v1.BudgetExhausted
	}
}

// spendOf is the Key's spend limit, or nil when it names none.
func spendOf(k *v1.Key) *v1.Spend {
	if s := k.Spec.Limits.Spend; s != nil && s.Amount != nil {
		return s
	}
	return nil
}

// hardBudgetsOf is every hard Budget the Key draws from; a soft one
// never exhausts a Key.
func (p *Plane) hardBudgetsOf(k *v1.Key) []*v1.Budget {
	var out []*v1.Budget
	for _, ref := range v1.KeyBudgets(k) {
		obj, _, err := p.store.get(v1.KindBudget, ref.ID)
		if err != nil {
			continue
		}
		if b, ok := obj.(*v1.Budget); ok && (b.Spec.Hard == nil || *b.Spec.Hard) {
			out = append(out, b)
		}
	}
	return out
}
