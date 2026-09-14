// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"regexp"
	"strings"
)

// globChars is the alphabet of a glob: lower-case letters, digits, dot,
// underscore, colon, slash, star, and hyphen, 1 to 128 of them. There is
// no ?, no character class, and no escape.
var globChars = regexp.MustCompile(`^[a-z0-9._:/*-]{1,128}$`)

// Match reports whether name matches selector under the glob rule of spec
// 003: * matches any run of characters, / included, every other character
// matches itself, and a selector without * is an exact name. The gateway
// matches a request's model against a Key's selectors with this function,
// the same one that validated them.
func Match(selector, name string) bool {
	parts := strings.Split(selector, "*")
	if len(parts) == 1 {
		return selector == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	rest := name[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// checkGlob holds a string to the glob alphabet.
func checkGlob(s string) error {
	if !globChars.MatchString(s) {
		return errString("a glob is 1 to 128 characters of [a-z0-9._:/*-]")
	}
	return nil
}

// checkSelector holds a Key selector to the rule: a glob when it carries
// a *, a Model name otherwise.
func checkSelector(s string) error {
	if strings.Contains(s, "*") {
		return checkGlob(s)
	}
	if !validModelName(s) {
		return errString("a selector without * is a Model name: segments of [a-z0-9._:-] joined by /, at most 128 characters")
	}
	return nil
}

// errString is a plain developer detail.
type errString string

func (e errString) Error() string { return string(e) }
