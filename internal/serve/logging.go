// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// keyValuePattern is the shape of a minted Key value: lux_ and forty
// characters of the mint's alphabet (spec 007). A supplied value has no
// pattern and never reaches a logger; a minted one that a deliberate
// caller writes is caught here.
var keyValuePattern = regexp.MustCompile(`lux_[A-Za-z0-9_-]{40}`)

// RedactedLength is what a Key value is truncated to in a log record:
// its prefix, twelve characters, the same figure status.prefix carries.
const RedactedLength = 12

// Redact wraps h so that every string a record carries, the message,
// each attribute at any depth, an error's text, and the attributes a
// logger was built with, has each minted Key value truncated to its
// first RedactedLength characters before h sees it. It runs outermost,
// so every exporter behind h, the local stream and the OTLP bridge
// alike, receives the redacted record (spec 019).
func Redact(h slog.Handler) slog.Handler { return redacting{h} }

type redacting struct{ slog.Handler }

func (r redacting) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, redactString(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return r.Handler.Handle(ctx, out)
}

func (r redacting) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return redacting{r.Handler.WithAttrs(out)}
}

func (r redacting) WithGroup(name string) slog.Handler {
	return redacting{r.Handler.WithGroup(name)}
}

// redactAttr rewrites one attribute: a string is truncated in place, a
// group is walked, and any other value that renders with a Key value in
// it, an error for one, is replaced by its redacted rendering. A value
// that carries none is returned as it was.
func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactString(v.String()))
	case slog.KindGroup:
		members := v.Group()
		out := make([]any, 0, len(members))
		for _, m := range members {
			out = append(out, redactAttr(m))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if s := fmt.Sprint(v.Any()); keyValuePattern.MatchString(s) {
			return slog.String(a.Key, redactString(s))
		}
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64, slog.KindTime, slog.KindUint64, slog.KindLogValuer:
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// redactString truncates every Key value in s to its prefix.
func redactString(s string) string {
	return keyValuePattern.ReplaceAllStringFunc(s, func(m string) string { return m[:RedactedLength] })
}
