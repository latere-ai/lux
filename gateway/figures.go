// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"strconv"
	"strings"

	"latere.ai/x/pkg/llmdialect/bridge"

	v1 "latere.ai/x/lux/manifest/v1"
)

// modelEntry is a Model as a door's model list and read carry it: its
// name, the gateway as the owner, and the figures its spec holds, which
// the bridge writes in each wire's own members. A figure the spec does
// not hold stays zero, and the bridge leaves it out.
func modelEntry(m *v1.Model) bridge.Model {
	e := bridge.Model{
		Name:            m.Metadata.Name,
		OwnedBy:         modelOwner,
		ContextWindow:   m.Spec.ContextWindow,
		MaxOutputTokens: m.Spec.MaxOutputTokens,
		Pricing:         pricingOf(m.Spec.Pricing),
	}
	// A discovered Model's modalities are the kind's default, which the
	// discovery job writes knowing nothing of the model; only a declared
	// Model's are a statement about it.
	if m.Status.Source != v1.SourceDiscovered {
		for _, mod := range m.Spec.Modalities.Input {
			e.InputModalities = append(e.InputModalities, string(mod))
		}
	}
	return e
}

// pricingOf is the Model's prices per 1,000,000 tokens, the count every
// list quotes them per, or nil for an unpriced Model and for one whose
// per is not a million divided by a power of ten, which the kind does
// not admit and which no decimal shift converts exactly. A price the
// spec does not hold stays empty.
func pricingOf(p *v1.Pricing) *bridge.ModelPricing {
	if p == nil {
		return nil
	}
	out := &bridge.ModelPricing{Currency: p.Currency}
	for _, price := range []struct {
		money *v1.Money
		to    *string
	}{{p.Input, &out.Input}, {p.Output, &out.Output}, {p.CachedInput, &out.CachedInput}, {p.CacheWrite, &out.CacheWrite}} {
		if price.money == nil {
			continue
		}
		s, ok := perMillion(*price.money, p.Per)
		if !ok {
			return nil
		}
		*price.to = s
	}
	return out
}

// pricePer is the token count every listed price is quoted per.
const pricePer = 1_000_000

// perMillion renders a price quoted per `per` tokens as its price per
// 1,000,000 tokens. Money is micro-units and per is 1, 1,000 or
// 1,000,000, so the price per million is the same digits with the
// decimal point moved: exact at any size, where multiplying the
// micro-units could overflow. ok is false for a negative price or a per
// that does not divide a million by a power of ten.
func perMillion(price v1.Money, per int) (string, bool) {
	if price < 0 || per <= 0 {
		return "", false
	}
	shift, scale := 0, 1
	for p := per; p < pricePer; p *= 10 {
		shift++
		scale *= 10
	}
	if per*scale != pricePer {
		return "", false
	}
	// price is micro-units per `per` tokens; per million tokens it is
	// price micro-units times 10^shift, which is price with 6-shift
	// fraction digits.
	point := v1.MoneyFractionDigits - shift
	digits := strconv.FormatInt(int64(price), 10)
	if len(digits) <= point {
		digits = strings.Repeat("0", point-len(digits)+1) + digits
	}
	whole, frac := digits[:len(digits)-point], strings.TrimRight(digits[len(digits)-point:], "0")
	if frac == "" {
		return whole, true
	}
	return whole + "." + frac, true
}
