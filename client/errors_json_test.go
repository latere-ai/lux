// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import "testing"

// TestDecodeErrorIsLenient: a refusal that passed through a proxy is read
// with a repeated member, another letter case and invalid UTF-8.
func TestDecodeErrorIsLenient(t *testing.T) {
	body := []byte("{\"Error\":{\"code\":\"a\",\"code\":\"b\",\"message\":\"m\xff\"}}")
	e, ok := decodeError(400, body, "req", "")
	if !ok || e.Code != "b" || e.Message != "m�" {
		t.Fatalf("decodeError = %+v, %v", e, ok)
	}
}
