// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	id := NewID(PrefixProvider, now, nil)
	if !strings.HasPrefix(id, "prv_") || len(id) != 4+IDLength {
		t.Fatalf("NewID = %q, want prv_ and %d characters", id, IDLength)
	}
	for _, c := range id[4:] {
		if !strings.ContainsRune(crockford, c) {
			t.Errorf("%q carries %q, outside Crockford base32", id, c)
		}
	}
	t.Run("sorts by the time given", func(t *testing.T) {
		var ids []string
		for i := range 100 {
			ids = append(ids, NewID(PrefixKey, now.Add(time.Duration(i)*time.Millisecond), nil))
		}
		if !slices.IsSorted(ids) {
			t.Error("ids minted at increasing instants do not sort")
		}
		earlier := NewID(PrefixKey, now.Add(-time.Hour), nil)
		if earlier >= ids[0] {
			t.Errorf("an earlier id %q does not sort before %q", earlier, ids[0])
		}
	})
	t.Run("ten thousand ids at one instant are distinct", func(t *testing.T) {
		seen := make(map[string]bool, 10000)
		for range 10000 {
			id := NewID(PrefixModel, now, nil)
			if seen[id] {
				t.Fatalf("%q minted twice", id)
			}
			seen[id] = true
		}
	})
	t.Run("the time is the first ten characters", func(t *testing.T) {
		// The ULID of the reference implementation for this millisecond.
		zero := NewID("", time.UnixMilli(0), strings.NewReader("\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"))
		if zero != "00000000000000000000000000" {
			t.Errorf("all-zero id = %q", zero)
		}
		max := NewID("", time.UnixMilli(0xFFFFFFFFFFFF), strings.NewReader("\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff"))
		if max != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
			t.Errorf("all-ones id = %q", max)
		}
	})
	t.Run("a failing random source panics", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("no panic")
			}
		}()
		NewID(PrefixEvent, now, failing{})
	})
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("closed") }
