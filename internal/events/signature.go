// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header is the header every delivery carries.
const Header = "Lux-Signature"

// ErrSignature is a header that does not verify; the wrapping error
// carries the developer's detail.
var ErrSignature = errors.New("the signature does not verify")

// Sign renders the header for body at t: t=<unix seconds>,v1=<hex
// HMAC-SHA256 of "<t>.<body>" under secret>. The clock is the attempt's,
// so a retry is re-signed.
func Sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(digest(secret, ts, body))
}

// Parse reads the two members of a header.
func Parse(header string) (t time.Time, v1 string, err error) {
	var ts string
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return time.Time{}, "", fmt.Errorf("%w: %q is not key=value", ErrSignature, part)
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	if ts == "" || v1 == "" {
		return time.Time{}, "", fmt.Errorf("%w: header %q lacks t or v1", ErrSignature, header)
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: t %q is not unix seconds", ErrSignature, ts)
	}
	return time.Unix(unix, 0).UTC(), v1, nil
}

// Verify checks header over body under secret, the way a sink should:
// the digest compared in constant time, and t within skew of now when
// skew is positive. It is what the stub sink and the tests use, and a
// reference for a sink author.
func Verify(secret []byte, header string, body []byte, now time.Time, skew time.Duration) error {
	t, v1, err := Parse(header)
	if err != nil {
		return err
	}
	if skew > 0 {
		if d := now.Sub(t); d > skew || d < -skew {
			return fmt.Errorf("%w: t %s is %s from the clock %s, over %s", ErrSignature, t.Format(time.RFC3339), d, now.UTC().Format(time.RFC3339), skew)
		}
	}
	got, err := hex.DecodeString(v1)
	if err != nil {
		return fmt.Errorf("%w: v1 is not hex", ErrSignature)
	}
	if !hmac.Equal(got, digest(secret, strconv.FormatInt(t.Unix(), 10), body)) {
		return fmt.Errorf("%w: v1 does not match the body under the secret", ErrSignature)
	}
	return nil
}

// digest is HMAC-SHA256 over "<t>.<body>".
func digest(secret []byte, ts string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write([]byte(ts))
	_, _ = h.Write([]byte("."))
	_, _ = h.Write(body)
	return h.Sum(nil)
}
