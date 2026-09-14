// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"strings"
	"testing"

	"latere.ai/x/pkg/metrics"
)

// TestRegisterIdleReadsZero: a process with no exporter still exposes
// MetricDropped, at zero, so the metric table holds without an archive.
func TestRegisterIdleReadsZero(t *testing.T) {
	reg := metrics.NewRegistry()
	RegisterIdle(reg)
	var out strings.Builder
	reg.WritePrometheus(&out)
	if !strings.Contains(out.String(), MetricDropped+" 0") {
		t.Fatalf("registry lacks %s at zero:\n%s", MetricDropped, out.String())
	}
}
