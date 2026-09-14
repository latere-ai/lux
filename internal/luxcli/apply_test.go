// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/lux/internal/api"
)

// TestApplyWalksDocumentsInOrder is spec 014's row: a file with three
// YAML documents of three kinds applies in file order, and a refusal on
// the second stops before the third.
func TestApplyWalksDocumentsInOrder(t *testing.T) {
	f := newFake(t)
	f.on("PUT", "/v1/providers/openai", 201, `{"kind":"Provider","metadata":{"name":"openai"}}`+"\n")
	f.on("PUT", "/v1/models/gpt-5", 200, `{"kind":"Model"}`+"\n")
	f.on("PUT", "/v1/budgets/team", 201, `{"kind":"Budget"}`+"\n")
	file := write(t, "all.yaml", "# a comment before the first document\n"+providerYAML+"---\n"+modelYAML+"--- # a trailing comment\n"+budgetYAML+"---\n# an empty document\n")
	r := run(t, f.env(nil), "apply", "-f", file)
	if r.code != 0 || r.stdout != `{"kind":"Provider","metadata":{"name":"openai"}}`+"\n"+`{"kind":"Model"}`+"\n"+`{"kind":"Budget"}`+"\n" {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	reqs := f.requests()
	if len(reqs) != 3 || reqs[0].Path != "/v1/providers/openai" || reqs[1].Path != "/v1/models/gpt-5" || reqs[2].Path != "/v1/budgets/team" {
		t.Fatalf("requests %+v", reqs)
	}
	if reqs[0].Body != "# a comment before the first document\n"+providerYAML || reqs[0].ContentType != "application/yaml" {
		t.Fatalf("the first document was not sent as written: %+v", reqs[0])
	}

	// A refusal on the second stops before the third.
	f.reset()
	f.refuse("PUT", "/v1/models/gpt-5", api.CodeInvalidField, "no such provider", "spec.targets[0].provider")
	r = run(t, f.env(nil), "apply", "-f", file)
	if r.code != 1 || r.stdout != `{"kind":"Provider","metadata":{"name":"openai"}}`+"\n" || r.stderr != api.CodeInvalidField.Message()+"\n" {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	if reqs := f.requests(); len(reqs) != 2 {
		t.Fatalf("%d requests after the refusal", len(reqs))
	}

	// Several files apply in the order given, and a JSON document is sent
	// as JSON.
	f.reset()
	f.on("PUT", "/v1/models/gpt-5", 200, `{"kind":"Model"}`+"\n")
	jsonDoc := `{"apiVersion":"lux.latere.ai/v1beta1","kind":"Budget","metadata":{"name":"team"},"spec":{"amount":"50"}}`
	r = run(t, f.env(nil), "apply", "-f", write(t, "b.json", jsonDoc), "-f", write(t, "p.yaml", providerYAML))
	reqs = f.requests()
	if r.code != 0 || len(reqs) != 2 || reqs[0].Path != "/v1/budgets/team" || reqs[0].ContentType != "application/json" || reqs[0].Body != jsonDoc+"\n" || reqs[1].Path != "/v1/providers/openai" {
		t.Fatalf("exit %d requests %+v", r.code, reqs)
	}
}

func TestApplyRefusesBeforeSending(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		name string
		file string
		args []string
		want string
	}{
		{"missing file", "", []string{"apply", "-f", "/nonexistent/x.yaml"}, `The file "/nonexistent/x.yaml" cannot be read.`},
		{"an argument", "", []string{"apply", "-f", "/x.yaml", "extra"}, "lux apply takes no argument; name the files with -f."},
		{"no file", "", []string{"apply"}, "Name at least one manifest file with -f."},
		{"no manifest", "# only a comment\n", nil, "The files hold no manifest."},
		{"malformed yaml", "kind: [\n", nil, ": document 1: " + api.CodeMalformedBody.Message()},
		{"unknown kind", "apiVersion: lux.latere.ai/v1beta1\nkind: Thing\nmetadata: {name: x}\n", nil, ": document 1: " + api.CodeUnsupportedKind.Message() + " (kind)"},
		{"unknown field", providerYAML + "  frobnicate: 1\n", nil, ": document 1: " + api.CodeUnknownField.Message() + " (spec.frobnicate)"},
		{"no name", "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nspec: {amount: \"1\"}\n", nil, ": document 1: The manifest has no metadata.name, and apply is by name."},
		{"a second document names the refusal", providerYAML + "---\nkind: [\n", nil, ": document 2: " + api.CodeMalformedBody.Message()},
		{"if-match over several", providerYAML + "---\n" + modelYAML, []string{"apply", "-f", "FILE", "--if-match", "3"}, "-if-match names one version, and the files hold 2 manifests; apply them one at a time."},
		{"bad if-match", providerYAML, []string{"apply", "-f", "FILE", "--if-match", "x"}, `-if-match takes a version above zero or *, not "x".`},
		{"no token", providerYAML, []string{"apply", "-f", "FILE", "--token", ""}, "Set " + EnvToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			args := tc.args
			if args == nil {
				args = []string{"apply", "-f", "FILE"}
			}
			env := f.env(nil)
			if tc.name == "no token" {
				env = f.env(map[string]string{"LUX_TOKEN": ""})
			}
			for i, a := range args {
				if a == "FILE" {
					args[i] = write(t, "m.yaml", tc.file)
				}
			}
			r := run(t, env, args...)
			if r.code != 2 || !strings.Contains(r.stderr, tc.want) || r.stdout != "" || len(f.requests()) != 0 {
				t.Fatalf("exit %d, %d requests\nstdout %q\nstderr %q\nwant %q", r.code, len(f.requests()), r.stdout, r.stderr, tc.want)
			}
		})
	}
}

