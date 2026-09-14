// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// TestLogRedactsKeyValues is spec 019's row: a Key value written to a
// log argument by a deliberate caller appears truncated to twelve
// characters, wherever it appears and whatever the field name: the
// message, a string attribute, a group member, an error, an attribute
// the logger was built with, and one under a group. A string that is
// not a minted value, and every non-string, passes untouched.
func TestLogRedactsKeyValues(t *testing.T) {
	value, err := MintKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MintKeyValue()
	if err != nil {
		t.Fatal(err)
	}
	prefix := value[:RedactedLength]
	var buf bytes.Buffer
	logger := slog.New(Redact(slog.NewJSONHandler(&buf, nil))).With("built", value)
	logger.WithGroup("g").Info("presented "+value+" twice: "+value,
		"key", value,
		"pair", "a="+value+" b="+second,
		"err", errors.New("Key "+value+" is disabled"),
		slog.Group("nested", "deep", value, "n", 7),
		"short", "lux_short",
		"count", 3,
		"on", true,
	)
	line := buf.String()
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("not one line: %q", line)
	}
	if strings.Contains(line, value) || strings.Contains(line, second) {
		t.Fatalf("a Key value survived:\n%s", line)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, line)
	}
	if got["msg"] != "presented "+prefix+" twice: "+prefix {
		t.Errorf("msg = %q", got["msg"])
	}
	if got["built"] != prefix {
		t.Errorf("built = %q", got["built"])
	}
	g, _ := got["g"].(map[string]any)
	if g["key"] != prefix || g["pair"] != "a="+prefix+" b="+second[:RedactedLength] || g["err"] != "Key "+prefix+" is disabled" {
		t.Errorf("group = %v", g)
	}
	nested, _ := g["nested"].(map[string]any)
	if nested["deep"] != prefix || nested["n"] != float64(7) {
		t.Errorf("nested = %v", nested)
	}
	if g["short"] != "lux_short" || g["count"] != float64(3) || g["on"] != true {
		t.Errorf("the untouched values changed: %v", g)
	}
}
