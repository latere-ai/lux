// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"reflect"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// money parses a money string or stops the test.
func money(t testing.TB, s string) *v1.Money {
	t.Helper()
	m, err := v1.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return &m
}

// pricing quotes the four prices per per tokens in currency.
func pricing(t testing.TB, currency string, per int, input, output, cached, write string) *v1.Pricing {
	t.Helper()
	p := &v1.Pricing{Currency: currency, Per: per}
	if input != "" {
		p.Input = money(t, input)
	}
	if output != "" {
		p.Output = money(t, output)
	}
	if cached != "" {
		p.CachedInput = money(t, cached)
	}
	if write != "" {
		p.CacheWrite = money(t, write)
	}
	return p
}

// TestCostGolden: a corpus of pricings and token counts produces the
// golden costs, per 1, 1000, and 1000000 tokens, in any currency, with
// one rounding half up over the whole sum, and a nil pricing is
// unpriced.
func TestCostGolden(t *testing.T) {
	for _, c := range []struct {
		name   string
		tokens Tokens
		p      *v1.Pricing
		want   string
		priced bool
	}{
		{"per token", Tokens{Input: 3, Output: 2}, pricing(t, "USD", 1, "0.01", "0.02", "", ""), "0.07", true},
		{"per thousand", Tokens{Input: 1500, Output: 500}, pricing(t, "EUR", 1000, "0.5", "1.5", "", ""), "1.5", true},
		{"per million, the usual card", Tokens{Input: 1_000_000, Output: 100_000}, pricing(t, "USD", 1_000_000, "2.5", "10", "", ""), "3.5", true},
		{"per million, one token", Tokens{Input: 1}, pricing(t, "USD", 1_000_000, "2.5", "10", "", ""), "0.000003", true},
		{"half up at the boundary", Tokens{Input: 1}, pricing(t, "USD", 1_000_000, "0.5", "", "", ""), "0.000001", true},
		{"below half rounds down", Tokens{Input: 1}, pricing(t, "USD", 1_000_000, "0.499999", "", "", ""), "0", true},
		{"one rounding over the sum", Tokens{Input: 1, Output: 1}, pricing(t, "USD", 1_000_000, "0.3", "0.3", "", ""), "0.000001", true},
		{"cached and written", Tokens{Input: 10, CachedInput: 90, CacheWrite: 20}, pricing(t, "GBP", 1, "0.01", "0.02", "0.001", "0.0125"), "0.44", true},
		{"a price the card does not name is zero", Tokens{Input: 5, Output: 5, CachedInput: 5, CacheWrite: 5}, pricing(t, "JPY", 1, "1", "", "", ""), "5", true},
		{"zero tokens", Tokens{}, pricing(t, "USD", 1000, "1", "1", "1", "1"), "0", true},
		{"a pricing with no rates prices at zero", Tokens{Input: 100}, &v1.Pricing{Currency: "USD", Per: 1000}, "0", true},
		{"a per of zero is read as one", Tokens{Input: 2}, &v1.Pricing{Currency: "USD", Input: money(t, "0.5")}, "1", true},
		{"unpriced", Tokens{Input: 100, Output: 100}, nil, "0", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, priced := Cost(c.tokens, c.p)
			if got.String() != c.want || priced != c.priced {
				t.Fatalf("Cost = %s, %v; want %s, %v", got, priced, c.want, c.priced)
			}
			charge := Charged(c.tokens, c.p)
			if charge.Priced != c.priced || charge.Amount != int64(got) {
				t.Fatalf("Charged = %+v", charge)
			}
			if c.priced && charge.Currency != c.p.Currency {
				t.Fatalf("Charged carries currency %q, want %q", charge.Currency, c.p.Currency)
			}
			if !c.priced && charge != (Charge{}) {
				t.Fatalf("an unpriced charge = %+v", charge)
			}
		})
	}
}

// TestCachedInputIsNotBilledTwice: the cached count is a term at the
// cached price alone, because Input already excludes it on every
// dialect, so a request with cached tokens costs less than the same
// request read fresh and never more.
func TestCachedInputIsNotBilledTwice(t *testing.T) {
	p := pricing(t, "USD", 1_000_000, "3", "15", "0.3", "3.75")
	fresh, _ := Cost(Tokens{Input: 1000, Output: 100}, p)
	cached, _ := Cost(Tokens{Input: 200, CachedInput: 800, Output: 100}, p)
	if fresh.String() != "0.0045" || cached.String() != "0.00234" {
		t.Fatalf("fresh %s, cached %s", fresh, cached)
	}
	// Billing the cached count at both prices would be 0.0045 + 0.00024.
	if double := v1.Money(int64(fresh) + 800*int64(*p.CachedInput)/1_000_000); cached >= fresh || cached == double {
		t.Fatalf("cached input was billed twice: %s", cached)
	}
}

