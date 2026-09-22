// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	stubsink "latere.ai/x/lux/test/stubs/sink"
)

// TestGenericOverlayServesUnderAPrefix is spec 034's criterion 9: the
// generic overlay, with the three variables an installation behind a
// shared origin sets together (the audience list, the base path with the
// public URL that carries it, and the trusted proxy range) merged into its
// configuration, renders, and luxd check is green against what rendered.
// The endpoints the overlay names at example.com are replaced by loopback
// doubles, because check dials them; the three variables are read from
// the render and not restated.
func TestGenericOverlayServesUnderAPrefix(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not on PATH, so the overlay is not rendered here")
	}
	overlay, err := filepath.Abs(filepath.Join("..", "..", "deploy", "overlays", "generic"))
	if err != nil {
		t.Fatal(err)
	}
	// kustomize takes a resource path relative to the kustomization and
	// refuses an absolute one; it resolves the directory's symlinks first,
	// so the path is taken from the resolved directory.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, overlay)
	if err != nil {
		t.Fatal(err)
	}
	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - " + filepath.ToSlash(rel) + "\n" +
		"configMapGenerator:\n  - name: luxd\n    namespace: lux\n    behavior: merge\n    literals:\n" +
		"      - LUX_OIDC_AUDIENCE=lux,api.example.com\n" +
		"      - LUX_BASE_PATH=/v1/models\n" +
		"      - LUX_PUBLIC_URL=https://api.example.com/v1/models\n" +
		"      - LUX_TRUSTED_PROXIES=10.0.0.0/8\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomization), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("kubectl", "kustomize", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize: %v\n%s", err, stderr.String())
	}
	data := renderedConfig(t, out)
	for name, want := range map[string]string{
		"LUX_OIDC_AUDIENCE":   "lux,api.example.com",
		"LUX_BASE_PATH":       "/v1/models",
		"LUX_PUBLIC_URL":      "https://api.example.com/v1/models",
		"LUX_TRUSTED_PROXIES": "10.0.0.0/8",
	} {
		if data[name] != want {
			t.Fatalf("the rendered ConfigMap has %s=%q, want %q", name, data[name], want)
		}
	}

	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	authorizer := stub.New(t)
	sinkSrv := httptest.NewServer(stubsink.New(stubsink.Options{Secret: []byte(stubsink.DefaultSecret)}))
	defer sinkSrv.Close()
	vars := serveEnv(t, data)
	vars["LUX_OIDC_ISSUERS"] = iss.URL()
	vars["LUX_AUTHORIZER_URL"], vars["LUX_AUTHORIZER_TOKEN"] = authorizer.URL(), authorizer.Token()
	vars["LUX_EVENTS_URL"], vars["LUX_EVENTS_SECRET"] = sinkSrv.URL, stubsink.DefaultSecret
	delete(vars, "LUX_DB_MAX_CONNS")
	var stdout, errOut bytes.Buffer
	if code := run(t.Context(), []string{"check"}, env(vars), &stdout, &errOut); code != 0 {
		t.Fatalf("luxd check against the rendered overlay: exit %d\n%s%s", code, stdout.String(), errOut.String())
	}
	for _, want := range []string{"answers under base path /v1/models", "; audience lux, api.example.com"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("luxd check does not report %q:\n%s", want, stdout.String())
		}
	}
}

// renderedConfig is the data of the luxd ConfigMap in a kustomize render,
// whose generated name carries a content hash after the prefix.
func renderedConfig(t *testing.T, out []byte) map[string]string {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the render does not decode: %v", err)
		}
		if doc.Kind == "ConfigMap" && strings.HasPrefix(doc.Metadata.Name, "luxd-") {
			return doc.Data
		}
	}
	t.Fatal("the render carries no luxd ConfigMap")
	return nil
}
