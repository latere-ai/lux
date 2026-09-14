// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"crypto/rand"
	"io"
	"time"
)

// crockford is the base32 alphabet of a ULID: no I, L, O, or U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// IDLength is the length of the ULID after its prefix.
const IDLength = 26

// NewID mints one prefixed ULID, the one generator of every id in the
// tree: 48 bits of milliseconds since the Unix epoch and 80 random bits,
// written as 26 Crockford base32 characters after the prefix, so ids of
// one kind sort by the time they were minted. A nil random reads
// crypto/rand. It is written with the standard library alone, so a binary
// gains no module for an identifier.
func NewID(prefix string, now time.Time, random io.Reader) string {
	if random == nil {
		random = rand.Reader
	}
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := range 6 {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := io.ReadFull(random, b[6:]); err != nil {
		panic("manifest/v1: NewID: the random source failed: " + err.Error())
	}
	// 128 bits are read as 130 with two leading zero bits, five at a
	// time, most significant first, which is the ULID's own encoding.
	out := make([]byte, 0, len(prefix)+IDLength)
	out = append(out, prefix...)
	acc, nbits := uint32(0), 2
	for _, by := range b {
		acc = acc<<8 | uint32(by)
		nbits += 8
		for nbits >= 5 {
			nbits -= 5
			out = append(out, crockford[(acc>>nbits)&31])
		}
	}
	return string(out)
}
