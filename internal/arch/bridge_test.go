// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// glueFiles are the three files of gateway that held only the codec glue
// spec 021 moved into latere.ai/x/pkg/llmdialect/bridge; none may come
// back.
var glueFiles = []string{"models.go", "usage.go", "probe.go"}

// glueBound is the most non-test lines the three files that held the
// rest of that glue may hold together: the 1438 the six files held
// before the move, less the 700 the spec requires the move to remove.
const glueBound = 1438 - 700

// TestGatewayCarriesNoCodecGlue is spec 021's line count: models.go,
// usage.go, and probe.go are gone, and translate.go, stream.go, and
// errors.go together hold no more than glueBound lines. A shape, a
// scanner, or a splice written back into gateway shows up here before
// it shows up as a second copy of the bridge.
func TestGatewayCarriesNoCodecGlue(t *testing.T) {
	dir := filepath.Join(root(t), "gateway")
	for _, name := range glueFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("gateway/%s exists; what it held is latere.ai/x/pkg/llmdialect/bridge's (spec 021)", name)
		}
	}
	var lines int
	for _, name := range []string{"translate.go", "stream.go", "errors.go"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		lines += bytes.Count(data, []byte("\n"))
	}
	if lines > glueBound {
		t.Errorf("gateway/translate.go, stream.go, and errors.go hold %d lines, above the %d spec 021 leaves them", lines, glueBound)
	}
}

// gatewayImports is every package outside the standard library that
// gateway itself imports, spec 021's list: the bridge and ir, whose
// Dialect names Open takes, from the llmdialect tree, and nothing else
// of it; no codec, no tokencount, no httpjson, each of which the bridge
// reaches for it. The rest is spec 004's and 008's: the kinds, the
// circuit, the metrics, the semaphore, and the OpenTelemetry API the
// request span and the upstream transport use.
var gatewayImports = []string{
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
	"go.opentelemetry.io/otel",
	"go.opentelemetry.io/otel/attribute",
	"go.opentelemetry.io/otel/propagation",
	"go.opentelemetry.io/otel/trace",
	module + "/manifest",
	module + "/manifest/v1",
	"latere.ai/x/pkg/circuitbreaker",
	"latere.ai/x/pkg/llmdialect/bridge",
	"latere.ai/x/pkg/llmdialect/ir",
	"latere.ai/x/pkg/metrics",
	"latere.ai/x/pkg/semaphore",
}

// TestGatewayImports is spec 021's import rule over the package itself
// rather than its build list, which TestRootPackagesDialNothing holds:
// gateway's own imports outside the standard library are exactly
// gatewayImports, so a codec package or httpjson imported back into
// gateway, or a new dependency, is a row here with its reason.
func TestGatewayImports(t *testing.T) {
	dir := root(t)
	var got []string
	for _, path := range goList(t, dir, "-f", `{{join .Imports "\n"}}`, "./gateway") {
		first, _, _ := strings.Cut(path, "/")
		if strings.Contains(first, ".") {
			got = append(got, path)
		}
	}
	want := slices.Clone(gatewayImports)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("gateway imports\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n      "))
	}
}
