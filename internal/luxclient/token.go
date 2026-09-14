// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxclient

import (
	"errors"
	"os"
	"strings"
)

// TokenSource yields the bearer for one request. It is called on every
// request, so a source that reads a file hands over whatever the file
// holds now.
type TokenSource func() (string, error)

// ErrEmptyToken is what a source answers when it has a place to read
// from and nothing is there.
var ErrEmptyToken = errors.New("the token is empty")

// StaticToken is a source that always yields s.
func StaticToken(s string) TokenSource {
	return func() (string, error) { return s, nil }
}

// FileToken is a source that reads path on every call and yields its
// contents with surrounding whitespace trimmed, which is how a helper
// that writes a token with a trailing newline is read back. An empty
// file is ErrEmptyToken; an unreadable one is the file's own error.
func FileToken(path string) TokenSource {
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			return "", ErrEmptyToken
		}
		return s, nil
	}
}
