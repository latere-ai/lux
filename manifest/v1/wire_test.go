// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

// TestObjectBytesAreStable: an object with several map members encodes to
// the same bytes on every call, members in key order, whatever options
// the caller passes, so a stored or hashed manifest does not move.
func TestObjectBytesAreStable(t *testing.T) {
	p := &Provider{
		Metadata: ObjectMeta{Name: "p", Labels: map[string]string{"c": "3", "a": "1", "b": "2"}},
		Spec: ProviderSpec{
			Dialect:       "openai",
			Headers:       map[string]string{"X-C": "3", "X-A": "1", "X-B": "2"},
			RequestFields: map[string]any{"z": 1, "m": map[string]any{"y": true, "x": false}, "a": "<&>"},
		},
	}
	first, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		again, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("the object moved:\n%s\n%s", first, again)
		}
	}
	for _, want := range []string{
		`"labels":{"a":"1","b":"2","c":"3"}`,
		`"headers":{"X-A":"1","X-B":"2","X-C":"3"}`,
		`"requestFields":{"a":"<&>","m":{"x":false,"y":true},"z":1}`,
		`"apiVersion":"lux.latere.ai/v1beta1","kind":"Provider"`,
	} {
		if !strings.Contains(string(first), want) {
			t.Errorf("%s lacks %s", first, want)
		}
	}
}

// TestOmittedMembers: a zero pointer, number, bool or timestamp is left
// out of a Key as before, and a pointer to zero is written.
func TestOmittedMembers(t *testing.T) {
	k := &Key{Metadata: ObjectMeta{Name: "k"}}
	out, err := json.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"valueFrom", "zeroRetention", "requestsPerMinute", "expiresAt", "\"version\""} {
		if strings.Contains(string(out), absent) {
			t.Errorf("%s carries %s", out, absent)
		}
	}
	zero := 0
	k.Spec.Limits.RequestsPerMinute = &zero
	out, err = json.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"requestsPerMinute":0`) {
		t.Errorf("%s drops a limit set to zero", out)
	}
}
