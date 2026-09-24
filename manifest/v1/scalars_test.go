// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMoneyParsesAndRendersWithoutLoss(t *testing.T) {
	cases := []struct {
		in    string
		micro Money
		out   string
	}{
		{"0", 0, "0"},
		{"1", 1_000_000, "1"},
		{"10", 10_000_000, "10"},
		{"1.25", 1_250_000, "1.25"},
		{"0.125", 125_000, "0.125"},
		{"500", 500_000_000, "500"},
		{"123.45", 123_450_000, "123.45"},
		{"0.000001", 1, "0.000001"},
		{"1.250", 1_250_000, "1.25"},
		{"999999999999.999999", 999_999_999_999_999_999, "999999999999.999999"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			m, err := ParseMoney(c.in)
			if err != nil {
				t.Fatalf("ParseMoney(%q): %v", c.in, err)
			}
			if m != c.micro {
				t.Errorf("ParseMoney(%q) = %d micro-units, want %d", c.in, m, c.micro)
			}
			if got := m.String(); got != c.out {
				t.Errorf("Money(%d).String() = %q, want %q", m, got, c.out)
			}
		})
	}
	if got := Money(-1_500_000).String(); got != "-1.5" {
		t.Errorf("a negative amount renders %q, want -1.5", got)
	}
}

func TestMoneyRefusals(t *testing.T) {
	cases := []struct{ in, why string }{
		{"", "not a decimal"},
		{"1.2345678", "7 fraction digits"},
		{"1234567890123", "13 integer digits"},
		{"-1", "not a decimal"},
		{"1.", "fraction is not"},
		{".5", "not a decimal"},
		{"1,5", "not a decimal"},
		{"1e3", "not a decimal"},
		{"$5", "not a decimal"},
		{"1.5.5", "fraction is not"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			_, err := ParseMoney(c.in)
			if err == nil {
				t.Fatalf("ParseMoney(%q) accepted", c.in)
			}
			if !strings.Contains(err.Error(), c.why) {
				t.Errorf("ParseMoney(%q) = %v, want it to say %q", c.in, err, c.why)
			}
		})
	}
}

func TestMoneyJSON(t *testing.T) {
	var p struct {
		Amount Money `json:"amount"`
	}
	if err := json.Unmarshal([]byte(`{"amount":"1.25"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Amount != 1_250_000 {
		t.Errorf("decoded %d, want 1250000", p.Amount)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"amount":"1.25"}` {
		t.Errorf("encoded %s", out)
	}
	for _, bad := range []string{`{"amount":1.25}`, `{"amount":"1.2345678"}`, `{"amount":true}`} {
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("%s decoded into Money", bad)
		}
	}
}

func TestDurationOfDropsZeroUnits(t *testing.T) {
	cases := map[time.Duration]Duration{
		10 * time.Minute:                   "10m",
		time.Hour:                          "1h",
		90 * time.Minute:                   "1h30m",
		168 * time.Hour:                    "168h",
		time.Second:                        "1s",
		1500 * time.Millisecond:            "1.5s",
		time.Minute + 500*time.Millisecond: "1m0.5s",
		0:                                  "0s",
	}
	for d, want := range cases {
		if got := DurationOf(d); got != want {
			t.Errorf("DurationOf(%v) = %q, want %q", d, got, want)
		}
		back, err := DurationOf(d).Parse()
		if err != nil || back != d {
			t.Errorf("DurationOf(%v).Parse() = %v, %v", d, back, err)
		}
	}
	if _, err := Duration("").Parse(); err == nil {
		t.Error("an empty duration parsed")
	}
	if _, err := Duration("soon").Parse(); err == nil {
		t.Error("\"soon\" parsed as a duration")
	}
}

func TestWindows(t *testing.T) {
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	t.Run("a duration window has one boundary for every instant inside it", func(t *testing.T) {
		w := Window("24h")
		a := time.Date(2026, 9, 13, 0, 0, 1, 0, time.UTC)
		b := time.Date(2026, 9, 13, 23, 59, 59, 0, time.UTC)
		c := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
		sa, ra := w.Bounds(a, created)
		sb, rb := w.Bounds(b, created)
		sc, _ := w.Bounds(c, created)
		if !sa.Equal(sb) || !ra.Equal(rb) {
			t.Errorf("bounds differ inside one window: %v %v vs %v %v", sa, ra, sb, rb)
		}
		if !sa.Equal(time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)) || !ra.Equal(c) {
			t.Errorf("bounds = %v, %v", sa, ra)
		}
		if !sc.Equal(c) {
			t.Errorf("the next window starts at %v, want %v", sc, c)
		}
	})
	t.Run("month resets on the first of the month UTC", func(t *testing.T) {
		at := time.Date(2026, 9, 13, 10, 0, 0, 0, time.FixedZone("east", 3600))
		start, reset := WindowMonth.Bounds(at, created)
		if !start.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("start = %v", start)
		}
		if !reset.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("reset = %v", reset)
		}
	})
	t.Run("none never resets", func(t *testing.T) {
		start, reset := WindowNone.Bounds(time.Now(), created)
		if !start.Equal(created) || !reset.IsZero() {
			t.Errorf("bounds = %v, %v", start, reset)
		}
	})
	t.Run("an invalid window has no bounds", func(t *testing.T) {
		start, reset := Window("soon").Bounds(time.Now(), created)
		if !start.IsZero() || !reset.IsZero() {
			t.Errorf("bounds = %v, %v", start, reset)
		}
	})
	t.Run("validation", func(t *testing.T) {
		for _, ok := range []Window{"1m", "24h", "8760h", WindowMonth, WindowNone} {
			if err := ok.Validate(); err != nil {
				t.Errorf("%q refused: %v", ok, err)
			}
		}
		for _, bad := range []Window{"", "30s", "8761h", "soon", "week"} {
			if err := bad.Validate(); err == nil {
				t.Errorf("%q accepted", bad)
			}
		}
		if d, ok := Window("24h").Duration(); !ok || d != 24*time.Hour {
			t.Errorf("Duration() = %v, %v", d, ok)
		}
		if _, ok := WindowMonth.Duration(); ok {
			t.Error("month reported a duration")
		}
	})
}

