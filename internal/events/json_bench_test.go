// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The benchmarks below measure the JSON cost of the event journal: the
// body every sink receives and the changed-paths diff every update
// raises.

func jsonBenchRecord() Record {
	return Record{
		ID: "evt_01JBENCH0000000000000000", Type: KeyUpdated, Time: time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC),
		Subject: "https://login.latere.ai|7c9e6679-7425-40de-944b-e07fc1f90ae7", Reason: ReasonRequest, RequestID: "req_01JBENCH0000000000000000",
		Object: ObjectRef{
			Kind: "Key", ID: "key_01JBENCH0000000000000000", Name: "ci-runner", Owner: "https://login.latere.ai|7c9e6679-7425-40de-944b-e07fc1f90ae7",
			Labels: map[string]string{"team": "platform", "env": "prod", "run": "r_42"},
		},
		Data: map[string]any{"paths": []string{"spec.models", "spec.limits.requestsPerMinute", "metadata.labels.env"}},
	}
}

// BenchmarkJSONSinkRecordMarshal is the body a sink receives for one
// event, as the journal row's payload.
func BenchmarkJSONSinkRecordMarshal(b *testing.B) {
	rec := jsonBenchRecord()
	probe, err := encode(rec)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(probe)))
	for b.Loop() {
		if _, err := encode(rec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONChangedPathsRoundTrip is the diff behind every
// <kind>.updated row: both objects encoded, read back as trees, and
// compared.
func BenchmarkJSONChangedPathsRoundTrip(b *testing.B) {
	for _, size := range []struct {
		name   string
		models int
	}{{"small", 2}, {"medium", 40}, {"large", 600}} {
		models := make([]string, size.models)
		for i := range models {
			models[i] = fmt.Sprintf("claude-sonnet-%03d", i)
		}
		old := &v1.Key{
			Metadata: v1.ObjectMeta{Name: "ci-runner", Labels: map[string]string{"team": "platform", "env": "prod"}},
			Spec:     v1.KeySpec{Models: models, Budget: "monthly", Doors: []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic}},
		}
		next := &v1.Key{
			Metadata: v1.ObjectMeta{Name: "ci-runner", Labels: map[string]string{"team": "platform", "env": "staging"}},
			Spec:     v1.KeySpec{Models: append(append([]string{}, models...), "gpt-4.1"), Budget: "monthly", Doors: []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic}},
		}
		if len(ChangedPaths(old, next)) == 0 {
			b.Fatal("the objects do not differ")
		}
		oldJSON, err := json.Marshal(old)
		if err != nil {
			b.Fatal(err)
		}
		nextJSON, err := json.Marshal(next)
		if err != nil {
			b.Fatal(err)
		}
		approx := len(oldJSON) + len(nextJSON)
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(approx))
			for b.Loop() {
				if len(ChangedPaths(old, next)) == 0 {
					b.Fatal("no changed paths")
				}
			}
		})
	}
}