// TestCredentialFromEnv is spec 014's row, table-driven: the flag sends
// the variable's value as spec.credential.value; an unset variable, a
// document that already carries a credential, two Providers with the
// bare form, and a non-Provider document are each exit 2 with nothing
// sent.
func TestCredentialFromEnv(t *testing.T) {
	f := newFake(t)
	f.on("PUT", "/v1/providers/openai", 201, `{"kind":"Provider"}`+"\n")
	f.on("PUT", "/v1/providers/anthropic", 201, `{"kind":"Provider"}`+"\n")
	f.on("PUT", "/v1/models/gpt-5", 201, `{"kind":"Model"}`+"\n")
	anthropic := strings.ReplaceAll(strings.ReplaceAll(providerYAML, "name: openai", "name: anthropic"), "dialect: openai", "dialect: anthropic")
	withValue := providerYAML + "  credential:\n    value: " + canaryCred + "\n"
	withValueFrom := providerYAML + "  credential:\n    valueFrom:\n      env: OPENAI_API_KEY\n"
	withHeader := providerYAML + "  credential:\n    header: X-Api-Key\n    scheme: raw\n"
	env := map[string]string{"OPENAI_API_KEY": canaryCred, "EMPTY": ""}
	cases := []struct {
		name  string
		file  string
		flags []string
		sent  int    // PUTs expected
		want  string // on exit 2, a fragment of stderr
	}{
		{"bare form with one Provider", providerYAML + "---\n" + modelYAML, []string{"--credential-from-env", "OPENAI_API_KEY"}, 2, ""},
		{"named form with two Providers", providerYAML + "---\n" + anthropic, []string{"--credential-from-env", "openai=OPENAI_API_KEY"}, 2, ""},
		{"a header alone merges with the flag", withHeader, []string{"--credential-from-env", "OPENAI_API_KEY"}, 1, ""},
		{"unset variable", providerYAML, []string{"--credential-from-env", "NOT_SET"}, 0, "The variable NOT_SET is unset or empty."},
		{"empty variable", providerYAML, []string{"--credential-from-env", "EMPTY"}, 0, "The variable EMPTY is unset or empty."},
		{"a document with a value", withValue, []string{"--credential-from-env", "OPENAI_API_KEY"}, 0, `already carries a credential`},
		{"a document with valueFrom", withValueFrom, []string{"--credential-from-env", "OPENAI_API_KEY"}, 0, `already carries a credential`},
		{"two Providers, bare form", providerYAML + "---\n" + anthropic, []string{"--credential-from-env", "OPENAI_API_KEY"}, 0, "the documents hold openai and anthropic; write <provider>=OPENAI_API_KEY"},
		{"no Provider", modelYAML, []string{"--credential-from-env", "OPENAI_API_KEY"}, 0, "needs a Provider among the documents, and there is none"},
		{"a non-Provider named", providerYAML + "---\n" + modelYAML, []string{"--credential-from-env", "gpt-5=OPENAI_API_KEY"}, 0, `names "gpt-5", and no Provider of that name is among the documents`},
		{"no such name", providerYAML, []string{"--credential-from-env", "mistral=OPENAI_API_KEY"}, 0, `names "mistral", and no Provider`},
		{"no variable", providerYAML, []string{"--credential-from-env", "openai="}, 0, `names a variable, and "openai=" names none`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.reset()
			args := append([]string{"apply", "-f", write(t, "m.yaml", tc.file)}, tc.flags...)
			r := run(t, f.env(env), args...)
			reqs := f.requests()
			if tc.want != "" {
				if r.code != 2 || len(reqs) != 0 || !strings.Contains(r.stderr, tc.want) || r.stdout != "" {
					t.Fatalf("exit %d, %d requests\nstderr %q\nwant %q", r.code, len(reqs), r.stderr, tc.want)
				}
				if strings.Contains(r.stderr, canaryCred) {
					t.Fatal("the credential reached stderr")
				}
				return
			}
			if r.code != 0 || len(reqs) != tc.sent {
				t.Fatalf("exit %d, %d requests\nstderr %q", r.code, len(reqs), r.stderr)
			}
			var body struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Metadata   struct{ Name string }
				Spec       struct {
					Dialect    string `json:"dialect"`
					Credential struct {
						Value  string `json:"value"`
						Header string `json:"header"`
					} `json:"credential"`
				} `json:"spec"`
				Status *json.RawMessage `json:"status"`
			}
			first := reqs[0]
			if first.Path != "/v1/providers/openai" || first.ContentType != "application/json" {
				t.Fatalf("the Provider was not sent first as JSON: %+v", first)
			}
			if err := json.Unmarshal([]byte(first.Body), &body); err != nil {
				t.Fatal(err)
			}
			if body.Kind != "Provider" || body.Metadata.Name != "openai" || body.Spec.Dialect != "openai" || body.Spec.Credential.Value != canaryCred || body.Status != nil {
				t.Fatalf("body %s", first.Body)
			}
			if tc.name == "a header alone merges with the flag" && body.Spec.Credential.Header != "X-Api-Key" {
				t.Fatalf("the header was lost: %s", first.Body)
			}
			// Every other document goes as written.
			for _, other := range reqs[1:] {
				if other.ContentType != "application/yaml" || strings.Contains(other.Body, canaryCred) {
					t.Fatalf("another document changed: %+v", other)
				}
			}
			if strings.Contains(r.stdout, canaryCred) || strings.Contains(r.stderr, canaryCred) {
				t.Fatal("the credential was printed")
			}
		})
	}
}

func TestSplitDocuments(t *testing.T) {
	docs := splitDocuments([]byte("---\na: 1\n---\n\n# only a comment\n\n--- \nb: 2\r\n---\t\nc: |\n  ---\n  not a marker\n...\n"))
	got := make([]string, 0, len(docs))
	for _, d := range docs {
		got = append(got, string(d))
	}
	want := []string{"a: 1\n", "b: 2\n", "c: |\n  ---\n  not a marker\n...\n"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("documents %q, want %q", got, want)
	}
	if n := len(splitDocuments([]byte(`{"a": 1}`))); n != 1 {
		t.Fatalf("a JSON file is %d documents", n)
	}
	if n := len(splitDocuments(nil)); n != 0 {
		t.Fatalf("an empty file is %d documents", n)
	}
}
