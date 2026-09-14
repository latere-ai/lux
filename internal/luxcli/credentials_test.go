// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestCredentialsDoNotCrossPlanes is spec 014's row: a /v1 command with
// only LUX_KEY, a door command with only LUX_TOKEN, and both token
// variables together are each exit 2 naming the variables, with no
// request made.
func TestCredentialsDoNotCrossPlanes(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		name  string
		env   map[string]string
		args  []string
		names []string
	}{
		{"a /v1 command with only a Key", map[string]string{"LUX_TOKEN": "", "LUX_KEY": "lux_x"}, []string{"get", "key", "x"}, []string{EnvKey, EnvToken, EnvTokenFile}},
		{"a door command with only a token", nil, []string{"models"}, []string{EnvToken, EnvKey}},
		{"a door command with only a token file", map[string]string{"LUX_TOKEN": "", "LUX_TOKEN_FILE": "/some/file"}, []string{"models"}, []string{EnvToken, EnvKey}},
		{"both token variables", map[string]string{"LUX_TOKEN_FILE": "/some/file"}, []string{"whoami"}, []string{EnvToken, EnvTokenFile}},
		{"both token flags", nil, []string{"whoami", "--token", "a", "--token-file", "b"}, []string{EnvToken, EnvTokenFile}},
		{"no credential on /v1", map[string]string{"LUX_TOKEN": ""}, []string{"whoami"}, []string{EnvToken, EnvTokenFile}},
		{"no credential on a door", map[string]string{"LUX_TOKEN": ""}, []string{"models"}, []string{EnvKey}},
		{"no URL on /v1", map[string]string{"LUX_URL": ""}, []string{"whoami"}, []string{EnvURL}},
		{"no URL on a door", map[string]string{"LUX_URL": "", "LUX_KEY": "lux_x"}, []string{"models"}, []string{EnvURL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			r := run(t, f.env(tc.env), tc.args...)
			if r.code != 2 || r.stdout != "" || len(f.requests()) != 0 {
				t.Fatalf("exit %d, stdout %q, %d requests, stderr %q", r.code, r.stdout, len(f.requests()), r.stderr)
			}
			for _, n := range tc.names {
				if !strings.Contains(r.stderr, n) {
					t.Errorf("stderr %q does not name %s", r.stderr, n)
				}
			}
		})
	}
	// A flag overrides the environment: --token beside LUX_TOKEN_FILE in
	// the environment is one token, not two.
	f.on("GET", "/v1/self", 200, "{}\n")
	if r := run(t, f.env(map[string]string{"LUX_TOKEN": "", "LUX_TOKEN_FILE": "/some/file"}), "whoami", "--token", "flag-token"); r.code != 0 || f.last(t).Auth != "Bearer flag-token" {
		t.Fatalf("--token over LUX_TOKEN_FILE: exit %d stderr %q", r.code, r.stderr)
	}
	// Both a token and a Key in the environment is a shell with both
	// exported: each command takes its own.
	f.on("GET", "/lux/v1/models", 200, "{}\n")
	if r := run(t, f.env(map[string]string{"LUX_KEY": "lux_x"}), "models"); r.code != 0 || f.last(t).Auth != "Bearer lux_x" {
		t.Fatalf("door with both: exit %d stderr %q", r.code, r.stderr)
	}
	if r := run(t, f.env(map[string]string{"LUX_KEY": "lux_x"}), "whoami"); r.code != 0 || f.last(t).Auth != "Bearer "+canaryToken {
		t.Fatalf("/v1 with both: exit %d stderr %q", r.code, r.stderr)
	}
}

// TestTokenFileIsReadPerRequest is spec 014's row: with LUX_TOKEN unset
// and a token file that changes between two requests, each request
// sends the file's current bytes.
func TestTokenFileIsReadPerRequest(t *testing.T) {
	f := newFake(t)
	path := write(t, "token", "first\n")
	f.answers["GET /v1/keys"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"items":[{"kind":"Key"}],"next_cursor":"c2"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"kind":"Key"}]}` + "\n"))
	}
	r := run(t, f.env(map[string]string{"LUX_TOKEN": "", "LUX_TOKEN_FILE": path}), "list", "keys")
	if r.code != 0 {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].Auth != "Bearer first" || reqs[1].Auth != "Bearer second" {
		t.Fatalf("requests %+v", reqs)
	}
	// A token file that cannot be read, or is empty, is exit 2 naming the
	// variable, and nothing is sent.
	for _, content := range []string{"", "  \n"} {
		f.reset()
		p := path + "-empty"
		if content != "" {
			p = write(t, "empty", content)
		}
		r := run(t, f.env(map[string]string{"LUX_TOKEN": "", "LUX_TOKEN_FILE": p}), "list", "keys")
		if r.code != 2 || !strings.Contains(r.stderr, EnvTokenFile) || len(f.requests()) != 0 {
			t.Fatalf("token file %q: exit %d stderr %q, %d requests", content, r.code, r.stderr, len(f.requests()))
		}
	}
}

// TestSDKVariableFallbacks is spec 014's row: a door command with only
// LUX_BASE_URL and LUX_API_KEY reaches the door with that Key; with
// LUX_URL and LUX_KEY also set those win; neither fallback is read on a
// /v1 command.
func TestSDKVariableFallbacks(t *testing.T) {
	sdk, lux := newFake(t), newFake(t)
	sdk.on("GET", "/lux/v1/models", 200, `{"object":"list","data":[]}`+"\n")
	lux.on("GET", "/lux/v1/models", 200, `{"object":"list","data":[]}`+"\n")
	r := run(t, map[string]string{"LUX_BASE_URL": sdk.srv.URL, "LUX_API_KEY": "lux_sdk"}, "models")
	if r.code != 0 || len(sdk.requests()) != 1 || sdk.last(t).Auth != "Bearer lux_sdk" || len(lux.requests()) != 0 {
		t.Fatalf("fallbacks alone: exit %d stderr %q, sdk %d lux %d", r.code, r.stderr, len(sdk.requests()), len(lux.requests()))
	}
	sdk.reset()
	r = run(t, map[string]string{"LUX_BASE_URL": sdk.srv.URL, "LUX_API_KEY": "lux_sdk", "LUX_URL": lux.srv.URL, "LUX_KEY": "lux_own"}, "models")
	if r.code != 0 || len(sdk.requests()) != 0 || len(lux.requests()) != 1 || lux.last(t).Auth != "Bearer lux_own" {
		t.Fatalf("both sets: exit %d, sdk %d lux %d auth %q", r.code, len(sdk.requests()), len(lux.requests()), lux.last(t).Auth)
	}
	// Neither fallback is read on a /v1 command: LUX_BASE_URL does not
	// stand in for LUX_URL, and LUX_API_KEY is not a token.
	sdk.reset()
	r = run(t, map[string]string{"LUX_BASE_URL": sdk.srv.URL, "LUX_API_KEY": "lux_sdk", "LUX_TOKEN": canaryToken}, "whoami")
	if r.code != 2 || !strings.Contains(r.stderr, EnvURL) || len(sdk.requests()) != 0 {
		t.Fatalf("/v1 with the fallbacks: exit %d stderr %q, %d requests", r.code, r.stderr, len(sdk.requests()))
	}
	r = run(t, map[string]string{"LUX_URL": sdk.srv.URL, "LUX_API_KEY": "lux_sdk"}, "whoami")
	if r.code != 2 || !strings.Contains(r.stderr, EnvToken) || len(sdk.requests()) != 0 {
		t.Fatalf("/v1 with the api key alone: exit %d stderr %q", r.code, r.stderr)
	}
}
