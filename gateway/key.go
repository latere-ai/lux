// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// credential reads the Key's value off a request, from the first of these
// that is present: Authorization: Bearer, x-api-key, x-goog-api-key, the
// query parameter key. Every door accepts every form. The value is
// returned as the exact bytes presented, trimmed of nothing, because the
// hash is over what a Key was created with.
func credential(r *http.Request) (string, bool) {
	if a := r.Header.Get("Authorization"); a != "" {
		if scheme, rest, ok := strings.Cut(a, " "); ok && strings.EqualFold(scheme, "Bearer") && rest != "" {
			return rest, true
		}
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return v, true
	}
	if v := r.Header.Get("x-goog-api-key"); v != "" {
		return v, true
	}
	if v := r.URL.Query().Get("key"); v != "" {
		return v, true
	}
	return "", false
}

// hashValue is the lookup key of a presented value: SHA-256, 64 lower-case
// hex characters, the same function the API stores under.
func hashValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// keyState decides the Key's state from the facts, not from the cached
// status.state word: Disabled from the spec, Expired from the clock.
// Exhausted is stage 7's, from the current window.
func keyState(k *v1.Key, now time.Time) *failure {
	if k.Spec.Disabled {
		return fail(CodeKeyDisabled, "Key "+k.Status.ID+" has spec.disabled true")
	}
	if !k.Status.ExpiresAt.IsZero() && !now.Before(k.Status.ExpiresAt) {
		return fail(CodeKeyExpired, "Key "+k.Status.ID+" expired at "+k.Status.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// allowed reports whether one of the Key's selectors matches the Model's
// name under manifest.Match, the function that validated the selector.
func allowed(k *v1.Key, name string) bool {
	for _, s := range k.Spec.Models {
		if manifest.Match(s, name) {
			return true
		}
	}
	return false
}

// The bounds of a Lux-Labels header of spec 007.
const (
	maxRequestLabels    = 8
	maxLabelKeyLength   = 64
	maxLabelValueLength = 128
)

// requestLabels parses the Lux-Labels header: comma separated k=v pairs,
// at most eight, each key 1 to 64 characters of [A-Za-z0-9._-] and each
// value 1 to 128 of [A-Za-z0-9._:/-], no duplicate keys. A pair that
// fails the rule, a duplicate, and every pair past the eighth are
// dropped and the rest kept, because attribution is not worth a refusal.
func requestLabels(header string) map[string]string {
	if header == "" {
		return nil
	}
	out := map[string]string{}
	for pair := range strings.SplitSeq(header, ",") {
		if len(out) == maxRequestLabels {
			break
		}
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || !labelKey(k) || !labelValue(v) {
			continue
		}
		if _, dup := out[k]; dup {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func labelKey(s string) bool {
	if s == "" || len(s) > maxLabelKeyLength {
		return false
	}
	for i := range len(s) {
		if !labelByte(s[i], false) {
			return false
		}
	}
	return true
}

func labelValue(s string) bool {
	if s == "" || len(s) > maxLabelValueLength {
		return false
	}
	for i := range len(s) {
		if !labelByte(s[i], true) {
			return false
		}
	}
	return true
}

// labelByte is the alphabet of a label key, and with value set, of a
// value, which adds : and /.
func labelByte(c byte, value bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		return true
	case value && (c == ':' || c == '/'):
		return true
	}
	return false
}
