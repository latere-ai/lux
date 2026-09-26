// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// wellKnownAt is a discovery document whose doors are public plus each
// dialect and whose control plane is api.
func wellKnownAt(public, api string) string {
	return `{"name":"lux","api":"` + api + `","openapi":"` + api + `/openapi.json","doors":{` +
		`"openai":"` + public + `/openai","anthropic":"` + public + `/anthropic",` +
		`"gemini":"` + public + `/gemini","lux":"` + public + `/lux"}}`
}

// TestDiscoverReadsTheMount is spec 040's client: under a base path in
// the place of /v1 the document names the public URL as the control
// plane and Discover sets BaseReplacesV1, after which a control plane
// path is sent without its /v1 and the documents and doors as written;
// under a prefixed base path it leaves the flag clear; at the root it
// sends nothing; and a document of neither shape is refused. The public
// URL in the document is never where a request goes: the client keeps
// the address it was given.
func TestDiscoverReadsTheMount(t *testing.T) {
	const base = "/v1/models"
	const public = "https://api.example.com" + base

	t.Run("replace", func(t *testing.T) {
		f := newFake(t)
		f.on(http.MethodGet, base+"/.well-known/lux", 200, wellKnownAt(public, public))
		for _, p := range []string{base + "/keys/run-42", base + "/models/openai/gpt-5", base + "/self", base + "/lux/v1/models"} {
			f.on(http.MethodGet, p, 200, `{}`)
		}
		c := f.client()
		c.BaseURL = f.srv.URL + base + "/"
		if err := c.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !c.BaseReplacesV1 {
			t.Fatal("BaseReplacesV1 is clear under a base path in the place of /v1")
		}
		if got := f.last(); got.Path != base+"/.well-known/lux" || got.Auth != "" {
			t.Fatalf("discovery sent %+v", got)
		}
		for _, tc := range []struct {
			call func() (*Response, error)
			path string
		}{
			{func() (*Response, error) { return c.Get(t.Context(), "keys", "run-42") }, base + "/keys/run-42"},
			{func() (*Response, error) { return c.Get(t.Context(), "models", "openai/gpt-5") }, base + "/models/openai/gpt-5"},
			{func() (*Response, error) { return c.Self(t.Context()) }, base + "/self"},
			{func() (*Response, error) { return c.Models(t.Context()) }, base + "/lux/v1/models"},
			{func() (*Response, error) { return c.WellKnown(t.Context()) }, base + "/.well-known/lux"},
		} {
			if _, err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.path, err)
			}
			if got := f.last(); got.Path != tc.path {
				t.Errorf("sent to %s, want %s", got.Path, tc.path)
			}
		}
	})

	t.Run("prefix", func(t *testing.T) {
		f := newFake(t)
		f.on(http.MethodGet, base+"/.well-known/lux", 200, wellKnownAt(public, public+"/v1"))
		f.on(http.MethodGet, base+"/v1/self", 200, `{}`)
		c := f.client()
		c.BaseURL = f.srv.URL + base
		c.BaseReplacesV1 = true
		if err := c.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if c.BaseReplacesV1 {
			t.Fatal("BaseReplacesV1 is set under a prefixed base path")
		}
		if _, err := c.Self(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := f.last(); got.Path != base+"/v1/self" {
			t.Errorf("sent to %s, want %s/v1/self", got.Path, base)
		}
	})

	t.Run("the root sends nothing", func(t *testing.T) {
		f := newFake(t)
		c := f.client()
		c.BaseReplacesV1 = true
		if err := c.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if c.BaseReplacesV1 || len(f.got) != 0 {
			t.Fatalf("BaseReplacesV1 %v after %d requests at the root", c.BaseReplacesV1, len(f.got))
		}
	})

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a control plane elsewhere", wellKnownAt(public, "https://other.example.com/v1"), `the control plane "https://other.example.com/v1" is neither`},
		{"no doors", `{"api":"` + public + `","doors":{}}`, "the document names no door"},
		{"doors at two addresses", `{"api":"` + public + `","doors":{"openai":"` + public + `/openai","lux":"https://other.example.com/lux"}}`, "the doors extend two addresses, https://other.example.com and " + public},
		{"a door without its dialect", `{"api":"` + public + `","doors":{"openai":"` + public + `/chat"}}`, `the openai door "` + public + `/chat" is not an address plus /openai`},
		{"not JSON", `<html>`, "with a body this client does not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.on(http.MethodGet, base+"/.well-known/lux", 200, tc.body)
			c := f.client()
			c.BaseURL = f.srv.URL + base
			err := c.Discover(t.Context())
			var unreadable *UnreadableError
			if !errors.As(err, &unreadable) || !strings.Contains(err.Error(), tc.want) || c.BaseReplacesV1 {
				t.Fatalf("Discover() = %v with BaseReplacesV1 %v, want an unreadable document holding %q", err, c.BaseReplacesV1, tc.want)
			}
		})
	}

	t.Run("a refused discovery", func(t *testing.T) {
		f := newFake(t)
		c := f.client()
		c.BaseURL = f.srv.URL + base
		var refusal *Error
		if err := c.Discover(t.Context()); !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
			t.Fatalf("Discover() = %v, want the not_found the server answered", err)
		}
	})

	t.Run("a base URL that does not parse", func(t *testing.T) {
		c := &Client{BaseURL: "http://[::1/x"}
		var transport *TransportError
		if err := c.Discover(t.Context()); !errors.As(err, &transport) {
			t.Fatalf("Discover() = %v, want a transport error", err)
		}
	})
}
