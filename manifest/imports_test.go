// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"os/exec"
	"strings"
	"testing"
)

// TestManifestImports is spec 003's dependency rule over the build lists:
// manifest/v1 reaches the standard library alone; manifest adds
// manifest/v1 and github.com/goccy/go-yaml; neither reaches internal/,
// gateway, or metering.
func TestManifestImports(t *testing.T) {
	deps := func(pkg string) map[string]bool {
		out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", pkg).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		set := map[string]bool{}
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				set[line] = true
			}
		}
		return set
	}
	v1 := deps("latere.ai/x/lux/manifest/v1")
	delete(v1, "latere.ai/x/lux/manifest/v1")
	if len(v1) != 0 {
		t.Errorf("manifest/v1 reaches outside the standard library: %v", v1)
	}
	for dep := range deps("latere.ai/x/lux/manifest") {
		switch {
		case dep == "latere.ai/x/lux/manifest", dep == "latere.ai/x/lux/manifest/v1":
		case strings.HasPrefix(dep, "github.com/goccy/go-yaml"):
		default:
			t.Errorf("manifest reaches %s, outside manifest/v1 and github.com/goccy/go-yaml", dep)
		}
		for _, forbidden := range []string{"latere.ai/x/lux/internal", "latere.ai/x/lux/gateway", "latere.ai/x/lux/metering"} {
			if strings.HasPrefix(dep, forbidden) {
				t.Errorf("manifest reaches %s", dep)
			}
		}
	}
}
