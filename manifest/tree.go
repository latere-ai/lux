// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"strconv"
)

// The bounds a generic tree is held to before it is decoded: the scalar
// bytes it expands to, which is what an alias chain buys an attacker, and
// its nesting. The YAML library's own depth guard sits far above at ten
// thousand and is not the bound this contract makes.
const (
	MaxScalarBytes = 1 << 20
	MaxDepth       = 64
)

// bounds counts the scalar bytes of one tree as it is built.
type bounds struct {
	bytes int
}

// scalar adds n bytes at path and refuses once the total passes the limit.
func (b *bounds) scalar(path string, n int) error {
	b.bytes += n
	if b.bytes > MaxScalarBytes {
		return refuse(CodeInvalidField, "the document expands past "+strconv.Itoa(MaxScalarBytes)+" bytes of scalar values at this field", path)
	}
	return nil
}

// deeper refuses a nesting level past MaxDepth.
func deeper(path string, depth int) error {
	if depth > MaxDepth {
		return refuse(CodeInvalidField, "the document nests past "+strconv.Itoa(MaxDepth)+" levels at this field", path)
	}
	return nil
}

// fieldPath spells a struct field or a mapping key under parent.
func fieldPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// indexPath spells a list entry under parent.
func indexPath(parent string, i int) string {
	return parent + "[" + strconv.Itoa(i) + "]"
}

// keyPath spells a map key under parent, quoted because a key may carry
// a dot or a slash.
func keyPath(parent, key string) string {
	return parent + "[" + strconv.Quote(key) + "]"
}

// lineCol returns the one-based line and column of a byte offset in body.
func lineCol(body []byte, offset int) (line, col int) {
	if offset > len(body) {
		offset = len(body)
	}
	if offset < 0 {
		offset = 0
	}
	head := body[:offset]
	line = bytes.Count(head, []byte{'\n'}) + 1
	col = offset - (bytes.LastIndexByte(head, '\n') + 1) + 1
	return line, col
}

// at renders a position for a developer detail.
func at(line, col int) string {
	return "line " + strconv.Itoa(line) + ", column " + strconv.Itoa(col)
}
