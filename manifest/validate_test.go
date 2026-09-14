// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	v1 "latere.ai/x/lux/manifest/v1"
)

// syntaxCase is one refused value: the kind, the spec body (or a whole
// body when it starts with apiVersion), the code, and the path.
type syntaxCase struct {
	name, kind, body string
	code             Code
	path             string
}

func (c syntaxCase) full() string {
	if strings.HasPrefix(c.body, "apiVersion") {
		return c.body
	}
	return head(c.kind, "n") + c.body
}

func runSyntaxCases(t *testing.T, o Options, cases []syntaxCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := resolveErr(t, c.full(), o)
			wantErr(t, e, c.code, c.path)
			if e.Detail == "" {
				t.Error("no developer detail")
			}
		})
	}
}

func TestFieldSyntax(t *testing.T) {
	o := corpusOptions(t)
	long := strings.Repeat("a", 64)
	cases := []syntaxCase{
		// The three name rules.
		{"provider name upper case", v1.KindProvider, head(v1.KindProvider, "OpenAI") + minProvider, CodeInvalidField, "metadata.name"},
		{"provider name with a dot", v1.KindProvider, head(v1.KindProvider, "api.openai") + minProvider, CodeInvalidField, "metadata.name"},
		{"provider name too long", v1.KindProvider, head(v1.KindProvider, long) + minProvider, CodeInvalidField, "metadata.name"},
		{"key name leading hyphen", v1.KindKey, head(v1.KindKey, "-run") + minKey, CodeInvalidField, "metadata.name"},
		{"budget name with underscore", v1.KindBudget, head(v1.KindBudget, "team_research") + minBudget, CodeInvalidField, "metadata.name"},
		{"model name empty segment", v1.KindModel, head(v1.KindModel, "openai//gpt-5") + minModel, CodeInvalidField, "metadata.name"},
		{"model name trailing slash", v1.KindModel, head(v1.KindModel, "openai/") + minModel, CodeInvalidField, "metadata.name"},
		{"model name upper case", v1.KindModel, head(v1.KindModel, "GPT-5") + minModel, CodeInvalidField, "metadata.name"},
		{"model name trailing dot", v1.KindModel, head(v1.KindModel, "gpt-5.") + minModel, CodeInvalidField, "metadata.name"},
		{"model name too long", v1.KindModel, head(v1.KindModel, strings.Repeat("a", 129)) + minModel, CodeInvalidField, "metadata.name"},
		// Labels and annotations.
		{"label key syntax", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  labels:\n    \"a b\": c\nspec:\n  amount: \"1\"\n", CodeInvalidField, `metadata.labels["a b"]`},
		{"label key two slashes", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  labels:\n    a/b/c: d\nspec:\n  amount: \"1\"\n", CodeInvalidField, `metadata.labels["a/b/c"]`},
		{"label prefix not a subdomain", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  labels:\n    Bad_Prefix/x: d\nspec:\n  amount: \"1\"\n", CodeInvalidField, `metadata.labels["Bad_Prefix/x"]`},
		{"label value syntax", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  labels:\n    tier: \"paid tier\"\nspec:\n  amount: \"1\"\n", CodeInvalidField, `metadata.labels["tier"]`},
		{"annotation key syntax", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  annotations:\n    \"-x\": d\nspec:\n  amount: \"1\"\n", CodeInvalidField, `metadata.annotations["-x"]`},
		{"annotation value too long", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  annotations:\n    note: \"" + strings.Repeat("x", 4097) + "\"\nspec:\n  amount: \"1\"\n", CodeInvalidField, `metadata.annotations["note"]`},
		{"annotations total too large", v1.KindBudget, "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  annotations:\n" + annotationsOf(17) + "spec:\n  amount: \"1\"\n", CodeInvalidField, "metadata.annotations"},
		// Money, window, glob, duration, timestamp, currency, header names.
		{"money seven fraction digits", v1.KindBudget, "spec:\n  amount: \"1.1234567\"\n", CodeInvalidField, "spec.amount"},
		{"money thirteenth integer digit", v1.KindBudget, "spec:\n  amount: \"1234567890123\"\n", CodeInvalidField, "spec.amount"},
		{"money zero amount", v1.KindBudget, "spec:\n  amount: \"0\"\n", CodeInvalidField, "spec.amount"},
		{"money zero spend", v1.KindKey, "spec:\n  models: [gpt-5]\n  limits:\n    spend: {amount: \"0\", window: 24h}\n", CodeInvalidField, "spec.limits.spend.amount"},
		{"window below a minute", v1.KindBudget, "spec:\n  amount: \"1\"\n  window: 30s\n", CodeInvalidField, "spec.window"},
		{"window above a year", v1.KindBudget, "spec:\n  amount: \"1\"\n  window: 8761h\n", CodeInvalidField, "spec.window"},
		{"window a word", v1.KindKey, "spec:\n  models: [gpt-5]\n  limits:\n    spend: {amount: \"1\", window: weekly}\n", CodeInvalidField, "spec.limits.spend.window"},
		{"glob upper case", v1.KindKey, "spec:\n  models: [\"GPT-*\"]\n", CodeInvalidField, "spec.models[0]"},
		{"glob question mark", v1.KindKey, "spec:\n  models: [\"gpt-?\"]\n", CodeInvalidField, "spec.models[0]"},
		{"exact selector not a name", v1.KindKey, "spec:\n  models: [\"gpt-5\", \"-x\"]\n", CodeInvalidField, "spec.models[1]"},
		{"too many selectors", v1.KindKey, "spec:\n  models: [" + selectors(65) + "]\n", CodeInvalidField, "spec.models"},
		{"duplicate selector", v1.KindKey, "spec:\n  models: [gpt-5, \"anthropic/*\", gpt-5]\n", CodeInvalidField, "spec.models[2]"},
		{"discovery include glob", v1.KindProvider, minProvider + "  discovery:\n    include: [\"gpt-*\", \"Bad\"]\n", CodeInvalidField, "spec.discovery.include[1]"},
		{"discovery exclude glob", v1.KindProvider, minProvider + "  discovery:\n    exclude: [\"a b\"]\n", CodeInvalidField, "spec.discovery.exclude[0]"},
		{"duration syntax", v1.KindProvider, minProvider + "  timeout: soon\n", CodeInvalidField, "spec.timeout"},
		{"timeout below a second", v1.KindProvider, minProvider + "  timeout: 500ms\n", CodeInvalidField, "spec.timeout"},
		{"timeout above an hour", v1.KindProvider, minProvider + "  timeout: 61m\n", CodeInvalidField, "spec.timeout"},
		{"ttl syntax", v1.KindKey, "spec:\n  models: [gpt-5]\n  ttl: 1 week\n", CodeInvalidField, "spec.ttl"},
		{"ttl below a minute", v1.KindKey, "spec:\n  models: [gpt-5]\n  ttl: 59s\n", CodeInvalidField, "spec.ttl"},
		{"timestamp syntax", v1.KindKey, "spec:\n  models: [gpt-5]\n  expiresAt: 2026-13-01\n", CodeInvalidField, "spec.expiresAt"},
		{"timestamp in the past", v1.KindKey, "spec:\n  models: [gpt-5]\n  expiresAt: 2026-09-13T10:00:00Z\n", CodeInvalidField, "spec.expiresAt"},
		{"currency lower case", v1.KindBudget, "spec:\n  amount: \"1\"\n  currency: usd\n", CodeInvalidField, "spec.currency"},
		{"currency four letters", v1.KindModel, minModel + "  pricing: {currency: USDT, input: \"1\", output: \"1\"}\n", CodeInvalidField, "spec.pricing.currency"},
		{"spend currency", v1.KindKey, "spec:\n  models: [gpt-5]\n  limits:\n    spend: {amount: \"1\", currency: \"$\", window: 24h}\n", CodeInvalidField, "spec.limits.spend.currency"},
		{"header name with a space", v1.KindProvider, minProvider + "  headers:\n    \"X A\": b\n", CodeInvalidField, `spec.headers["X A"]`},
		{"header value with a newline", v1.KindProvider, minProvider + "  headers:\n    X-A: \"a\\nb\"\n", CodeInvalidField, `spec.headers["X-A"]`},
		{"header value too long", v1.KindProvider, minProvider + "  headers:\n    X-A: \"" + strings.Repeat("v", 4097) + "\"\n", CodeInvalidField, `spec.headers["X-A"]`},
		{"header value non-ASCII", v1.KindProvider, minProvider + "  headers:\n    X-A: \"héllo\"\n", CodeInvalidField, `spec.headers["X-A"]`},
		{"too many headers", v1.KindProvider, minProvider + "  headers:\n" + headersOf(17), CodeInvalidField, "spec.headers"},
		{"credential header name", v1.KindProvider, minProvider + "  credential: {header: \"Auth orization\"}\n", CodeInvalidField, "spec.credential.header"},
		// Enums.
		{"dialect", v1.KindProvider, "spec:\n  dialect: azure\n  baseURL: https://a.example.com\n", CodeInvalidField, "spec.dialect"},
		{"scheme", v1.KindProvider, minProvider + "  credential: {scheme: basic}\n", CodeInvalidField, "spec.credential.scheme"},
		{"discovery mode", v1.KindProvider, minProvider + "  discovery: {mode: manual}\n", CodeInvalidField, "spec.discovery.mode"},
		{"health mode", v1.KindProvider, minProvider + "  health: {mode: active}\n", CodeInvalidField, "spec.health.mode"},
		{"fallback", v1.KindModel, minModel + "  fallback: always\n", CodeInvalidField, "spec.fallback"},
		{"modality", v1.KindModel, minModel + "  modalities: {input: [text, smell]}\n", CodeInvalidField, "spec.modalities.input[1]"},
		{"modality repeated", v1.KindModel, minModel + "  modalities: {output: [text, text]}\n", CodeInvalidField, "spec.modalities.output[1]"},
		{"modalities empty", v1.KindModel, minModel + "  modalities: {input: []}\n", CodeInvalidField, "spec.modalities.input"},
		// Ranges.
		{"weight above 1000", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, weight: 1001}\n", CodeInvalidField, "spec.targets[0].weight"},
		{"weight negative", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, weight: -1}\n", CodeInvalidField, "spec.targets[0].weight"},
		{"priority above 9", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, priority: 10}\n", CodeInvalidField, "spec.targets[0].priority"},
		{"priority negative", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, priority: -1}\n", CodeInvalidField, "spec.targets[0].priority"},
		{"every weight zero", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, weight: 0}\n    - {provider: azure, weight: 0}\n", CodeInvalidField, "spec.targets"},
		{"per", v1.KindModel, minModel + "  pricing: {per: 500, input: \"1\", output: \"1\"}\n", CodeInvalidField, "spec.pricing.per"},
		{"concurrency negative", v1.KindProvider, minProvider + "  concurrency: -1\n", CodeInvalidField, "spec.concurrency"},
		{"requestsPerMinute negative", v1.KindKey, "spec:\n  models: [gpt-5]\n  limits: {requestsPerMinute: -1}\n", CodeInvalidField, "spec.limits.requestsPerMinute"},
		{"tokensPerMinute negative", v1.KindKey, "spec:\n  models: [gpt-5]\n  limits: {tokensPerMinute: -1}\n", CodeInvalidField, "spec.limits.tokensPerMinute"},
		{"contextWindow negative", v1.KindModel, minModel + "  contextWindow: -1\n", CodeInvalidField, "spec.contextWindow"},
		{"maxOutputTokens negative", v1.KindModel, minModel + "  maxOutputTokens: -1\n", CodeInvalidField, "spec.maxOutputTokens"},
		{"maxOutputTokens above contextWindow", v1.KindModel, minModel + "  contextWindow: 1000\n  maxOutputTokens: 1001\n", CodeInvalidField, "spec.maxOutputTokens"},
		// References and upstream names.
		{"target provider shape", v1.KindModel, "spec:\n  targets:\n    - provider: Open_AI\n", CodeInvalidField, "spec.targets[0].provider"},
		{"target model control character", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, model: \"a\\u0007b\"}\n", CodeInvalidField, "spec.targets[0].model"},
		{"target model too long", v1.KindModel, "spec:\n  targets:\n    - {provider: openai, model: \"" + strings.Repeat("m", 257) + "\"}\n", CodeInvalidField, "spec.targets[0].model"},
		{"budget reference shape", v1.KindKey, "spec:\n  models: [gpt-5]\n  budget: Team/Research\n", CodeInvalidField, "spec.budget"},
		{"env name", v1.KindProvider, minProvider + "  credential: {valueFrom: {env: 9LIVES}}\n", CodeInvalidField, "spec.credential.valueFrom.env"},
		{"key env name", v1.KindKey, "spec:\n  models: [gpt-5]\n  valueFrom: {env: \"a-b\"}\n", CodeInvalidField, "spec.valueFrom.env"},
		{"credential value empty", v1.KindProvider, minProvider + "  credential: {value: \"\"}\n", CodeInvalidField, "spec.credential.value"},
		{"credential value too long", v1.KindProvider, minProvider + "  credential: {value: \"" + strings.Repeat("s", 4097) + "\"}\n", CodeInvalidField, "spec.credential.value"},
		{"tunnel off", v1.KindProvider, "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata: {name: n}\nspec:\n  dialect: openai\n  tunnel: true\n", CodeInvalidField, "spec.tunnel"},
	}
	// The tunnel case needs the tunnel off; everything else runs under
	// the corpus options.
	var tunnelOff []syntaxCase
	var rest []syntaxCase
	for _, c := range cases {
		if c.name == "tunnel off" {
			tunnelOff = append(tunnelOff, c)
		} else {
			rest = append(rest, c)
		}
	}
	runSyntaxCases(t, o, rest)
	off := o
	off.TunnelEnabled = false
	runSyntaxCases(t, off, tunnelOff)
	t.Run("the values the rules admit", func(t *testing.T) {
		for _, body := range []string{
			head(v1.KindModel, "gpt-4.1") + minModel,
			head(v1.KindModel, "laptop/llama3.1:8b") + minModel,
			head(v1.KindModel, "relay/anthropic/claude-sonnet-4") + minModel,
			head(v1.KindModel, "a/b/c/d") + minModel,
			head(v1.KindModel, "x_y.z") + minModel,
			head(v1.KindProvider, strings.Repeat("a", 63)) + minProvider,
			head(v1.KindProvider, "p") + minProvider + "  headers:\n    X-Trace: \"a b~\"\n  timeout: 1h\n  concurrency: 100\n",
			head(v1.KindProvider, "p") + minProvider + "  credential:\n    value: \"" + strings.Repeat("s", 4096) + "\"\n",
			head(v1.KindModel, "m") + "spec:\n  targets:\n    - {provider: prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y, weight: 1000, priority: 9}\n" + "  pricing: {per: 1, input: \"0\", output: \"999999999999.999999\"}\n",
			head(v1.KindKey, "k") + "spec:\n  models: [" + selectors(64) + "]\n  budget: bud_01J9ZK2P7Q8R9S0T1U2V3W4X61\n  ttl: 1m\n",
			head(v1.KindBudget, "b") + "spec:\n  amount: \"0.000001\"\n  window: 8760h\n",
			head(v1.KindBudget, "b") + "spec:\n  amount: \"1\"\n  window: 1m\n",
			"apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: n\n  labels:\n    example.com/tier: paid\n    tier: \"\"\n  annotations:\n    example.com/note: \"" + strings.Repeat("x", 4096) + "\"\nspec:\n  amount: \"1\"\n",
		} {
			o2 := o
			o2.Lookup = &stubLookup{
				providers: map[string]*v1.Provider{"openai": {}, "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y": {}},
				budgets:   map[string]*v1.Budget{"bud_01J9ZK2P7Q8R9S0T1U2V3W4X61": {}},
			}
			mustResolve(t, body, o2)
		}
	})
}

