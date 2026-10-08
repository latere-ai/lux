// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import v1 "latere.ai/x/lux/manifest/v1"

// Cost prices t under p. Pricing quotes money per Per tokens, with Per
// one of 1, 1000, and 1000000 (spec 003), and Money is an integer count
// of micro-units of the currency, so no float touches a price at any
// point. There is one rounding, half up, over the whole sum: rounding
// each term would let a price change of zero move a bill. A nil p is
// an unpriced Model and answers 0 and false; a price the Pricing does
// not name is 0, and Reasoning is not a term, because the upstream
// reports it inside Output already.
func Cost(t Tokens, p *v1.Pricing) (v1.Money, bool) {
	if p == nil {
		return 0, false
	}
	per := int64(p.Per)
	if per <= 0 {
		per = 1
	}
	n := t.Input*price(p.Input) +
		t.Output*price(p.Output) +
		t.CachedInput*price(p.CachedInput) +
		t.CacheWrite*price(p.CacheWrite)
	return v1.Money((n + per/2) / per), true
}

// Free reports whether p prices every token at zero: every member Cost
// reads, input, output, cachedInput, and cacheWrite, is zero or unset,
// so Cost answers zero for any count. It is the one test of whether a
// request can spend, which the spend and Budget windows read to admit a
// request whatever their remainder (spec 045). A nil p is an unpriced
// Model, whose cost is unknown rather than zero, and is not free; one
// member above zero makes p priced however small it is, even where
// rounding would bill a short request at nothing.
func Free(p *v1.Pricing) bool {
	if p == nil {
		return false
	}
	return price(p.Input) == 0 && price(p.Output) == 0 && price(p.CachedInput) == 0 && price(p.CacheWrite) == 0
}

// price is the micro-units a Pricing member quotes, 0 when it is unset.
func price(m *v1.Money) int64 {
	if m == nil {
		return 0
	}
	return int64(*m)
}

// Charged is the record's cost block for t under p: the Cost and the
// Pricing's currency when p prices the request, and an unpriced block
// with an empty currency and a zero amount otherwise.
func Charged(t Tokens, p *v1.Pricing) Charge {
	amount, priced := Cost(t, p)
	if !priced {
		return Charge{}
	}
	return Charge{Amount: int64(amount), Currency: p.Currency, Priced: true}
}
