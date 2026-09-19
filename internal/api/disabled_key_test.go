// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestDisabledKeyCanWaitForModelAssignment(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, name := range []string{"minted", "supplied"} {
		spec := `{"spec":{"disabled":true}}`
		if name == "supplied" {
			spec = `{"spec":{"disabled":true,"valueSHA256":"` + strings.Repeat("ab", 32) + `"}}`
		}
		r := h.request(http.MethodPut, "/v1/keys/"+name, spec)
		if r.Code != http.StatusCreated || status(t, r)["state"] != "Disabled" {
			t.Fatal(r.Code, r.Body)
		}
		r = h.request(http.MethodPut, "/v1/keys/"+name, `{"spec":{"disabled":true}}`)
		if r.Code != http.StatusOK {
			t.Fatal(r.Body)
		}
		wantCode(t, h.request(http.MethodPut, "/v1/keys/"+name, `{"spec":{"disabled":false}}`), CodeMissingField)
		r = h.request(http.MethodPut, "/v1/keys/"+name, `{"spec":{"disabled":false,"models":["gpt-5"]}}`)
		if r.Code != http.StatusOK || status(t, r)["state"] != "Active" {
			t.Fatal(r.Code, r.Body)
		}
	}
}
