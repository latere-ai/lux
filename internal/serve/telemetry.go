// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"io"
	"log/slog"

	"latere.ai/x/pkg/otel"
)

// ServiceName is what every span, metric, and log record of the gateway
// is labeled with.
const ServiceName = "luxd"

// Telemetry is spec 019's start-up, the one call the serve role makes:
// latere.ai/x/pkg/otel's Bootstrap on the standard OTEL_* variables, so
// traces, metrics, and logs export over OTLP only with
// OTEL_EXPORTER_OTLP_ENDPOINT set and OTEL_SDK_DISABLED not true, with
// the W3C propagator and the parent-based sampler that package installs.
// The doors and the control plane take their tracer from the provider it
// installs, so nothing else is passed around.
//
// The returned logger is slog in JSON on stderr with service, version,
// and replica on every record and trace_id and span_id on one written
// inside a span, behind the handler that truncates a Key value to its
// prefix before the local stream or the OTLP bridge sees it. Bootstrap
// installs its own logger as slog's default; the caller installs the
// redacted one over it, because the process's default is the wiring's
// to set. A log exporter that cannot start is one WARN line and local
// logging, because telemetry never stops the gateway. stop flushes every
// exporter and is safe to call when none was installed.
func Telemetry(ctx context.Context, version string, stderr io.Writer) (*slog.Logger, func(context.Context) error) {
	logger, stop, err := otel.Bootstrap(ctx, otel.Config{
		ServiceName: ServiceName,
		Version:     version,
		Stdout:      slog.NewJSONHandler(stderr, nil),
	})
	logger = slog.New(Redact(logger.Handler()))
	if err != nil {
		logger.WarnContext(ctx, "telemetry: the OTLP log exporter did not start; logs stay on stderr", "err", err)
	}
	return logger, stop
}
