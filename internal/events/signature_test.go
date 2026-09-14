// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSignature is the documented formula: the header is t=<unix
// seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">, computed here by hand
// and by Sign alike; it verifies under the secret, and a body changed
// by one byte, a wrong secret, a malformed header, a t outside the
// sink's skew, and a v1 that is not hex do not.
func TestSignature(t *testing.T) {
	at := time.Date(2026, 9, 14, 10, 41, 0, 500_000_000, time.UTC)
	body := []byte(`{"id":"evt_1","type":"key.created"}`)
	ts := strconv.FormatInt(at.Unix(), 10)
	header := Sign(secret, at, body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "." + string(body)))
	if want := "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil)); header != want {
		t.Fatalf("Sign = %s, want %s", header, want)
	}
	if err := Verify(secret, header, body, at, 5*time.Minute); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := Verify(secret, header, body, time.Time{}, 0); err != nil {
		t.Fatalf("Verify without a clock: %v", err)
	}
	if got, v1, err := Parse(header); err != nil || !got.Equal(at.Truncate(time.Second)) || len(v1) != 64 {
		t.Fatalf("Parse = %s %s %v", got, v1, err)
	}
	changed := append([]byte(nil), body...)
	changed[len(changed)-2] ^= 1
	for name, tc := range map[string]struct {
		secret []byte
		header string
		body   []byte
		now    time.Time
		want   string
	}{
		"one byte changed": {secret, header, changed, at, "does not match"},
		"a wrong secret":   {[]byte("another"), header, body, at, "does not match"},
		"no v1":            {secret, "t=" + ts, body, at, "lacks t or v1"},
		"no equals":        {secret, "t;" + ts, body, at, "is not key=value"},
		"t not a number":   {secret, "t=now,v1=00", body, at, "is not unix seconds"},
		"v1 not hex":       {secret, "t=" + ts + ",v1=zz", body, at, "is not hex"},
		"t too old":        {secret, header, body, at.Add(6 * time.Minute), "over 5m0s"},
		"t in the future":  {secret, header, body, at.Add(-6 * time.Minute), "over 5m0s"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(tc.secret, tc.header, tc.body, tc.now, 5*time.Minute)
			if err == nil || !errors.Is(err, ErrSignature) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify = %v, want ErrSignature with %q", err, tc.want)
			}
		})
	}
}
