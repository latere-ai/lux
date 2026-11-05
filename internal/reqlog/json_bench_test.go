// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"latere.ai/x/pkg/s3"

	"latere.ai/x/lux/metering"
)

// The benchmarks below measure the JSON cost of the request archive:
// the exporter's batch write, one object of newline-delimited records,
// and the reader's decode of one object. The bucket is in memory.

// jsonBenchRecord is one finished request, numbered i, with a two-attempt
// fallback on every fifth so the attempt list is not always one entry.
func jsonBenchRecord(i int) metering.Record {
	at := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC).Add(time.Duration(i) * 250 * time.Millisecond)
	r := metering.Record{
		ID: fmt.Sprintf("req_%026d", i), At: at, EndedAt: at.Add(1840 * time.Millisecond),
		Key:   metering.KeyRef{ID: fmt.Sprintf("key_%026d", i%40), Prefix: "lux_ab12cd34ef56"},
		Owner: "https://login.latere.ai|7c9e6679-7425-40de-944b-e07fc1f90ae7",
		Model: metering.Ref{Name: "claude-sonnet", ID: "mdl_01JBENCH0000000000000001"}, Provider: metering.Ref{Name: "anthropic", ID: "prv_01JBENCH0000000000000002"},
		UpstreamModel: "claude-sonnet-4-5", Door: "openai", TargetDialect: "anthropic", Route: "/openai/v1/chat/completions", Translated: true,
		Loss: []string{"messages[3].name", "user"},
		Attempts: []metering.Attempt{
			{Provider: "anthropic", UpstreamModel: "claude-sonnet-4-5", Status: metering.StatusOK, HTTPStatus: 200, DurationMs: 1840},
		},
		Status: metering.StatusOK, UpstreamStatus: 200, LatencyMs: 1840, TTFBMs: 412,
		Tokens: metering.Tokens{Input: 1450, Output: 312, CachedInput: 1024},
		Cost:   metering.Charge{Amount: 7410, Currency: "USD", Priced: true},
		Stream: i%2 == 0,
		Labels: map[string]string{"team": "platform", "env": "prod", "run": fmt.Sprintf("r_%d", i%90)}, RequestLabels: map[string]string{"trace": "tr_01JBENCH"},
	}
	if i%5 == 0 {
		r.Attempts = append([]metering.Attempt{{Provider: "openai", UpstreamModel: "gpt-4.1", Status: metering.StatusFailed, HTTPStatus: 529, Error: "upstream_error", DurationMs: 310}}, r.Attempts...)
	}
	return r
}

// jsonBenchBucket is a Bucket that keeps one object in memory.
type jsonBenchBucket struct {
	key  string
	body []byte
}

func (b *jsonBenchBucket) PutObject(_ context.Context, key string, body s3.Body) (string, error) {
	rc, err := body.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b.key = key
	b.body, err = io.ReadAll(rc)
	return `"etag"`, err
}

func (b *jsonBenchBucket) ListObjects(context.Context, s3.ListOptions) (s3.ListResult, error) {
	return s3.ListResult{}, nil
}

func (b *jsonBenchBucket) GetObject(context.Context, string, string) (io.ReadCloser, s3.Object, error) {
	return io.NopCloser(bytes.NewReader(b.body)), s3.Object{Key: b.key, Size: int64(len(b.body))}, nil
}

var jsonBenchBatches = []struct {
	name    string
	records int
}{{"small", 1}, {"medium", 25}, {"large", 400}}

// BenchmarkJSONArchiveBatchEncode is the exporter's flush of one batch:
// the records written as newline-delimited JSON into one object. The
// records are appended to the ring inside the loop, because a flush
// removes them.
func BenchmarkJSONArchiveBatchEncode(b *testing.B) {
	for _, size := range jsonBenchBatches {
		records := make([]metering.Record, size.records)
		for i := range records {
			records[i] = jsonBenchRecord(i)
		}
		bucket := &jsonBenchBucket{}
		e := NewExporter(ExporterOptions{
			Bucket: bucket, Replica: "replica-a", Now: func() time.Time { return time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC) },
			NewID: func(time.Time) string { return "01JBENCH0000000000000000000" },
		})
		for _, r := range records {
			e.Append(r)
		}
		if err := e.Flush(b.Context()); err != nil {
			b.Fatal(err)
		}
		n := len(bucket.body)
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(n))
			for b.Loop() {
				for _, r := range records {
					e.Append(r)
				}
				if err := e.Flush(b.Context()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONArchiveObjectDecode is the archive reader's decode of
// one object, every line a Record, as GET /v1/requests reads a page.
func BenchmarkJSONArchiveObjectDecode(b *testing.B) {
	for _, size := range jsonBenchBatches {
		bucket := &jsonBenchBucket{}
		e := NewExporter(ExporterOptions{
			Bucket: bucket, Replica: "replica-a", Now: func() time.Time { return time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC) },
			NewID: func(time.Time) string { return "01JBENCH0000000000000000000" },
		})
		for i := range size.records {
			e.Append(jsonBenchRecord(i))
		}
		if err := e.Flush(b.Context()); err != nil {
			b.Fatal(err)
		}
		reader := NewReader(bucket, "", "")
		q := metering.RecordQuery{From: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)}
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(bucket.body)))
			for b.Loop() {
				var out []metering.Record
				last, err := reader.read(b.Context(), bucket.key, 0, q, 0, &out)
				if err != nil {
					b.Fatal(err)
				}
				if last != size.records || len(out) != size.records {
					b.Fatalf("read %d lines and kept %d, want %d", last, len(out), size.records)
				}
			}
		})
	}
}