func annotationsOf(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("    k" + itoa(i) + ": \"" + strings.Repeat("x", 4096) + "\"\n")
	}
	return b.String()
}

func headersOf(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("    X-H" + itoa(i) + ": v\n")
	}
	return b.String()
}

func selectors(n int) string {
	parts := make([]string, n)
	for i := range n {
		parts[i] = "m" + itoa(i)
	}
	return strings.Join(parts, ", ")
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestExclusiveMissingAndDuplicates(t *testing.T) {
	o := corpusOptions(t)
	o.FileMode = true // so both valueFrom forms are otherwise admitted
	cases := []struct {
		name, body string
		code       Code
		paths      []string
	}{
		{"ttl with expiresAt", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  ttl: 1h\n  expiresAt: 2027-01-01T00:00:00Z\n", CodeExclusiveFields, []string{"spec.ttl", "spec.expiresAt"}},
		{"credential value with valueFrom", head(v1.KindProvider, "p") + minProvider + "  credential:\n    value: x\n    valueFrom: {env: X}\n", CodeExclusiveFields, []string{"spec.credential.value", "spec.credential.valueFrom.env"}},
		{"key value with valueFrom", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  value: " + strings.Repeat("v", 32) + "\n  valueFrom: {env: X}\n", CodeExclusiveFields, []string{"spec.value", "spec.valueFrom.env"}},
		{"tunnel with baseURL", head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n  tunnel: true\n  baseURL: https://a.example.com\n", CodeExclusiveFields, []string{"spec.tunnel", "spec.baseURL"}},
		{"tunnel with credential value", head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n  tunnel: true\n  credential: {value: x}\n", CodeExclusiveFields, []string{"spec.tunnel", "spec.credential"}},
		{"tunnel with credential valueFrom", head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n  tunnel: true\n  credential: {valueFrom: {env: X}}\n", CodeExclusiveFields, []string{"spec.tunnel", "spec.credential"}},
		{"tunnel with an empty credential", head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n  tunnel: true\n  credential: {}\n", CodeExclusiveFields, []string{"spec.tunnel", "spec.credential"}},
		{"duplicate target", head(v1.KindModel, "m") + "spec:\n  targets:\n    - {provider: openai, model: gpt-5}\n    - {provider: azure, model: gpt-5}\n    - {provider: openai, model: gpt-5, priority: 1}\n", CodeDuplicateTarget, []string{"spec.targets[2]"}},
		{"duplicate target by the default model", head(v1.KindModel, "gpt-5") + "spec:\n  targets:\n    - {provider: openai}\n    - {provider: openai, model: gpt-5}\n", CodeDuplicateTarget, []string{"spec.targets[1]"}},
		{"missing dialect", head(v1.KindProvider, "p") + "spec:\n  baseURL: https://a.example.com\n", CodeMissingField, []string{"spec.dialect"}},
		{"missing baseURL", head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n", CodeMissingField, []string{"spec.baseURL"}},
		{"missing targets", head(v1.KindModel, "m") + "spec: {}\n", CodeMissingField, []string{"spec.targets"}},
		{"empty targets", head(v1.KindModel, "m") + "spec:\n  targets: []\n", CodeMissingField, []string{"spec.targets"}},
		{"missing target provider", head(v1.KindModel, "m") + "spec:\n  targets:\n    - {model: x}\n", CodeMissingField, []string{"spec.targets[0].provider"}},
		{"missing pricing input", head(v1.KindModel, "m") + minModel + "  pricing: {output: \"1\"}\n", CodeMissingField, []string{"spec.pricing.input"}},
		{"missing pricing output", head(v1.KindModel, "m") + minModel + "  pricing: {input: \"1\"}\n", CodeMissingField, []string{"spec.pricing.output"}},
		{"missing models", head(v1.KindKey, "k") + "spec: {}\n", CodeMissingField, []string{"spec.models"}},
		{"missing spend amount", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  limits:\n    spend: {window: 24h}\n", CodeMissingField, []string{"spec.limits.spend.amount"}},
		{"missing spend window", head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  limits:\n    spend: {amount: \"1\"}\n", CodeMissingField, []string{"spec.limits.spend.window"}},
		{"missing budget amount", head(v1.KindBudget, "b") + "spec: {}\n", CodeMissingField, []string{"spec.amount"}},
		{"reserved header Host", head(v1.KindProvider, "p") + minProvider + "  headers: {Host: x}\n", CodeReservedPrefix, []string{`spec.headers["Host"]`}},
		{"reserved header Content-Length", head(v1.KindProvider, "p") + minProvider + "  headers: {content-length: \"1\"}\n", CodeReservedPrefix, []string{`spec.headers["content-length"]`}},
		{"reserved header hop-by-hop", head(v1.KindProvider, "p") + minProvider + "  headers: {Transfer-Encoding: chunked}\n", CodeReservedPrefix, []string{`spec.headers["Transfer-Encoding"]`}},
		{"reserved header the dialect credential", head(v1.KindProvider, "p") + minProvider + "  headers: {authorization: Bearer x}\n", CodeReservedPrefix, []string{`spec.headers["authorization"]`}},
		{"reserved header the given credential", head(v1.KindProvider, "p") + minProvider + "  credential: {header: api-key}\n  headers: {Api-Key: x}\n", CodeReservedPrefix, []string{`spec.headers["Api-Key"]`}},
		{"reserved label", "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: b\n  labels:\n    lux.latere.ai/managed: \"true\"\nspec:\n  amount: \"1\"\n", CodeReservedPrefix, []string{`metadata.labels["lux.latere.ai/managed"]`}},
		{"reserved annotation", "apiVersion: lux.latere.ai/v1beta1\nkind: Budget\nmetadata:\n  name: b\n  annotations:\n    lux.latere.ai/note: x\nspec:\n  amount: \"1\"\n", CodeReservedPrefix, []string{`metadata.annotations["lux.latere.ai/note"]`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantErr(t, resolveErr(t, c.body, o), c.code, c.paths...)
		})
	}
}

func TestUpstreamHostRule(t *testing.T) {
	o := corpusOptions(t)
	o.PublicURL, _ = url.Parse("https://lux.example.com:8443/")
	provider := func(u string) string {
		return head(v1.KindProvider, "p") + "spec:\n  dialect: openai\n  baseURL: \"" + u + "\"\n"
	}
	cases := []struct {
		url     string
		private bool // admitted only with AllowPrivateUpstreams
		refused bool // refused in both modes
		detail  string
	}{
		{"https://api.example.com/v1", false, false, ""},
		{"https://api.example.com", false, false, ""},
		{"https://api.example.com:8443/v1/", false, false, ""},
		{"https://203.0.113.10/v1", false, false, ""},
		{"https://[2001:db8::10]/v1", false, false, ""},
		{"HTTPS://API.Example.COM/v1", false, false, ""},
		{"https://api.example.com./v1", false, false, ""},
		{"https://ollama/v1", true, false, "single-label"},
		{"https://localhost:11434/v1", true, false, "single-label"},
		{"https://127.0.0.1:11434/v1", true, false, "loopback"},
		{"https://[::1]/v1", true, false, "loopback"},
		{"https://169.254.169.254/latest", true, false, "link-local"},
		{"https://[fe80::1]/v1", true, false, "link-local"},
		{"https://10.0.0.5/v1", true, false, "private"},
		{"https://192.168.1.20:8080/v1", true, false, "private"},
		{"https://[fd00::1]/v1", true, false, "private"},
		{"https://models.local/v1", true, false, ".local"},
		{"https://models.internal/v1", true, false, ".internal"},
		{"http://api.example.com/v1", true, false, "plaintext"},
		{"http://127.0.0.1:11434/v1", true, false, "plaintext http:// address on a loopback"},
		{"https://user:pw@api.example.com/v1", false, true, "userinfo"},
		{"https://api.example.com/v1?x=1", false, true, "query"},
		{"https://api.example.com/v1?", false, true, "query"},
		{"https://api.example.com/v1#frag", false, true, "fragment"},
		{"ftp://api.example.com/v1", false, true, "scheme"},
		{"api.example.com/v1", false, true, "absolute URL"},
		{"https:///v1", false, true, "absolute URL"},
		{"mailto:ops@example.com", false, true, "absolute URL"},
		{"https://0.0.0.0/v1", false, true, "unicast"},
		{"https://224.0.0.1/v1", false, true, "unicast"},
		{"https://api_example.com/v1", false, true, "not a host name"},
		{"https://a..example.com/v1", false, true, "not a host name"},
		{"https://1.2.3/v1", false, true, "not a host name"},
		{"https://lux.example.com/v1", false, true, "own host"},
		{"https://LUX.example.com:9000/openai", false, true, "own host"},
		{"http://lux.example.com/v1", false, true, "own host"},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			strict, permissive := o, o
			permissive.AllowPrivateUpstreams = true
			switch {
			case c.refused:
				for _, opts := range []Options{strict, permissive} {
					e := resolveErr(t, provider(c.url), opts)
					wantErr(t, e, CodeInvalidField, "spec.baseURL")
					if !strings.Contains(e.Detail, c.detail) {
						t.Errorf("detail %q lacks %q", e.Detail, c.detail)
					}
				}
			case c.private:
				e := resolveErr(t, provider(c.url), strict)
				wantErr(t, e, CodeInvalidField, "spec.baseURL")
				if !strings.Contains(e.Detail, c.detail) || !strings.Contains(e.Detail, "LUX_UPSTREAM_ALLOW_PRIVATE") {
					t.Errorf("detail %q lacks %q or the variable", e.Detail, c.detail)
				}
				r := mustResolve(t, provider(c.url), permissive)
				if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "private upstreams") {
					t.Errorf("warnings = %q, want one about private upstreams", r.Warnings)
				}
				if !strings.Contains(r.Warnings[0], c.detail) {
					t.Errorf("warning %q does not say %q", r.Warnings[0], c.detail)
				}
			default:
				for _, opts := range []Options{strict, permissive} {
					r := mustResolve(t, provider(c.url), opts)
					if len(r.Warnings) != 0 {
						t.Errorf("warnings = %q", r.Warnings)
					}
				}
			}
		})
	}
	t.Run("no PublicURL refuses no host", func(t *testing.T) {
		o2 := o
		o2.PublicURL = nil
		mustResolve(t, provider("https://lux.example.com/v1"), o2)
	})
}

func TestValueFromIsFileModeOnly(t *testing.T) {
	o := corpusOptions(t)
	provider := head(v1.KindProvider, "p") + minProvider + "  credential:\n    valueFrom: {env: LUX_PROVIDER_P_CREDENTIAL}\n"
	key := head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  valueFrom: {env: LUX_KEY_K_VALUE}\n"
	e := resolveErr(t, provider, o)
	wantErr(t, e, CodeInvalidField, "spec.credential.valueFrom.env")
	if !strings.Contains(e.Detail, "file mode") {
		t.Errorf("detail %q", e.Detail)
	}
	wantErr(t, resolveErr(t, key, o), CodeInvalidField, "spec.valueFrom.env")
	o.FileMode = true
	p := mustResolve(t, provider, o).Object.(*v1.Provider)
	if p.Spec.Credential.ValueFrom == nil || p.Spec.Credential.ValueFrom.Env != "LUX_PROVIDER_P_CREDENTIAL" {
		t.Errorf("credential = %+v", p.Spec.Credential)
	}
	if _, set := p.Spec.Credential.Value(); set {
		t.Error("a valueFrom credential has a value")
	}
	k := mustResolve(t, key, o).Object.(*v1.Key)
	if k.Spec.ValueFrom == nil || k.Spec.ValueFrom.Env != "LUX_KEY_K_VALUE" {
		t.Errorf("valueFrom = %+v", k.Spec.ValueFrom)
	}
	// The encoded object carries the variable's name and no value.
	j, _ := json.Marshal(p)
	if !strings.Contains(string(j), `"valueFrom":{"env":"LUX_PROVIDER_P_CREDENTIAL"}`) {
		t.Errorf("encoding %s", j)
	}
}

func TestSuppliedValueSchema(t *testing.T) {
	o := corpusOptions(t)
	key := func(value string, extra string) string {
		return head(v1.KindKey, "k") + "spec:\n  models: [gpt-5]\n  value: \"" + value + "\"\n" + extra
	}
	v32 := strings.Repeat("v", 32)
	cases := []struct {
		name  string
		body  string
		opts  func(Options) Options
		code  Code
		paths []string
	}{
		{"32 bytes resolves", key(v32, ""), nil, "", nil},
		{"4096 bytes resolves", key(strings.Repeat("v", 4096), ""), nil, "", nil},
		{"31 bytes", key(strings.Repeat("v", 31), ""), nil, CodeInvalidField, []string{"spec.value"}},
		{"4097 bytes", key(strings.Repeat("v", 4097), ""), nil, CodeInvalidField, []string{"spec.value"}},
		{"empty", key("", ""), nil, CodeInvalidField, []string{"spec.value"}},
		{"with valueFrom", key(v32, "  valueFrom: {env: X}\n"), func(o Options) Options { o.FileMode = true; return o }, CodeExclusiveFields, []string{"spec.value", "spec.valueFrom.env"}},
		{"in file mode", key(v32, ""), func(o Options) Options { o.FileMode = true; return o }, CodeInvalidField, []string{"spec.value"}},
		{"on an update, equal to the stored", key(v32, ""), withExisting(t, o, key(v32, "")), CodeImmutableField, []string{"spec.value"}},
		{"on an update, another value", key(strings.Repeat("w", 32), ""), withExisting(t, o, key(v32, "")), CodeImmutableField, []string{"spec.value"}},
		{"on an update, out of range", key("short", ""), withExisting(t, o, key(v32, "")), CodeImmutableField, []string{"spec.value"}},
		{"an update without a value", head(v1.KindKey, "k") + minKey, withExisting(t, o, key(v32, "")), "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o2 := o
			if c.opts != nil {
				o2 = c.opts(o2)
			}
			if c.code == "" {
				r := mustResolve(t, c.body, o2)
				k := r.Object.(*v1.Key)
				j, _ := json.Marshal(k)
				y, _ := yaml.Marshal(k)
				if strings.Contains(string(j), "vvvv") || strings.Contains(string(y), "vvvv") {
					t.Errorf("an encoding carries the value:\n%s\n%s", j, y)
				}
				if v, set := k.Spec.Value(); c.body != head(v1.KindKey, "k")+minKey && (!set || !strings.HasPrefix(v, "vvvv")) {
					t.Errorf("Value() = %q %v", v, set)
				}
				return
			}
			wantErr(t, resolveErr(t, c.body, o2), c.code, c.paths...)
		})
	}
}

// withExisting resolves body under o and returns an option setter that
// makes the result the existing object, as a store would hold it.
func withExisting(t *testing.T, o Options, body string) func(Options) Options {
	t.Helper()
	existing := mustResolve(t, body, o).Object
	if k, ok := existing.(*v1.Key); ok {
		k.Spec.ClearValue() // the store keeps a hash, never the value
	}
	return func(o Options) Options {
		o.Existing = existing
		return o
	}
}
