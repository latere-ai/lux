// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
)

// The operator-facing reference for luxd's environment. referenceGroups is
// the one source docs/configuration.md and .env.example are generated from,
// so the two files, and the code that reads the variables, cannot drift:
// reference_test.go holds both files equal to the generators below and holds
// the names here equal to the getenv calls of this package. The meanings are
// spec 002's, the repository scaffold, which owns them.

// referenceVar is one variable of the reference.
type referenceVar struct {
	// name is the LUX_ variable, as the code reads it.
	name string
	// meaning is a single line, agreeing with spec 002's table; it carries
	// no pipe so it sits in a Markdown table cell.
	meaning string
	// def is the default as the table shows it: a value, "unset" when a
	// blank leaves a feature off, or "none" when the variable is required.
	def string
	// required is when the variable is required, for the table's last column.
	required string
	// envValue is what .env.example writes after "=": a placeholder for a
	// required variable, the default for an optional one, or empty.
	envValue string
	// envRequired writes the line uncommented, for a variable a start needs.
	envRequired bool
	// envNote is a second comment line in .env.example, when one helps.
	envNote string
}

// referenceGroup is a titled run of variables, one Markdown table and one
// .env.example section.
type referenceGroup struct {
	title string
	vars  []referenceVar
}

var referenceGroups = []referenceGroup{
	{title: "Listeners", vars: []referenceVar{
		{
			name:     "LUX_PUBLIC_ADDR",
			meaning:  "Address the public listener binds: the dialect doors, the control plane under `/v1`, and the public probes.",
			def:      ":8080",
			required: "No",
			envValue: ":8080",
		},
		{
			name:     "LUX_INTERNAL_ADDR",
			meaning:  "Address the internal listener binds for the cluster's probes; must differ from `LUX_PUBLIC_ADDR` unless both ask for port 0.",
			def:      ":8081",
			required: "No",
			envValue: ":8081",
		},
		{
			name:        "LUX_PUBLIC_URL",
			meaning:     "Absolute URL callers reach the public listener at; the base of every URL in a response and the loop check of resolve.",
			def:         "none",
			required:    "Yes",
			envValue:    "https://lux.example.com",
			envRequired: true,
		},
	}},
	{title: "Identity", vars: []referenceVar{
		{
			name:        "LUX_OIDC_ISSUERS",
			meaning:     "Comma-separated issuer URLs whose tokens the control plane accepts.",
			def:         "none",
			required:    "Yes, unless file mode",
			envValue:    "https://issuer.example.com",
			envRequired: true,
		},
		{
			name:     "LUX_OIDC_AUDIENCE",
			meaning:  "The one audience a caller token must contain.",
			def:      "lux",
			required: "No",
			envValue: "lux",
		},
		{
			name:     "LUX_OIDC_INSECURE_ISSUERS",
			meaning:  "Issuers from the list allowed to use `http://` off loopback; the test stubs set it, never production.",
			def:      "unset",
			required: "No",
		},
	}},
	{title: "Authorizer", vars: []referenceVar{
		{
			name:     "LUX_AUTHORIZER_URL",
			meaning:  "The operator's authorization endpoint; unset selects the built-in owner policy.",
			def:      "unset",
			required: "No",
		},
		{
			name:     "LUX_AUTHORIZER_TOKEN",
			meaning:  "The bearer luxd sends the authorizer. Never echoed.",
			def:      "unset",
			required: "When `LUX_AUTHORIZER_URL` is set",
			envNote:  "required when LUX_AUTHORIZER_URL is set",
		},
		{
			name:     "LUX_AUTHORIZER_TIMEOUT",
			meaning:  "One authorization decision's deadline, the retry included.",
			def:      "5s",
			required: "No",
			envValue: "5s",
		},
		{
			name:     "LUX_ADMIN_SUBJECTS",
			meaning:  "Comma-separated subjects the built-in owner policy lets act on every object; read and unused when an authorizer is set.",
			def:      "unset",
			required: "No",
		},
	}},
	{title: "Store", vars: []referenceVar{
		{
			name:     "LUX_MANIFEST_DIR",
			meaning:  "A directory of manifests read at start: file mode, where desired state comes from disk and its kinds are read-only through the API.",
			def:      "unset",
			required: "No",
		},
		{
			name:     "LUX_DB_URL",
			meaning:  "A `postgres://` URL selecting the Postgres store; unset keeps every state in memory. Never echoed.",
			def:      "unset",
			required: "No",
		},
		{
			name:     "LUX_DB_MAX_CONNS",
			meaning:  "The Postgres pool size; read only with `LUX_DB_URL`, between 1 and 100.",
			def:      "8",
			required: "No",
			envValue: "8",
		},
	}},
	{title: "Keys and limits", vars: []referenceVar{
		{
			name:     "LUX_KEY_CACHE",
			meaning:  "How long a Key lookup, positive or negative, is cached per replica on the data plane (1s to 10m).",
			def:      "10s",
			required: "No",
			envValue: "10s",
		},
		{
			name:     "LUX_DEFAULT_REQUESTS_PER_MINUTE",
			meaning:  "The requests-per-minute a Key gets when it names none; 0 is no limit.",
			def:      "0",
			required: "No",
			envValue: "0",
		},
		{
			name:     "LUX_DEFAULT_TOKENS_PER_MINUTE",
			meaning:  "The tokens-per-minute a Key gets when it names none; 0 is no limit.",
			def:      "0",
			required: "No",
			envValue: "0",
		},
		{
			name:     "LUX_REQUESTS_PER_MINUTE",
			meaning:  "Control-plane requests one subject may send in a minute; 0 is no limit.",
			def:      "600",
			required: "No",
			envValue: "600",
		},
		{
			name:     "LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE",
			meaning:  "Requests one client address may send before authentication, on both planes; 0 is no limit.",
			def:      "60",
			required: "No",
			envValue: "60",
		},
		{
			name:     "LUX_TRUSTED_PROXIES",
			meaning:  "Comma-separated CIDR ranges of front proxies whose `X-Forwarded-For` names the client; unset trusts no header.",
			def:      "unset",
			required: "No",
		},
		{
			name:     "LUX_MAX_MANIFEST_BYTES",
			meaning:  "The largest manifest or JSON body accepted on the control plane (1Ki to 16Mi).",
			def:      "64Ki",
			required: "No",
			envValue: "64Ki",
		},
		{
			name:     "LUX_MAX_BODY_BYTES",
			meaning:  "The largest data-plane request body, and the cap on an upstream body read whole (4Ki to 1Gi).",
			def:      "64Mi",
			required: "No",
			envValue: "64Mi",
		},
		{
			name:     "LUX_METERING_FLUSH",
			meaning:  "How often a replica writes its spend deltas and usage aggregates to the store (100ms to 1m).",
			def:      "1s",
			required: "No",
			envValue: "1s",
		},
	}},
	{title: "Providers and upstream", vars: []referenceVar{
		{
			name:     "LUX_UPSTREAM_ALLOW_PRIVATE",
			meaning:  "1 admits a Provider base URL, and the address it resolves to, on a loopback, link-local, or private network.",
			def:      "unset",
			required: "No",
			envValue: "1",
		},
		{
			name:     "LUX_UPSTREAM_TIMEOUT",
			meaning:  "The deadline of one upstream request including its stream (1s to 1h).",
			def:      "10m",
			required: "No",
			envValue: "10m",
		},
		{
			name:     "LUX_DISCOVERY_INTERVAL",
			meaning:  "How often a Provider's model list is refreshed (1m to 24h).",
			def:      "1h",
			required: "No",
			envValue: "1h",
		},
		{
			name:     "LUX_HEALTH_INTERVAL",
			meaning:  "How often a Provider is health-probed (5s to 10m).",
			def:      "30s",
			required: "No",
			envValue: "30s",
		},
	}},
	{title: "Events and request log", vars: []referenceVar{
		{
			name:     "LUX_EVENTS_URL",
			meaning:  "The event sink; unset turns events off. Reached over `https://` or on loopback.",
			def:      "unset",
			required: "No",
		},
		{
			name:     "LUX_EVENTS_SECRET",
			meaning:  "The HMAC-SHA256 key every delivery's `Lux-Signature` is signed with. Never echoed.",
			def:      "unset",
			required: "When `LUX_EVENTS_URL` is set",
			envNote:  "required when LUX_EVENTS_URL is set",
		},
		{
			name:     "LUX_REQUESTLOG_EXPORTER",
			meaning:  "Where the request log is archived: `none` or `s3`.",
			def:      "none",
			required: "No",
			envValue: "none",
		},
		{
			name:     "LUX_S3_ENDPOINT",
			meaning:  "The S3-compatible endpoint URL of the request-log archive; there is no default endpoint.",
			def:      "unset",
			required: "When exporter is `s3`",
			envNote:  "required when LUX_REQUESTLOG_EXPORTER is s3",
		},
		{
			name:     "LUX_S3_REGION",
			meaning:  "The request-log archive's region.",
			def:      "us-east-1",
			required: "No",
			envValue: "us-east-1",
		},
		{
			name:     "LUX_S3_BUCKET",
			meaning:  "The request-log archive's bucket.",
			def:      "unset",
			required: "When exporter is `s3`",
			envNote:  "required when LUX_REQUESTLOG_EXPORTER is s3",
		},
		{
			name:     "LUX_S3_ACCESS_KEY",
			meaning:  "The request-log archive's access key; there is no credential chain. Never echoed.",
			def:      "unset",
			required: "When exporter is `s3`",
			envNote:  "required when LUX_REQUESTLOG_EXPORTER is s3",
		},
		{
			name:     "LUX_S3_SECRET_KEY",
			meaning:  "The request-log archive's secret key. Never echoed.",
			def:      "unset",
			required: "When exporter is `s3`",
			envNote:  "required when LUX_REQUESTLOG_EXPORTER is s3",
		},
		{
			name:     "LUX_S3_PREFIX",
			meaning:  "The key prefix request-log objects are written under.",
			def:      "lux/",
			required: "No",
			envValue: "lux/",
		},
	}},
	{title: "Tunnel", vars: []referenceVar{
		{
			name:     "LUX_TUNNEL_ENABLED",
			meaning:  "1 serves the tunnel routes and admits `spec.tunnel`.",
			def:      "unset",
			required: "No",
			envValue: "1",
		},
		{
			name:     "LUX_TUNNEL_REGISTRY_TTL",
			meaning:  "The liveness window of a registry row; the agent heartbeats at a third of it (5s to 5m).",
			def:      "30s",
			required: "No",
			envValue: "30s",
		},
		{
			name:     "LUX_TUNNEL_FORWARD_ADDR",
			meaning:  "The `host:port` other replicas reach this one's internal listener at; unset serves a tunnelled Provider on the holding replica only.",
			def:      "unset",
			required: "No",
		},
		{
			name:     "LUX_TUNNEL_FORWARD_SECRET",
			meaning:  "Comma-separated bearers of the forward route: the first is sent, every one is accepted, so a rotation is prepending. Each at least 32 bytes. Never echoed.",
			def:      "unset",
			required: "When `LUX_TUNNEL_FORWARD_ADDR` is set",
			envNote:  "required when LUX_TUNNEL_FORWARD_ADDR is set",
		},
	}},
	{title: "Secrets", vars: []referenceVar{
		{
			name:        "LUX_SECRETS_KEK",
			meaning:     "One to eight 32-byte keys, standard base64, comma separated; the first wraps every new data key, every key is tried to open one, so rotation is prepending a key and running `luxd rewrap`. Never echoed.",
			def:         "none",
			required:    "Yes for serve and rewrap, except file mode",
			envRequired: true,
			envNote:     "generate a key: head -c 32 /dev/urandom | base64",
		},
	}},
}

