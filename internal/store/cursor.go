// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// EncodeCursor renders the cursor that resumes a list of kind under f
// after last: base64url, without padding, of "<kind>|<8 hex of SHA-256
// over the filter's canonical JSON>|<last>". Every implementation uses
// this one encoding, so a cursor minted by one store is refused by
// another only for the reason the contract names.
func EncodeCursor(kind string, f Filter, last string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + f.digest() + "|" + last))
}

// DecodeCursor checks cursor against the kind and filter of the request
// and returns the value to resume after. A cursor that does not decode,
// or whose kind or filter differs, is ErrInvalidCursor with the detail.
func DecodeCursor(cursor, kind string, f Filter) (last string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("%w: not base64url: %w", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: %d fields, want kind, filter, and last", ErrInvalidCursor, len(parts))
	}
	if parts[0] != kind {
		return "", fmt.Errorf("%w: cursor is over %s, the request over %s", ErrInvalidCursor, parts[0], kind)
	}
	if parts[1] != f.digest() {
		return "", fmt.Errorf("%w: cursor filter %s, the request's %s", ErrInvalidCursor, parts[1], f.digest())
	}
	return parts[2], nil
}

// digest is the 8 hex characters of SHA-256 over the canonical JSON.
func (f Filter) digest() string {
	sum := sha256.Sum256(f.canonical())
	return hex.EncodeToString(sum[:4])
}

// canonical is the filter as JSON with one spelling: members in a fixed
// order, absent when zero, labels by sorted key, ids sorted, so two
// filters that select the same rows encode the same bytes.
func (f Filter) canonical() []byte {
	ids := slices.Clone(f.IDs)
	slices.Sort(ids)
	data, err := json.Marshal(struct {
		Owner    string            `json:"owner,omitempty"`
		Labels   map[string]string `json:"labels,omitempty"`
		Source   string            `json:"source,omitempty"`
		Provider string            `json:"provider,omitempty"`
		IDs      []string          `json:"ids,omitempty"`
	}{f.Owner, f.Labels, f.Source, f.Provider, ids})
	if err != nil {
		panic("store: a Filter of strings does not encode: " + err.Error())
	}
	return data
}
