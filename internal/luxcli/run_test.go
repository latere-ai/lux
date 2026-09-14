// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelpAndVersionExitZero is spec 014's row: -help with no command and
// after one prints that command's usage on stdout and exits 0, and
// -version prints the identity in luxd's shape.
func TestHelpAndVersionExitZero(t *testing.T) {
	for _, args := range [][]string{{"-help"}, {"--help"}, {"-h"}} {
		r := run(t, nil, args...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: lux <command> [flags]\n") || !strings.Contains(r.stdout, "\nCommands:\n  apply ") || r.stderr != "" {
			t.Fatalf("%v: exit %d\nstdout %q\nstderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
	for _, c := range commands {
		words := strings.Fields(c.name)
		r := run(t, nil, append(words, "-help")...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: "+c.usage+"\n\n") || !strings.Contains(r.stdout, "\nFlags:\n") || r.stderr != "" {
			t.Fatalf("lux %s -help: exit %d\nstdout %q\nstderr %q", c.name, r.code, r.stdout, r.stderr)
		}
		if c.plane == planeUsage {
			if strings.Contains(r.stdout, EnvKey+" when unset") {
				t.Fatalf("lux %s -help lists -key as the credential", c.name)
			}
		} else if !strings.Contains(r.stdout, "  -key string\n") {
			t.Fatalf("lux %s -help lacks -key", c.name)
		}
	}
	r := run(t, nil, "-version")
	if r.code != 0 || r.stdout != "lux 1.2.3 (abc1234, 2026-09-14)\n" || r.stderr != "" {
		t.Fatalf("-version: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := run(t, nil, "--version"); r.code != 0 || r.stdout != "lux 1.2.3 (abc1234, 2026-09-14)\n" {
		t.Fatalf("--version: exit %d, stdout %q", r.code, r.stdout)
	}
}

// TestCLIDocIsCurrent holds docs/cli.md equal to the binary's -help,
// command by command. LUX_CLI_DOC_WRITE=1 rewrites the file.
func TestCLIDocIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "cli.md")
	want := Doc()
	if os.Getenv("LUX_CLI_DOC_WRITE") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("docs/cli.md is not the binary's -help; run LUX_CLI_DOC_WRITE=1 go test ./internal/luxcli -run TestCLIDocIsCurrent\n%s", diff(string(got), want))
	}
	// The document holds every command's help, each under its own heading.
	for _, c := range commands {
		if !strings.Contains(want, "\n## lux "+c.name+"\n\n```\nUsage: "+c.usage+"\n") {
			t.Errorf("docs/cli.md lacks the section for lux %s", c.name)
		}
	}
}

// diff is the first line the two texts disagree on.
func diff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(g), len(w)) {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "line " + itoa(i+1) + ":\n  file: " + gl + "\n  help: " + wl
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestUnknownCommandAndFlagAreUsageErrors(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		args []string
		want string
	}{
		{nil, "A command is required."},
		{[]string{"frobnicate"}, `There is no command "frobnicate".`},
		{[]string{"serve", "--dialect", "openai"}, `There is no command "serve --dialect".`},
		{[]string{"keys", "frobnicate"}, `There is no command "keys frobnicate".`},
		{[]string{"-frobnicate"}, "flag provided but not defined: -frobnicate."},
		{[]string{"get", "-frobnicate", "key", "x"}, "flag provided but not defined: -frobnicate."},
		{[]string{"get", "key"}, "lux get takes a kind and a name."},
		{[]string{"get", "thing", "x"}, `The kind is provider, model, key, or budget, not "thing".`},
		{[]string{"get", "key", "x", "-o", "xml"}, `-o takes json, yaml, table, or wide, not "xml".`},
		{[]string{"list", "keys", "-limit", "x"}, `invalid value "x" for flag -limit: parse error.`},
		{[]string{"list", "keys", "-limit", "-1"}, "-limit is a count of items and cannot be negative."},
		{[]string{"list", "keys", "-l", "team"}, `-l takes k=v, not "team".`},
		{[]string{"delete", "key", "x", "-if-match", "seven"}, `-if-match takes a version above zero or *, not "seven".`},
		{[]string{"whoami", "extra"}, "lux whoami takes no argument."},
		{[]string{"models", "extra"}, "lux models takes no argument."},
		{[]string{"keys", "rotate"}, "lux keys rotate takes a name."},
	}
	for _, tc := range cases {
		f.reset()
		r := run(t, f.env(map[string]string{"LUX_KEY": "lux_x"}), tc.args...)
		if r.code != 2 || !strings.HasPrefix(r.stderr, tc.want+"\n") || r.stdout != "" {
			t.Errorf("%v: exit %d\nstdout %q\nstderr %q", tc.args, r.code, r.stdout, r.stderr)
		}
		if len(f.requests()) != 0 {
			t.Errorf("%v: a request was sent", tc.args)
		}
	}
}

func TestFlagsInterleaveWithArguments(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/v1/keys/run-42", 200, `{"kind":"Key"}`+"\n")
	for _, args := range [][]string{
		{"get", "key", "run-42", "-o", "json"},
		{"get", "-o", "json", "key", "run-42"},
		{"-o", "json", "get", "key", "run-42"},
		{"get", "key", "-o=json", "run-42"},
		{"--url", f.srv.URL, "--token", canaryToken, "get", "key", "run-42"},
	} {
		env := f.env(nil)
		if args[0] == "--url" {
			env = nil
		}
		r := run(t, env, args...)
		if r.code != 0 || r.stdout != `{"kind":"Key"}`+"\n" {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if got := f.last(t); got.UA != "lux/1.2.3" || got.Auth != "Bearer "+canaryToken {
		t.Fatalf("request %+v", got)
	}
}
