// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestCredentialSources: the store source opens the sealed row and
// answers nil for a Provider without one; the file source hands back the
// variable's value and nil for a Provider that names none.
func TestCredentialSources(t *testing.T) {
	h := newHarness(t)
	p := h.provider(t, "openai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	got, err := h.creds.Credential(t.Context(), p.Status.ID)
	if err != nil || string(got) != canary {
		t.Fatalf("Credential() = %q, %v", got, err)
	}
	if got, err := h.creds.Credential(t.Context(), "prv_none"); err != nil || got != nil {
		t.Fatalf("Credential() of a Provider without one = %q, %v", got, err)
	}
	wrong := &StoreCredentials{Credentials: h.st.Credentials(), Keys: mustKeys(t, strings.ReplaceAll(kek, "AQ", "Ag"))}
	if _, err := wrong.Credential(t.Context(), p.Status.ID); err == nil || !strings.Contains(err.Error(), "no listed key opens") {
		t.Fatalf("Credential() under the wrong key: %v", err)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.creds.Credential(ended, p.Status.ID); err == nil || !strings.Contains(err.Error(), "credential of "+p.Status.ID) {
		t.Fatalf("Credential() over an ended context: %v", err)
	}

	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("with.yaml", "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: with\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n  credential:\n    valueFrom:\n      env: WITH_KEY\n")
	write("without.yaml", "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: without\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n")
	files, err := filemode.Load(t.Context(), filemode.Options{Dir: dir, Getenv: func(k string) string {
		if k == "WITH_KEY" {
			return canary
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	fc := &FileCredentials{Files: files}
	with, _, err := files.Objects().ByName(t.Context(), v1.KindProvider, "with")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fc.Credential(t.Context(), with.ID()); err != nil || string(got) != canary {
		t.Fatalf("file Credential() = %q, %v", got, err)
	}
	without, _, err := files.Objects().ByName(t.Context(), v1.KindProvider, "without")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fc.Credential(t.Context(), without.ID()); err != nil || got != nil {
		t.Fatalf("file Credential() of a Provider without one = %q, %v", got, err)
	}
}

// TestProviderCredentialNeverLeavesTheGateway is the package half of the
// row: a canary credential reaches the upstream's headers on the jobs'
// requests toward its Provider, and appears in no stored object, no
// encoding of one, no event payload, no status, and no log line. The
// half through a door and the stub provider is spec 015's.
func TestProviderCredentialNeverLeavesTheGateway(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5", "BAD")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	d, job := h.discovery("a"), h.health("a")
	d.acquire(t.Context())
	d.Tick(t.Context())
	job.acquire(t.Context())
	job.Tick(t.Context())
	up.set(500, `{"error":"`+canary+`"}`)
	for range 3 {
		job.Tick(t.Context())
	}
	d.Tick(t.Context())
	for _, r := range up.all() {
		if r.Header.Get("Authorization") != "Bearer "+canary {
			t.Fatalf("a request toward the Provider carried %q", r.Header.Get("Authorization"))
		}
	}
	if up.count() < 6 {
		t.Fatalf("%d requests", up.count())
	}
	var encodings []string
	for _, kind := range []string{v1.KindProvider, v1.KindModel} {
		objs, _, err := h.st.Objects().List(t.Context(), kind, store.Filter{}, store.Page{})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			b, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			encodings = append(encodings, string(b))
		}
	}
	for _, e := range h.events(t, "") {
		encodings = append(encodings, string(e.Payload))
	}
	encodings = append(encodings, h.logged())
	status, _ := json.Marshal(h.get(t, p.Status.ID).Status)
	encodings = append(encodings, string(status))
	if len(h.events(t, "")) == 0 || len(encodings) < 5 {
		t.Fatalf("too little to check: %d encodings", len(encodings))
	}
	for _, enc := range encodings {
		if strings.Contains(enc, canary) || strings.Contains(enc, canary[3:12]) {
			t.Fatalf("the credential appears in:\n%s", enc)
		}
	}
	// The upstream echoed the credential back in its 500 body; the
	// excerpt kept in status.health.lastError has it redacted.
	if got := h.get(t, p.Status.ID).Status.Health.LastError; !strings.Contains(got, "upstream status 500") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("lastError = %q", got)
	}
}

// TestUpstreamPaths is the models-route half of the row: each dialect's
// list is reached at the path and query the table names under the
// Provider's baseURL, by the jobs alone.
func TestUpstreamPaths(t *testing.T) {
	for _, tc := range []struct {
		dialect v1.Dialect
		base    string
		want    string
		page    string
		next    string
	}{
		{v1.DialectOpenAI, "https://api.example.com/v1", "https://api.example.com/v1/models", "", ""},
		{v1.DialectOpenAI, "https://api.example.com/v1/", "https://api.example.com/v1/models", "x", "https://api.example.com/v1/models"},
		{v1.DialectAnthropic, "https://api.example.com", "https://api.example.com/models?limit=1000", "m9", "https://api.example.com/models?after_id=m9&limit=1000"},
		{v1.DialectGemini, "https://api.example.com/v1beta", "https://api.example.com/v1beta/models?pageSize=1000", "t2", "https://api.example.com/v1beta/models?pageSize=1000&pageToken=t2"},
		{v1.DialectLux, "https://lux.example.com/lux/v1", "https://lux.example.com/lux/v1/models", "", ""},
	} {
		t.Run(string(tc.dialect)+" "+tc.base, func(t *testing.T) {
			p := &v1.Provider{Spec: v1.ProviderSpec{Dialect: tc.dialect, BaseURL: tc.base}}
			got, err := modelsURL(p, "")
			if err != nil || got != tc.want {
				t.Fatalf("modelsURL() = %q, %v; want %q", got, err, tc.want)
			}
			if tc.page != "" {
				next, err := modelsURL(p, tc.page)
				if err != nil || next != tc.next {
					t.Fatalf("modelsURL(page) = %q, %v; want %q", next, err, tc.next)
				}
			}
		})
	}
	if _, err := modelsURL(&v1.Provider{Spec: v1.ProviderSpec{BaseURL: "://nope"}}, ""); err == nil {
		t.Fatal("a bad baseURL")
	}
	if _, _, err := parseList(v1.DialectAnthropic, []byte(`{"data":[],"has_more":true}`)); err == nil {
		t.Fatal("has_more without last_id")
	}
	if _, _, err := parseList(v1.DialectAnthropic, []byte(`[`)); err == nil {
		t.Fatal("a broken anthropic page")
	}
	if _, _, err := parseList(v1.DialectGemini, []byte(`[`)); err == nil {
		t.Fatal("a broken gemini page")
	}
	if got := excerpt([]byte("a\n\n b   c" + strings.Repeat("x", 2000))); !strings.HasPrefix(got, "a b c") || len(got) > 1024 {
		t.Fatalf("excerpt = %q", got)
	}
	if timeoutOf(&v1.Provider{}) != defaultUpstreamTimeout || timeoutOf(&v1.Provider{Spec: v1.ProviderSpec{Timeout: "5s"}}) != 5e9 {
		t.Fatal("timeoutOf")
	}
}
