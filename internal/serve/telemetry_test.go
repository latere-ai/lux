// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestTelemetryLoggerIsJSONWithBaseFieldsAndRedaction is spec 019's
// start-up at the package: without an endpoint nothing is exported and
// the logger is JSON on the given writer with service, version, and
// replica on every record, set as slog's default, a Key value truncated
// to its prefix wherever it is written, and a stop that is safe to call;
// with an endpoint and OTEL_SDK_DISABLED=true the exporter is never
// reached and logging stays local.
func TestTelemetryLoggerIsJSONWithBaseFieldsAndRedaction(t *testing.T) {
	var reached atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	t.Cleanup(collector.Close)
	value, err := MintKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		endpoint string
		disabled string
	}{
		{"no endpoint", "", ""},
		{"disabled with an endpoint", collector.URL, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.endpoint)
			t.Setenv("OTEL_SDK_DISABLED", tc.disabled)
			t.Setenv("POD_NAME", "luxd-0")
			var stderr bytes.Buffer
			logger, stop := Telemetry(t.Context(), "1.2.3", &stderr)
			logger.Info("presented "+value, "key", value)
			// The wiring installs the returned logger as the default, and a
			// line through the default is then the same handler's.
			slog.SetDefault(logger)
			slog.Info("through the default", "n", 1)
			if err := stop(t.Context()); err != nil {
				t.Fatalf("stop: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("%d lines:\n%s", len(lines), stderr.String())
			}
			for _, line := range lines {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatalf("not JSON: %v\n%s", err, line)
				}
				if m["service"] != ServiceName || m["version"] != "1.2.3" || m["replica"] != "luxd-0" {
					t.Errorf("base fields: %v", m)
				}
				if _, ok := m["trace_id"]; ok {
					t.Errorf("a trace id outside any span: %v", m)
				}
			}
			if strings.Contains(stderr.String(), value) || !strings.Contains(stderr.String(), `"key":"`+value[:RedactedLength]+`"`) {
				t.Errorf("the Key value was not redacted:\n%s", stderr.String())
			}
			if !strings.Contains(lines[1], `"msg":"through the default"`) {
				t.Errorf("the default logger is not the telemetry's:\n%s", lines[1])
			}
			if reached.Load() != 0 {
				t.Errorf("the collector was reached %d times", reached.Load())
			}
		})
	}
}
