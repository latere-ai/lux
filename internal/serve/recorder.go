// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import "latere.ai/x/lux/gateway"

// DiscardRecorder is a gateway.Recorder that keeps nothing: every record
// the doors finish is dropped. It is the seam the doors mount with until
// spec 009's recorder, which turns a gateway.Record into a usage record
// with its cost and writes it through the metering package, replaces it
// in cmd/luxd; nothing else reads a record until then.
type DiscardRecorder struct{}

// Record implements gateway.Recorder.
func (DiscardRecorder) Record(gateway.Record) {}

var _ gateway.Recorder = DiscardRecorder{}
