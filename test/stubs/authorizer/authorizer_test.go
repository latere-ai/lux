// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	vocabulary "latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
	v1 "latere.ai/x/lux/manifest/v1"
)

// client is the shared authorizer client luxd dials the stub with.
func client(t *testing.T, s *stub.Server, timeout time.Duration) *authz.Client {
	t.Helper()
	c, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func model(name string) *v1.Model {
	m := &v1.Model{}
	m.Metadata.Name = name
	m.Status.ID = "mdl_" + strings.ToUpper(strings.ReplaceAll(name, "-", "")) + strings.Repeat("0", 26-len(strings.ReplaceAll(name, "-", "")))
	return m
}

// TestAuthorizerStubNamesResourcesLuxsWay: a rule written as Kind/name
// matches the resource of spec 006's shapes before the object exists,
// through the client luxd uses, and a resource with no name is matched
// by id or by * alone.
func TestAuthorizerStubNamesResourcesLuxsWay(t *testing.T) {
	s := New(t)
	s.SetRules(stub.Rule{Action: vocabulary.ActionModelRead, Resource: "Model/gpt-4o", Allow: false, Reason: "named before it existed"})
	c := client(t, s, time.Second)
	caller := auth.Caller{Subject: "https://login.example.com|alice"}
	d, err := c.Authorize(t.Context(), auth.Request(caller, vocabulary.ActionModelRead, vocabulary.ModelObject(model("gpt-4o")), authz.Caller{}))
	if err != nil || d.Allow || d.Reason != "named before it existed" {
		t.Fatalf("the named rule did not match: %+v, %v", d, err)
	}
	d, err = c.Authorize(t.Context(), auth.Request(caller, vocabulary.ActionModelRead, vocabulary.ModelObject(model("gpt-4o-mini")), authz.Caller{}))
	if err != nil || !d.Allow {
		t.Fatalf("another name matched the rule: %+v, %v", d, err)
	}
	if got := Name(vocabulary.ModelList()); got != "" {
		t.Fatalf("Name of a list resource = %q", got)
	}
	if got := Name(vocabulary.KeyObject(&v1.Key{Metadata: v1.ObjectMeta{Name: "dev"}})); got != "Key/dev" {
		t.Fatalf("Name = %q", got)
	}
	if got := s.Requests(); len(got) != 2 || got[0].Action != vocabulary.ActionModelRead {
		t.Fatalf("requests = %+v", got)
	}
	// NewHandler carries the same naming, for the binary.
	h := NewHandler(stub.WithToken("other"))
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	h.SetRules(stub.Rule{Resource: "Model/gpt-4o", Allow: false, Reason: "by name"})
	if d := h.Decide(authz.Request{Subject: "s", Action: vocabulary.ActionModelRead, Resource: vocabulary.ModelObject(model("gpt-4o"))}); d.Allow {
		t.Fatalf("the handler's naming did not match: %+v", d)
	}
	if h.Token() != "other" {
		t.Fatalf("Token = %q", h.Token())
	}
}

// TestAuthorizerStubFlags: the two lux-stubs flags drive the table and
// the outage through the package's methods, every outage is
// authorizer_unavailable at luxd's client and none is an allow, and a
// mode outside the list is an error.
func TestAuthorizerStubFlags(t *testing.T) {
	s := New(t)
	c := client(t, s, 300*time.Millisecond)
	caller := auth.Caller{Subject: "https://login.example.com|alice"}
	ask := func() (authz.Decision, error) {
		return c.Authorize(t.Context(), auth.Request(caller, vocabulary.ActionKeyCreate, vocabulary.KeyCreate(&v1.Key{Metadata: v1.ObjectMeta{Name: "dev"}}), authz.Caller{}))
	}
	Deny(s, "")
	if d, err := ask(); err != nil || !d.Allow {
		t.Fatalf("Deny of nothing changed the table: %+v, %v", d, err)
	}
	Deny(s, vocabulary.ActionKeyCreate)
	if d, err := ask(); err != nil || d.Allow || !strings.Contains(d.Reason, "-authorizer-deny key.create") {
		t.Fatalf("Deny did not deny: %+v, %v", d, err)
	}
	s.SetRules()
	var unavailable *authz.Unavailable
	for _, mode := range []string{"503", string(stub.BodyMalformed), string(stub.BodyNoAllow), "hang"} {
		if err := Fail(s, mode); err != nil {
			t.Fatalf("Fail(%q): %v", mode, err)
		}
		d, err := ask()
		if !errors.As(err, &unavailable) || d.Allow {
			t.Fatalf("Fail(%q): decision %+v, err %v; want unavailable and no allow", mode, d, err)
		}
		s.Resume()
	}
	if d, err := ask(); err != nil || !d.Allow {
		t.Fatalf("after Resume: %+v, %v", d, err)
	}
	for _, mode := range []string{"bogus", "200", "99", "600", "5xx"} {
		if err := Fail(s, mode); err == nil || !strings.Contains(err.Error(), "-authorizer-fail") {
			t.Fatalf("Fail(%q) = %v, want an error naming the flag", mode, err)
		}
	}
	if err := Fail(s, ""); err != nil {
		t.Fatalf("Fail of nothing: %v", err)
	}
}

// TestAuthorizerStubDeniesTheProbe: whatever the table says, the probe id
// is denied, so the check every authorizer must pass passes against the
// stub through the same call luxd check's authorizer row makes.
func TestAuthorizerStubDeniesTheProbe(t *testing.T) {
	s := New(t)
	s.SetRules(stub.Rule{Subject: "*", Action: "*", Resource: "*", Allow: true})
	c := client(t, s, time.Second)
	if err := auth.NewAuthorizer(c).Check(t.Context()); err != nil {
		t.Fatalf("the probe was not denied: %v", err)
	}
	if err := authz.Check(t.Context(), c, vocabulary.ActionProviderRead, v1.KindProvider); err != nil {
		t.Fatalf("authz.Check: %v", err)
	}
}
