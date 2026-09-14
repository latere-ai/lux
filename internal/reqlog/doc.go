// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package reqlog is the request log of spec 012: one metering.Record per
// data plane request, batched as NDJSON objects into an S3 compatible
// bucket, and the reader GET /v1/requests uses when the archive is the
// source.
//
// The Exporter is the Recorder's archive: Append is a non-blocking push
// into a ring of BufferCap records that drops the oldest when full and
// counts the drop, so the hot path never waits on the bucket and never
// grows without bound. Run writes a batch of FlushSize records, or
// whatever is there every FlushInterval, as one object keyed by the UTC
// hour of the batch's first record, the replica, and a ULID; a failed
// write returns its batch to the head of the ring for the next flush,
// and the stop drains what the ring holds. The Reader lists each hour
// prefix the range covers, newest first, decodes each object a line at
// a time with the filters applied, and resumes from a cursor of the
// object key and the line offset.
package reqlog
