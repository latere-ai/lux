// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package reqlog

import (
	"sync"

	"latere.ai/x/lux/metering"
)

// ring is the bounded buffer between the hot path and the bucket: a
// circular buffer of records that drops the oldest when full, which is
// the choice latere.ai/x/pkg/batch does not make, so it is this
// package's own. A batch being written stays in the ring until the
// write succeeds, so a push that drops meanwhile drops the oldest record
// there is, the batch's own, and a failed write has nothing to return:
// the batch is still at the head for the next flush. Every record
// carries a sequence so the acknowledgment after a write removes those
// records and no other, whatever was dropped in between.
type ring struct {
	mu   sync.Mutex
	buf  []metering.Record
	seqs []uint64
	head int    // the oldest record
	n    int    // how many are held
	next uint64 // the sequence of the next push
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]metering.Record, capacity), seqs: make([]uint64, capacity)}
}

// push appends r, dropping the oldest record when the ring is full, and
// reports whether one was dropped.
func (r *ring) push(rec metering.Record) (dropped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == len(r.buf) {
		r.buf[r.head] = metering.Record{}
		r.head = (r.head + 1) % len(r.buf)
		r.n--
		dropped = true
	}
	i := (r.head + r.n) % len(r.buf)
	r.buf[i], r.seqs[i] = rec, r.next
	r.next++
	r.n++
	return dropped
}

// peek copies the oldest records, at most n, oldest first, and returns
// the sequence of the last, for ack once they are written.
func (r *ring) peek(n int) ([]metering.Record, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n = min(n, r.n)
	out := make([]metering.Record, n)
	var last uint64
	for i := range n {
		j := (r.head + i) % len(r.buf)
		out[i], last = r.buf[j], r.seqs[j]
	}
	return out, last
}

// ack removes every record whose sequence is at or before through, the
// batch a peek returned that has since been written; a record of the
// batch dropped in between is already gone, and a record pushed since
// stays.
func (r *ring) ack(through uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.n > 0 && r.seqs[r.head] <= through {
		r.buf[r.head] = metering.Record{}
		r.head = (r.head + 1) % len(r.buf)
		r.n--
	}
}

// len is how many records the ring holds.
func (r *ring) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}
