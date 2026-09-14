// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"maps"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/api"
)

// TestSecretsGoOneWay is spec 014's row: LUX_TOKEN, a token file's
// contents, a --credential-from-env value, and a Key value reach no
// stderr byte, -v included; a Key value reaches stdout exactly once per
// create and per rotate.
func TestSecretsGoOneWay(t *testing.T) {
	f := newFake(t)
	created := strings.Replace(keyJSON, `"prefix":"lux_9d4e",`, `"prefix":"lux_9d4e","value":"`+canaryKey+`",`, 1)
	f.on("PUT", "/v1/keys/run-42", 201, created)
	f.on("POST", "/v1/keys/run-42/rotate", 200, created)
	f.on("PUT", "/v1/providers/openai", 201, providerJSON)
	tokenFile := write(t, "token", canaryFile+"\n")
	keyFile := write(t, "key.yaml", keyYAML)
	providerFile := write(t, "provider.yaml", providerYAML)
	envs := map[string]map[string]string{
		"LUX_TOKEN":      f.env(map[string]string{"OPENAI_API_KEY": canaryCred}),
		"LUX_TOKEN_FILE": f.env(map[string]string{"LUX_TOKEN": "", "LUX_TOKEN_FILE": tokenFile, "OPENAI_API_KEY": canaryCred}),
	}
	secrets := []string{canaryToken, canaryFile, canaryCred}
	noSecret := func(t *testing.T, what, s string) {
		t.Helper()
		for _, c := range secrets {
			if strings.Contains(s, c) {
				t.Fatalf("%s carries %s:\n%s", what, c, s)
			}
		}
	}
	for source, env := range envs {
		for _, mode := range []string{"json", "yaml", "table", "wide"} {
			for _, args := range [][]string{
				{"apply", "-f", keyFile, "-v", "-o", mode},
				{"keys", "rotate", "run-42", "-v", "-o", mode},
			} {
				r := run(t, env, args...)
				if r.code != 0 {
					t.Fatalf("%s %v: exit %d stderr %q", source, args, r.code, r.stderr)
				}
				if n := strings.Count(r.stdout, canaryKey); n != 1 {
					t.Fatalf("%s %v: the Key value appears %d times on stdout:\n%s", source, args, n, r.stdout)
				}
				noSecret(t, source+" stdout", r.stdout)
				noSecret(t, source+" stderr", r.stderr)
				if strings.Contains(r.stderr, canaryKey) {
					t.Fatalf("%s %v: the Key value reached stderr", source, args)
				}
			}
			// The credential goes in and is never printed.
			r := run(t, env, "apply", "-f", providerFile, "--credential-from-env", "OPENAI_API_KEY", "-v", "-o", mode)
			if r.code != 0 || !strings.Contains(f.last(t).Body, canaryCred) {
				t.Fatalf("%s: exit %d stderr %q body %q", source, r.code, r.stderr, f.last(t).Body)
			}
			noSecret(t, source+" stdout", r.stdout)
			noSecret(t, source+" stderr", r.stderr)
		}
		// A refusal under -v prints the envelope's parts and nothing of the
		// request.
		f.refuse("PUT", "/v1/keys/run-42", api.CodeForbidden, "subject "+canaryToken+" may not", "spec")
		r := run(t, env, "apply", "-f", keyFile, "-v")
		if r.code != 1 || r.stdout != "" {
			t.Fatalf("%s refusal: exit %d stdout %q", source, r.code, r.stdout)
		}
		// The server's own detail is printed as the server wrote it; a
		// server that echoes a token is the server's leak, not the
		// command's, so the check is on the command's own lines.
		for line := range strings.SplitSeq(r.stderr, "\n") {
			if !strings.HasPrefix(line, "detail: ") {
				noSecret(t, source+" refusal stderr", line)
			}
		}
		f.on("PUT", "/v1/keys/run-42", 201, created)
		// A transport failure under -v names the URL and the error, never
		// the token.
		r = run(t, func() map[string]string {
			m := map[string]string{}
			maps.Copy(m, env)
			m["LUX_URL"] = "http://127.0.0.1:1"
			return m
		}(), "apply", "-f", keyFile, "-v")
		if r.code != 1 {
			t.Fatalf("%s unreachable: exit %d", source, r.code)
		}
		noSecret(t, source+" unreachable stderr", r.stderr)
	}
}
