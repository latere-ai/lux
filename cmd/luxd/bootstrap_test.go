// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/test/stubs/provider"
)

// bootstrapKey is a supplied Key value in the form the API accepts, read
// from the variable a bootstrapped Key names.
const bootstrapKey = "lux_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-"

// writeDir writes each named document into a fresh directory.
func writeDir(t *testing.T, docs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range docs {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestBootstrapAppliesTheDirectory is spec 035's criterion 10: at start
// serve applies every manifest of LUX_BOOTSTRAP_DIR, the Provider's
// credential and the Key's value read from the variables they name, each
// object owned by the first admin subject; the Key opens the door with
// the value from the environment, and the control plane stays writable.
func TestBootstrapAppliesTheDirectory(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	upstream := httptest.NewServer(provider.New(provider.Options{Dialect: v1.DialectOpenAI}))
	defer upstream.Close()
	dir := writeDir(t, map[string]string{
		"providers/openai.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: openai\nspec:\n  dialect: openai\n  baseURL: " + upstream.URL + "/v1\n  credential:\n    valueFrom:\n      env: OPENAI_API_KEY\n  discovery:\n    mode: none\n  health:\n    mode: none\n",
		"budgets/team.yaml":     "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: team\nspec:\n  amount: \"10\"\n  currency: USD\n",
		"models/gpt.yaml":       "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: gpt\nspec:\n  targets:\n    - provider: openai\n      model: gpt-4.1\n  pricing:\n    currency: USD\n    input: \"1\"\n    output: \"2\"\n",
		"keys/ci.yaml":          "apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: ci\nspec:\n  models: [\"gpt\"]\n  budget: team\n  valueFrom:\n    env: LUX_CI_KEY\n",
	})
	admin := iss.URL() + "|alice"
	srv := startServe(t, map[string]string{
		"LUX_OIDC_ISSUERS": iss.URL(), "LUX_ADMIN_SUBJECTS": admin, "LUX_BOOTSTRAP_DIR": dir,
		"LUX_UPSTREAM_ALLOW_PRIVATE": "1", "OPENAI_API_KEY": provider.DefaultCredential, "LUX_CI_KEY": bootstrapKey,
	})
	defer srv.stop()
	if !strings.Contains(srv.out.String(), "4 created, 0 updated, 0 unchanged") {
		t.Fatalf("the start-up lines do not count four created objects:\n%s", srv.out.String())
	}
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}
	for _, path := range []string{"/v1/providers/openai", "/v1/budgets/team", "/v1/models/gpt", "/v1/keys/ci"} {
		resp, body := do(t, http.MethodGet, srv.publicURL+path, "", bearer...)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, resp.StatusCode, body)
		}
		var obj struct {
			Status struct{ Owner string } `json:"status"`
		}
		if err := json.Unmarshal([]byte(body), &obj); err != nil || obj.Status.Owner != admin {
			t.Errorf("GET %s: owner %q, want %q (%v)", path, obj.Status.Owner, admin, err)
		}
	}
	resp, body := do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model": "gpt", "messages": [{"role": "user", "content": "hi"}]}`, "Authorization", "Bearer "+bootstrapKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the bootstrapped Key at the openai door: %d %s", resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/budgets/extra", `{"spec": {"amount": "5"}}`, bearer...); resp.StatusCode != http.StatusCreated {
		t.Fatalf("the control plane after bootstrap: PUT = %d %s", resp.StatusCode, body)
	}
}

// TestBootstrapRefusalNamesTheFile is spec 035's criterion 12: a document
// that fails to resolve, and one whose credential variable is unset, each
// stop the start with a message naming the file, the object, and the code.
func TestBootstrapRefusalNamesTheFile(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	for _, tc := range []struct {
		name string
		docs map[string]string
		want []string
	}{
		{"a Model naming an absent Provider", map[string]string{
			"models/orphan.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: orphan\nspec:\n  targets:\n    - provider: nobody\n      model: x\n",
		}, []string{"models/orphan.yaml", "Model orphan", "not_found"}},
		{"a Provider whose variable is unset", map[string]string{
			"providers/openai.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: openai\nspec:\n  dialect: openai\n  baseURL: https://api.example.com/v1\n  credential:\n    valueFrom:\n      env: LUX_TEST_UNSET_VARIABLE\n",
		}, []string{"providers/openai.yaml", "Provider openai", "missing_field", "LUX_TEST_UNSET_VARIABLE"}},
		{"a document that does not decode", map[string]string{
			"broken.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata: [\n",
		}, []string{"broken.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := serveEnv(t, map[string]string{"LUX_OIDC_ISSUERS": iss.URL(), "LUX_ADMIN_SUBJECTS": iss.URL() + "|alice", "LUX_BOOTSTRAP_DIR": writeDir(t, tc.docs)})
			// A server that does not refuse would serve until stopped, so the
			// deadline turns a missing refusal into a failure, not a hang.
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			var out, errOut bytes.Buffer
			if code := run(ctx, nil, env(vars), &out, &errOut); code != 1 {
				t.Fatalf("exit %d, want 1:\n%s%s", code, out.String(), errOut.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("the refusal does not name %q:\n%s", want, errOut.String())
				}
			}
			if strings.Contains(out.String(), "listening") {
				t.Error("a listener opened before the bootstrap refusal")
			}
		})
	}
}

// TestBootstrapLoadsTheCatalog is spec 035's criterion 16: the example
// catalog of deploy/catalog bootstraps into a running server, and one of
// its Models answers through a door. The catalog's Providers name the
// vendors' own endpoints, so the copy this test bootstraps points the
// openai Provider at a stub and gives every Provider's variable a value.
func TestBootstrapLoadsTheCatalog(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	upstream := httptest.NewServer(provider.New(provider.Options{Dialect: v1.DialectOpenAI}))
	defer upstream.Close()
	src := filepath.Join("..", "..", "deploy", "catalog")
	dir := t.TempDir()
	files := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(rel) == "providers/openai.yaml" {
			data = []byte(strings.Replace(string(data), "https://api.openai.com/v1", upstream.URL+"/v1", 1))
		}
		files++
		target := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"LUX_OIDC_ISSUERS": iss.URL(), "LUX_ADMIN_SUBJECTS": iss.URL() + "|alice", "LUX_BOOTSTRAP_DIR": dir, "LUX_UPSTREAM_ALLOW_PRIVATE": "1",
		"OPENAI_API_KEY": provider.DefaultCredential,
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY", "ZHIPU_API_KEY"} {
		vars[name] = "unused-" + strings.ToLower(name)
	}
	srv := startServe(t, vars)
	defer srv.stop()
	if !strings.Contains(srv.out.String(), " "+strconv.Itoa(files)+" created, 0 updated, 0 unchanged") {
		t.Fatalf("the catalog's %d files were not all created:\n%s", files, srv.out.String())
	}
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}
	resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/keys/k", `{"spec": {"models": ["openai/*"]}}`, bearer...)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT key: %d %s", resp.StatusCode, body)
	}
	var key struct {
		Status struct{ Value string } `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &key); err != nil || key.Status.Value == "" {
		t.Fatalf("the Key's value: %v %s", err, body)
	}
	resp, body = do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model": "openai/gpt-4.1-mini", "messages": [{"role": "user", "content": "hi"}]}`, "Authorization", "Bearer "+key.Status.Value)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a catalog Model at the openai door: %d %s", resp.StatusCode, body)
	}
}
