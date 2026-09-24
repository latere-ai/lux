// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The status members of a Key and a Budget that spec 007 renders at
// read time from the store's counters, so no flush writes a row per
// object per interval and every replica renders the same answer. The
// API of spec 011 calls these on every read and list.

// RenderKey fills k's status.state and status.usage from the counters
// as they stand now: the lifetime totals, the current spend window when
// the Key has a spend limit, and the state from spec.disabled, the
// clock, and the windows. A window is Exhausted when its spend is at or
// over the amount; a hard Budget's window exhausts the Key the same way
// and a soft one never does. A request whose estimate would cross the
// amount is refused before the window is at it, so a Key can be refused
// spend_exceeded while it renders Active with the remainder in
// status.usage; the refusal's Retry-After names the reset either way.
func RenderKey(ctx context.Context, st store.Store, k *v1.Key, now time.Time) error {
	id, createdAt := k.Status.ID, k.Status.CreatedAt
	totalRequests := metering.TotalKey(metering.ScopeKeyRequests, id)
	totalTokens := metering.TotalKey(metering.ScopeKeyTokens, id)
	totalSpend := metering.TotalKey(metering.ScopeKeySpend, id)
	keys := []string{totalRequests, totalTokens, totalSpend}
	var windowKeys [3]string // requests, tokens, spend
	var resetsAt time.Time
	spend := spendLimit(k)
	if spend != nil {
		_, resetsAt = metering.Window(spend.Window, now, createdAt)
		windowKeys = [3]string{
			metering.CounterKey(metering.ScopeKeyRequests, id, spend.Window, now, createdAt),
			metering.CounterKey(metering.ScopeKeyTokens, id, spend.Window, now, createdAt),
			metering.CounterKey(metering.ScopeKeySpend, id, spend.Window, now, createdAt),
		}
		keys = append(keys, windowKeys[:]...)
	}
	type hardBudget struct {
		key    string
		amount v1.Money
	}
	var hard []hardBudget
	for _, ref := range v1.KeyBudgets(k) {
		obj, _, err := st.Objects().Get(ctx, v1.KindBudget, ref.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("reading Budget %s of Key %s: %w", ref.ID, id, err)
		}
		if b, ok := obj.(*v1.Budget); ok && isHard(b) {
			key := metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, now)
			hard = append(hard, hardBudget{key: key, amount: *b.Spec.Amount})
			keys = append(keys, key)
		}
	}
	m, err := st.Counters().Read(ctx, keys)
	if err != nil {
		return fmt.Errorf("reading the counters of Key %s: %w", id, err)
	}
	usage := &v1.KeyUsage{Total: v1.UsageTotal{Requests: m[totalRequests], Tokens: m[totalTokens], Spend: v1.Money(m[totalSpend])}}
	exhausted := false
	if spend != nil {
		usage.Window = &v1.UsageWindow{Requests: m[windowKeys[0]], Tokens: m[windowKeys[1]], Spend: v1.Money(m[windowKeys[2]]), ResetsAt: resetsAt}
		exhausted = m[windowKeys[2]] >= int64(*spend.Amount)
	}
	for _, b := range hard {
		if m[b.key] >= int64(b.amount) {
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
	return nil
}

// RenderBudget fills b's read-time status: keys, the live Keys that
// draw on it now, read through the store's Budget filter rather than
// every Key; spent, the current window's counter; remaining, the amount
// less that floored at zero; resetsAt, the window's reset or absent
// for none; and state, Exhausted while spent is at or over the amount
// and Open otherwise, so raising the amount opens it at the next read.
// The window is spec 037's: aligned to spec.anchor and started at
// spec.restartedAt when that lies inside it.
func RenderBudget(ctx context.Context, st store.Store, b *v1.Budget, now time.Time) error {
	id := b.Status.ID
	_, resetsAt := metering.BudgetWindow(b, now)
	spendKey := metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, now)
	m, err := st.Counters().Read(ctx, []string{spendKey})
	if err != nil {
		return fmt.Errorf("reading the counters of Budget %s: %w", id, err)
	}
	keys, _, err := st.Objects().List(ctx, v1.KindKey, store.Filter{Budget: id}, store.Page{})
	if err != nil {
		return fmt.Errorf("listing the Keys of Budget %s: %w", id, err)
	}
	n := len(keys)
	spent := v1.Money(m[spendKey])
	amount := v1.Money(0)
	if b.Spec.Amount != nil {
		amount = *b.Spec.Amount
	}
	remaining := max(amount-spent, 0)
	b.Status.Spent, b.Status.Remaining, b.Status.Keys = &spent, &remaining, &n
	b.Status.ResetsAt = resetsAt
	b.Status.State = v1.BudgetOpen
	if spent >= amount {
		b.Status.State = v1.BudgetExhausted
	}
	return nil
}