// ReferenceDoc renders docs/configuration.md from referenceGroups.
func ReferenceDoc() string {
	var b strings.Builder
	b.WriteString(`# luxd configuration

Every variable ` + "`luxd`" + ` reads from the environment, read once at
start-up by ` + "`internal/config`" + `. A blank value is unset, and an unknown
variable is never an error, so a deployment that sets one before its feature
lands is not refused. A start-up with anything missing or malformed fails with
one message that lists every problem, sorted by variable name, so an operator
fixes a deployment in one round.

This page and the ` + "`.env.example`" + ` at the repository root are
generated from ` + "`internal/config`" + ` and held current by
` + "`TestConfigurationReferenceIsCurrent`" + `, so they never drift from the
code; ` + "`.env.example`" + ` is this same set as a file to copy.

Variables that are not luxd's live elsewhere: the ` + "`lux`" + ` command's own
in [` + "`cli.md`" + `](cli.md), the install script's ` + "`LUX_INSTALL_*`" + ` in
[` + "`install.md`" + `](install.md), the conformance suite's ` + "`LUX_TEST_*`" + `
in the [conformance spec](../specs/018-conformance-suite.md), and the
OpenTelemetry exporter's ` + "`OTEL_*`" + `, read by ` + "`latere.ai/x/pkg/otel`" + `,
in [` + "`observability.md`" + `](observability.md). In file mode a
manifest may name the operator's own variables, outside the ` + "`LUX_`" + `
namespace, in ` + "`credential.valueFrom.env`" + ` or ` + "`Key.spec.valueFrom.env`" + `.
`)
	for _, g := range referenceGroups {
		b.WriteString("\n## " + g.title + "\n\n")
		b.WriteString("| Variable | Meaning | Default | When required |\n")
		b.WriteString("|---|---|---|---|\n")
		for _, v := range g.vars {
			b.WriteString("| `" + v.name + "` | " + v.meaning + " | `" + v.def + "` | " + v.required + " |\n")
		}
	}
	return b.String()
}

