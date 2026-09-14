// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// stubLookup answers from fixed sets. A name in refused answers its error.
type stubLookup struct {
	providers map[string]*v1.Provider
	budgets   map[string]*v1.Budget
	models    []string
	refused   map[string]error
}

func (s *stubLookup) Provider(_ context.Context, name string) (*v1.Provider, error) {
	if err, ok := s.refused[name]; ok {
		return nil, err
	}
	p, ok := s.providers[name]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

func (s *stubLookup) Budget(_ context.Context, name string) (*v1.Budget, error) {
	if err, ok := s.refused[name]; ok {
		return nil, err
	}
	b, ok := s.budgets[name]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func (s *stubLookup) Models(_ context.Context, selector string) ([]v1.ModelRef, error) {
	if err, ok := s.refused[selector]; ok {
		return nil, err
	}
	var refs []v1.ModelRef
	for _, m := range s.models {
		if Match(selector, m) {
			refs = append(refs, v1.ModelRef{ID: "mdl_" + m, Name: m, Owner: "https://login.example.com|alice"})
		}
	}
	return refs, nil
}

// corpusOptions reads options.json and builds the fixed Options, with a
// Lookup answering from the accepted corpus.
func corpusOptions(t *testing.T) Options {
	t.Helper()
	var f struct {
		Now      time.Time `json:"now"`
		NewName  string    `json:"newName"`
		Defaults struct {
			RequestsPerMinute int         `json:"requestsPerMinute"`
			TokensPerMinute   int         `json:"tokensPerMinute"`
			Timeout           v1.Duration `json:"timeout"`
		} `json:"defaults"`
		Limits struct {
			MaxRequestsPerMinute int         `json:"maxRequestsPerMinute"`
			MaxTokensPerMinute   int         `json:"maxTokensPerMinute"`
			MaxSpend             v1.Money    `json:"maxSpend"`
			MaxTTL               v1.Duration `json:"maxTTL"`
		} `json:"limits"`
		FileMode              bool   `json:"fileMode"`
		AllowPrivateUpstreams bool   `json:"allowPrivateUpstreams"`
		TunnelEnabled         bool   `json:"tunnelEnabled"`
		PublicURL             string `json:"publicURL"`
	}
	if err := json.Unmarshal(corpusFile(t, "options.json"), &f); err != nil {
		t.Fatal(err)
	}
	timeout, err := f.Defaults.Timeout.Parse()
	if err != nil {
		t.Fatal(err)
	}
	maxTTL, err := f.Limits.MaxTTL.Parse()
	if err != nil {
		t.Fatal(err)
	}
	public, err := url.Parse(f.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Actor:                 Actor{Subject: "https://login.example.com|alice"},
		Lookup:                corpusLookup(t),
		Defaults:              Defaults{RequestsPerMinute: f.Defaults.RequestsPerMinute, TokensPerMinute: f.Defaults.TokensPerMinute, Timeout: timeout},
		Limits:                Limits{MaxRequestsPerMinute: f.Limits.MaxRequestsPerMinute, MaxTokensPerMinute: f.Limits.MaxTokensPerMinute, MaxSpend: f.Limits.MaxSpend, MaxTTL: maxTTL},
		FileMode:              f.FileMode,
		AllowPrivateUpstreams: f.AllowPrivateUpstreams,
		TunnelEnabled:         f.TunnelEnabled,
		PublicURL:             public,
		Now:                   func() time.Time { return f.Now },
		NewName:               func() string { return f.NewName },
	}
}

// corpusLookup decodes every accepted case and answers from their names.
func corpusLookup(t *testing.T) *stubLookup {
	t.Helper()
	s := &stubLookup{providers: map[string]*v1.Provider{}, budgets: map[string]*v1.Budget{}}
	for _, name := range corpusCases(t, "accepted") {
		obj, err := Decode(corpusFile(t, name), MediaYAML, Hint{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		switch o := obj.(type) {
		case *v1.Provider:
			s.providers[o.Metadata.Name] = o
		case *v1.Budget:
			s.budgets[o.Metadata.Name] = o
		case *v1.Model:
			s.models = append(s.models, o.Metadata.Name)
		}
	}
	return s
}

// corpusCases lists the .yaml files under one corpus directory.
func corpusCases(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(Corpus, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// goldenName is the golden file beside a case.
func goldenName(caseFile string) string {
	return strings.TrimSuffix(caseFile, path.Ext(caseFile)) + ".golden.json"
}

// mustDecode decodes a YAML body or fails the test.
func mustDecode(t *testing.T, body string) v1.Object {
	t.Helper()
	obj, err := Decode([]byte(body), MediaYAML, Hint{})
	if err != nil {
		t.Fatalf("Decode: %v\n%s", err, body)
	}
	return obj
}

// resolveErr decodes and resolves and returns the *Error of whichever
// refused, failing on any other outcome.
func resolveErr(t *testing.T, body string, o Options) *Error {
	t.Helper()
	obj, err := Decode([]byte(body), MediaYAML, Hint{})
	if err == nil {
		_, err = Resolve(context.Background(), obj, o)
	}
	if err == nil {
		t.Fatalf("Resolve accepted:\n%s", body)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("Resolve returned %T %v, not *Error", err, err)
	}
	if e.Message != e.Code.Message() {
		t.Errorf("%s carries message %q, want %q", e.Code, e.Message, e.Code.Message())
	}
	return e
}

// mustResolve resolves under o or fails the test.
func mustResolve(t *testing.T, body string, o Options) *Resolved {
	t.Helper()
	r, err := Resolve(context.Background(), mustDecode(t, body), o)
	if err != nil {
		t.Fatalf("Resolve: %v\n%s", err, body)
	}
	return r
}

// head is the envelope of a manifest body of one kind and name.
func head(kind, name string) string {
	return "apiVersion: lux.latere.ai/v1beta1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\n"
}

// Minimal accepted bodies per kind under corpusOptions.
const (
	minProvider = "spec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n"
	minModel    = "spec:\n  targets:\n    - provider: openai\n"
	minKey      = "spec:\n  models: [gpt-5]\n"
	minBudget   = "spec:\n  amount: \"10\"\n"
)
