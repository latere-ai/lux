// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Money is an amount of a currency as an integer count of micro-units, so
// no float touches a price at any point: "1.25" is 1250000. A manifest
// writes it as a decimal string of at most MoneyIntegerDigits integer
// digits and MoneyFractionDigits fraction digits, and it renders back as
// the shortest string that names the same amount.
type Money int64

// The bounds of a money string. Twelve integer digits keep every amount
// inside an int64 of micro-units with headroom for the arithmetic of
// spec 009, which multiplies a price by a token count.
const (
	MoneyIntegerDigits  = 12
	MoneyFractionDigits = 6
)

// microPerUnit is the scale of Money.
const microPerUnit = 1_000_000

// ParseMoney reads a money string: digits, an optional point, and one to
// six more digits. Nothing else is accepted: no sign, no exponent, no
// thousands separator, no currency symbol.
func ParseMoney(s string) (Money, error) {
	whole, frac, hasPoint := strings.Cut(s, ".")
	if whole == "" || !digits(whole) {
		return 0, fmt.Errorf("money %q: not a decimal string of digits", s)
	}
	if len(whole) > MoneyIntegerDigits {
		return 0, fmt.Errorf("money %q: %d integer digits, at most %d", s, len(whole), MoneyIntegerDigits)
	}
	if hasPoint && (frac == "" || !digits(frac)) {
		return 0, fmt.Errorf("money %q: the fraction is not one to %d digits", s, MoneyFractionDigits)
	}
	if len(frac) > MoneyFractionDigits {
		return 0, fmt.Errorf("money %q: %d fraction digits, at most %d", s, len(frac), MoneyFractionDigits)
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money %q: %w", s, err)
	}
	frac += strings.Repeat("0", MoneyFractionDigits-len(frac))
	micro, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money %q: %w", s, err)
	}
	return Money(units*microPerUnit + micro), nil
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// String renders the amount as the shortest money string: the integer
// part, then the fraction with its trailing zeros dropped, or no fraction
// at all when it is zero.
func (m Money) String() string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	units, micro := int64(m)/microPerUnit, int64(m)%microPerUnit
	if micro == 0 {
		return sign + strconv.FormatInt(units, 10)
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", micro), "0")
	return sign + strconv.FormatInt(units, 10) + "." + frac
}

// MarshalJSON writes the money string.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON reads a money string and refuses anything else, a JSON
// number included, because a float has already lost what the string keeps.
func (m *Money) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("money: %s is not a string: %w", string(b), err)
	}
	v, err := ParseMoney(s)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// Duration is a Go duration in the text the caller wrote, "10m" or "168h",
// so a resolved manifest reads back with the spelling it was given. Parse
// gives the value; DurationOf renders one the operator's configuration
// supplied.
type Duration string

// Parse reads the duration in Go syntax.
func (d Duration) Parse() (time.Duration, error) {
	if d == "" {
		return 0, errors.New("duration: empty")
	}
	return time.ParseDuration(string(d))
}

// DurationOf renders d in Go syntax with the zero units time.Duration's
// own String adds dropped, so ten minutes is "10m" and not "10m0s".
func DurationOf(d time.Duration) Duration {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return Duration(s)
}

// Window is a spend window: a Go duration of at least WindowMin and at
// most WindowMax, the word month, or the word none. A duration window is
// fixed, not rolling, so every replica agrees on its boundary from the
// clock alone; month is the calendar month in UTC; none never resets.
type Window string

// The two named windows.
const (
	WindowMonth Window = "month"
	WindowNone  Window = "none"
)

// The bounds of a duration window.
const (
	WindowMin = time.Minute
	WindowMax = 8760 * time.Hour
)

// Validate reports why w is not a window, or nil.
func (w Window) Validate() error {
	switch w {
	case WindowMonth, WindowNone:
		return nil
	case "":
		return errors.New("window: empty")
	}
	d, err := time.ParseDuration(string(w))
	if err != nil {
		return fmt.Errorf("window %q: not a Go duration, month, or none", string(w))
	}
	if d < WindowMin || d > WindowMax {
		return fmt.Errorf("window %q: a duration window is between %s and %s", string(w), DurationOf(WindowMin), DurationOf(WindowMax))
	}
	return nil
}

// Duration returns the length of a duration window and false for month,
// none, and anything Validate refuses.
func (w Window) Duration() (time.Duration, bool) {
	if w.Validate() != nil || w == WindowMonth || w == WindowNone {
		return 0, false
	}
	d, _ := time.ParseDuration(string(w))
	return d, true
}

// Bounds returns the window containing at: its start and the instant it
// resets. A duration window is aligned to the Unix epoch in UTC; month
// starts on the first of at's month; none starts at createdAt and never
// resets, which a zero resetsAt says. An invalid window has no bounds.
func (w Window) Bounds(at, createdAt time.Time) (start, resetsAt time.Time) {
	switch w {
	case WindowNone:
		return createdAt.UTC(), time.Time{}
	case WindowMonth:
		y, m, _ := at.UTC().Date()
		start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
	d, ok := w.Duration()
	if !ok {
		return time.Time{}, time.Time{}
	}
	n := at.UnixNano() / int64(d)
	start = time.Unix(0, n*int64(d)).UTC()
	return start, start.Add(d)
}
