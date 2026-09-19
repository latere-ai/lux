// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

func TestCursorRoundTrips(t *testing.T) {
	f := Filter{Owner: "https://login.example.com|alice", Labels: map[string]string{"team": "research", "env": "prod"}, IDs: []string{"mdl_b", "mdl_a"}}
	c := EncodeCursor(v1.KindModel, f, "gpt-5")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		t.Fatalf("cursor is not base64url: %v", err)
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != v1.KindModel || len(parts[1]) != 8 || parts[2] != "gpt-5" {
		t.Fatalf("cursor decodes to %q, want kind, 8 hex, last", raw)
	}
	same := Filter{Owner: f.Owner, Labels: map[string]string{"env": "prod", "team": "research"}, IDs: []string{"mdl_a", "mdl_b"}}
	last, err := DecodeCursor(c, v1.KindModel, same)
	if err != nil || last != "gpt-5" {
		t.Fatalf("DecodeCursor = %q, %v", last, err)
	}
}

func TestCursorRefusesAnotherKindOrFilter(t *testing.T) {
	c := EncodeCursor(v1.KindModel, Filter{Source: "declared"}, "gpt-5")
	for name, tc := range map[string]struct {
		cursor, kind string
		f            Filter
		detail       string
	}{
		"kind":       {c, v1.KindKey, Filter{Source: "declared"}, "cursor is over Model"},
		"filter":     {c, v1.KindModel, Filter{Source: "discovered"}, "cursor filter"},
		"not base64": {"%%%", v1.KindModel, Filter{}, "not base64url"},
		"fields":     {base64.RawURLEncoding.EncodeToString([]byte("Model|abcd")), v1.KindModel, Filter{}, "2 fields"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeCursor(tc.cursor, tc.kind, tc.f)
			if !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("err = %v, want ErrInvalidCursor", err)
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("detail %q lacks %q", err, tc.detail)
			}
		})
	}
}

func TestCursorCarriesAnEmptyLast(t *testing.T) {
	last, err := DecodeCursor(EncodeCursor(JournalKind, Filter{IDs: []string{"key_1"}}, ""), JournalKind, Filter{IDs: []string{"key_1"}})
	if err != nil || last != "" {
		t.Fatalf("DecodeCursor = %q, %v", last, err)
	}
}

func TestErrorsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, err := range Errors() {
		if !strings.HasPrefix(err.Error(), "store: ") {
			t.Errorf("%v lacks the store: prefix", err)
		}
		if seen[err.Error()] {
			t.Errorf("%v is named twice", err)
		}
		seen[err.Error()] = true
	}
	if len(seen) != 9 {
		t.Fatalf("%d errors, want 9", len(seen))
	}
}
