// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/lux/internal/api"
)

// skillPath is skills/lux/SKILL.md from this package's directory.
var skillPath = filepath.Join("..", "..", "skills", "lux", "SKILL.md")

// readSkill returns the frontmatter and the body of the skill.
func readSkill(t *testing.T) (front, body string) {
	t.Helper()
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("the skill does not begin with frontmatter")
	}
	rest := text[len("---\n"):]
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("the frontmatter never closes")
	}
	return front, body
}

// TestSkillFrontmatter is spec 014's row: skills/lux/SKILL.md parses
// with frontmatter of exactly name and description, name is lux, and the
// file stays under 200 lines.
func TestSkillFrontmatter(t *testing.T) {
	front, body := readSkill(t)
	var m map[string]any
	if err := yaml.Unmarshal([]byte(front), &m); err != nil {
		t.Fatalf("the frontmatter is not YAML: %v", err)
	}
	if len(m) != 2 || m["name"] != "lux" {
		t.Fatalf("frontmatter %v, want exactly name: lux and description", m)
	}
	desc, _ := m["description"].(string)
	if desc == "" || strings.Contains(desc, "\n") || !strings.Contains(desc, "lux") {
		t.Fatalf("description %q is not one sentence naming the command", desc)
	}
	data, _ := os.ReadFile(skillPath)
	if n := strings.Count(string(data), "\n"); n >= 200 {
		t.Fatalf("the skill is %d lines; under 200 keeps it cheap to hold resident", n)
	}
	for _, want := range []string{EnvURL, EnvToken, EnvKey, "kind: Provider", "kind: Model", "kind: Key", "kind: Budget", "lux apply", "lux get", "lux list", "lux models", "lux usage", "| 0 |", "| 1 |", "| 2 |", "-v"} {
		if !strings.Contains(body, want) {
			t.Errorf("the skill does not teach %q", want)
		}
	}
	if !strings.Contains(body, EnvToken+"` opens `/v1` and `"+EnvKey+"` opens a door") {
		t.Error("the skill does not say in one sentence which variable opens which plane")
	}
}

var (
	fenced    = regexp.MustCompile("(?s)```(\\w*)\n(.*?)```")
	backticks = regexp.MustCompile("`([^`]+)`")
)

// TestSkillNamesOnlyRealCommands is spec 014's row: every command and
// flag the skill names is in the table, in its shell blocks and in its
// prose, and every error code it names is in spec 011's table.
func TestSkillNamesOnlyRealCommands(t *testing.T) {
	_, body := readSkill(t)
	var lines []string
	for _, m := range fenced.FindAllStringSubmatch(body, -1) {
		if m[1] != "sh" && m[1] != "" {
			continue
		}
		for line := range strings.SplitSeq(m[2], "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), "$ ")
			if strings.HasPrefix(line, "lux ") {
				lines = append(lines, line)
			}
		}
	}
	for _, m := range backticks.FindAllStringSubmatch(body, -1) {
		if strings.HasPrefix(m[1], "lux ") {
			lines = append(lines, m[1])
		}
	}
	if len(lines) < 10 {
		t.Fatalf("only %d lux lines found in the skill", len(lines))
	}
	for _, line := range lines {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		words := strings.Fields(line)[1:]
		cmd, rest, ok := lookup(words)
		if !ok {
			t.Errorf("%q names no command", line)
			continue
		}
		probe := &app{o: Options{}}
		fs := probe.flagSet(cmd.name, cmd.plane != planeUsage)
		if cmd.flags != nil {
			cmd.flags(probe, fs)
		}
		for _, w := range rest {
			if !strings.HasPrefix(w, "-") {
				continue
			}
			name, _, _ := strings.Cut(strings.TrimLeft(w, "-"), "=")
			if fs.Lookup(name) == nil {
				t.Errorf("%q names the flag -%s, which lux %s does not have", line, name, cmd.name)
			}
		}
	}
	known := map[string]bool{}
	for _, c := range api.Codes() {
		known[string(c)] = true
	}
	for _, m := range backticks.FindAllStringSubmatch(body, -1) {
		if s := m[1]; regexp.MustCompile(`^[a-z]+(_[a-z]+)+$`).MatchString(s) && !known[s] {
			t.Errorf("the skill names the code %q, which spec 011's table does not hold", s)
		}
	}
}