// TestReasoningIsNotBilled: the reasoning count is recorded and is not
// a term in the cost, whatever the card says.
func TestReasoningIsNotBilled(t *testing.T) {
	p := pricing(t, "USD", 1, "1", "1", "1", "1")
	with, _ := Cost(Tokens{Input: 1, Output: 5, Reasoning: 4}, p)
	without, _ := Cost(Tokens{Input: 1, Output: 5}, p)
	if with != without || with.String() != "6" {
		t.Fatalf("with reasoning %s, without %s", with, without)
	}
}

// TestRecordCarriesNoContent is the half the type proves: Record has no
// member of interface type, no byte slice, and no reader anywhere in
// its shape, so no field can hold a body; the values are the strings
// and numbers the table names. The other half, that a canary appears
// in no record of a run, is the gateway's and the e2e tier's.
func TestRecordCarriesNoContent(t *testing.T) {
	var seen []string
	walk(t, reflect.TypeFor[Record](), "Record", &seen)
	for _, f := range seen {
		t.Errorf("%s can carry a body", f)
	}
}

// walk reports every field of ty whose kind could hold content: an
// interface, a byte slice, a pointer to one, or a function.
func walk(t *testing.T, ty reflect.Type, path string, out *[]string) {
	t.Helper()
	switch ty.Kind() {
	case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		*out = append(*out, path+" is a "+ty.Kind().String())
	case reflect.Slice, reflect.Array:
		if ty.Elem().Kind() == reflect.Uint8 {
			*out = append(*out, path+" is bytes")
			return
		}
		walk(t, ty.Elem(), path+"[]", out)
	case reflect.Map:
		walk(t, ty.Key(), path+"{key}", out)
		walk(t, ty.Elem(), path+"{value}", out)
	case reflect.Pointer:
		walk(t, ty.Elem(), "*"+path, out)
	case reflect.Struct:
		if ty == reflect.TypeFor[time.Time]() {
			return
		}
		for f := range ty.Fields() {
			walk(t, f.Type, path+"."+f.Name, out)
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String, reflect.Invalid:
	}
}

// TestWalkFindsContent: the walker names an interface member, a byte
// slice, and a function, so its silence over Record means something.
func TestWalkFindsContent(t *testing.T) {
	type leaky struct {
		Body   any
		Raw    []byte
		Hook   func()
		Nested struct{ Reader *[]byte }
		Fine   map[string][]string
	}
	var seen []string
	walk(t, reflect.TypeFor[leaky](), "leaky", &seen)
	if len(seen) != 4 {
		t.Fatalf("the walker found %d of 4 leaks: %v", len(seen), seen)
	}
}

// TestWindowBoundaries: a duration window's key is the same for every
// instant inside it and differs across the boundary; month resets on
// the first in UTC; none never resets; and the totals' key carries the
// word none, not the Key's createdAt, because a duration window that
// begins at the creation instant must not share the totals' key, which
// is what spec 007 settled and this spec's table takes.
func TestWindowBoundaries(t *testing.T) {
	inside := CounterKey(ScopeKeySpend, "key_1", "1h", at.Add(42*time.Minute), created)
	if inside != CounterKey(ScopeKeySpend, "key_1", "1h", at, created) {
		t.Errorf("two instants inside one window name two keys: %s", inside)
	}
	start, resets := Window("1h", at, created)
	if next := CounterKey(ScopeKeySpend, "key_1", "1h", resets, created); next == inside || !start.Equal(at.Truncate(time.Hour)) {
		t.Errorf("the boundary did not move the key: %s", next)
	}
	mStart, mResets := Window(v1.WindowMonth, at, created)
	if mStart.Day() != 1 || mStart.Hour() != 0 || mResets.Month() != mStart.Month()+1 || mStart.Location() != time.UTC {
		t.Errorf("month = %s .. %s", mStart, mResets)
	}
	if _, nResets := Window(v1.WindowNone, at, created); !nResets.IsZero() {
		t.Errorf("none resets at %s", nResets)
	}
	totals := TotalKey(ScopeKeySpend, "key_1")
	if totals != "key:key_1:spend:none" || CounterKey(ScopeKeySpend, "key_1", v1.WindowNone, at, created) != totals {
		t.Errorf("the totals key = %s", totals)
	}
	if CounterKey(ScopeKeySpend, "key_1", "1h", created, created) == totals {
		t.Error("a window aligned to createdAt shares the totals' key")
	}
}