// EnvExample renders .env.example from referenceGroups: required variables
// uncommented with a placeholder, optional ones commented out with their
// default, and a comment per variable.
func EnvExample() string {
	var b strings.Builder
	b.WriteString(`# luxd configuration. Copy to .env, edit, and load it into the process
# environment (docker run --env-file .env, a Compose env_file, or your
# supervisor). A blank value is unset; an unknown variable is ignored.
#
# Required variables are uncommented with a placeholder; edit each one. Optional
# variables are commented out with their default; uncomment to override.
#
# Generated from internal/config; the full reference is docs/configuration.md.
# It lists every LUX_* variable luxd reads and no others: the lux command's own
# are in docs/cli.md, the install script's LUX_INSTALL_* in docs/install.md, and
# the conformance suite's LUX_TEST_* in the conformance spec.
`)
	for _, g := range referenceGroups {
		b.WriteString("\n# --- " + g.title + " ---\n")
		for _, v := range g.vars {
			b.WriteString("\n# " + v.meaning + "\n")
			if v.envNote != "" {
				b.WriteString("# " + v.envNote + "\n")
			}
			line := v.name + "=" + v.envValue
			if !v.envRequired {
				line = "# " + line
			}
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// referenceNames is every variable name in referenceGroups, the set the
// reference documents.
func referenceNames() []string {
	var out []string
	for _, g := range referenceGroups {
		for _, v := range g.vars {
			out = append(out, v.name)
		}
	}
	return out
}