// TestAnchoredBounds is spec 037's window rule: a duration window starts
// at the anchor plus whole periods, before the anchor as after it; month
// starts on the anchor's day and time of day, on the month's last day
// when the month is shorter; a zero anchor and none are Bounds.
func TestAnchoredBounds(t *testing.T) {
	d := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	created := d("2026-01-05T08:00:00Z")
	monday := d("2026-09-14T00:00:00Z")
	cases := []struct {
		name          string
		w             Window
		at, anchor    string
		start, resets string
	}{
		{"a week from a Monday, inside the anchored week", "168h", "2026-09-17T12:00:00Z", "2026-09-14T00:00:00Z", "2026-09-14T00:00:00Z", "2026-09-21T00:00:00Z"},
		{"a week from a Monday, weeks later", "168h", "2026-10-05T00:00:00Z", "2026-09-14T00:00:00Z", "2026-10-05T00:00:00Z", "2026-10-12T00:00:00Z"},
		{"a week from a Monday, before the anchor", "168h", "2026-09-10T00:00:00Z", "2026-09-14T00:00:00Z", "2026-09-07T00:00:00Z", "2026-09-14T00:00:00Z"},
		{"every ten days from a date", "240h", "2026-10-25T00:00:00Z", "2026-10-01T00:00:00Z", "2026-10-21T00:00:00Z", "2026-10-31T00:00:00Z"},
		{"a month from the 15th at noon", "month", "2026-09-20T00:00:00Z", "2026-01-15T12:00:00Z", "2026-09-15T12:00:00Z", "2026-10-15T12:00:00Z"},
		{"a month from the 15th at noon, before this month's", "month", "2026-09-15T11:59:59Z", "2026-01-15T12:00:00Z", "2026-08-15T12:00:00Z", "2026-09-15T12:00:00Z"},
		{"a month from the 31st, in February", "month", "2027-02-28T10:00:00Z", "2026-01-31T00:00:00Z", "2027-02-28T00:00:00Z", "2027-03-31T00:00:00Z"},
		{"a month from the 31st, before February's", "month", "2027-02-27T10:00:00Z", "2026-01-31T00:00:00Z", "2027-01-31T00:00:00Z", "2027-02-28T00:00:00Z"},
		{"a month from the 31st, in a leap February", "month", "2028-02-29T00:00:00Z", "2026-01-31T00:00:00Z", "2028-02-29T00:00:00Z", "2028-03-31T00:00:00Z"},
		{"a month from the 30th across a year", "month", "2027-01-02T00:00:00Z", "2026-01-30T00:00:00Z", "2026-12-30T00:00:00Z", "2027-01-30T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, resets := tc.w.AnchoredBounds(d(tc.at), created, d(tc.anchor))
			if !start.Equal(d(tc.start)) || !resets.Equal(d(tc.resets)) {
				t.Fatalf("AnchoredBounds = %s, %s, want %s, %s", start.Format(time.RFC3339), resets.Format(time.RFC3339), tc.start, tc.resets)
			}
		})
	}
	at := d("2026-09-17T12:00:00Z")
	for _, w := range []Window{"168h", WindowMonth, WindowNone} {
		s1, r1 := w.AnchoredBounds(at, created, time.Time{})
		s2, r2 := w.Bounds(at, created)
		if !s1.Equal(s2) || !r1.Equal(r2) {
			t.Errorf("%s with no anchor = %s, %s, want Bounds %s, %s", w, s1, r1, s2, r2)
		}
	}
	if s, r := WindowNone.AnchoredBounds(at, created, monday); !s.Equal(created) || !r.IsZero() {
		t.Errorf("none with an anchor = %s, %s", s, r)
	}
	if s, r := Window("bad").AnchoredBounds(at, created, monday); !s.IsZero() || !r.IsZero() {
		t.Errorf("an invalid window with an anchor = %s, %s", s, r)
	}
}
