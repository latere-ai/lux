# Documentation

For people who run `luxd`, write manifests, or build a platform on the
packages.

## Running it

| Page | |
|---|---|
| Install | not written yet; owned by the [release and installation spec](../specs/017-release-and-installation.md) |
| Configuration | the table in the [repository scaffold spec](../specs/002-repository-scaffold.md) until `docs/configuration.md` is generated from the code |
| Security | what the design protects and what it does not, in the [threat model](../specs/016-security-and-threat-model.md); how to report a vulnerability, in [`SECURITY.md`](../SECURITY.md) |
| Observability | the metrics on `/metrics`, the spans, the log fields, and the alerts to start with, in the [observability spec](../specs/019-observability.md); the rules file is [`deploy/base/prometheusrule.yaml`](../deploy/base/prometheusrule.yaml) |

Trying it out before there is anything to install takes one command,
`make run`: the gateway on loopback, serving its probes.

## Building against it

| Page | |
|---|---|
| The `lux` command | what `lux -help` prints, command by command, in [`cli.md`](cli.md); the skill that teaches an agent the command is [`skills/lux/SKILL.md`](../skills/lux/SKILL.md) |
| Manifest reference | the schema in the [manifest contract spec](../specs/003-manifest-contract.md) |
| API | the endpoints and error codes in the [API spec](../specs/011-api.md), and the webhooks an operator writes in the [identity spec](../specs/006-identity.md) |
| The packages | the [architecture spec](../specs/001-architecture.md) names the exported packages and what each promises |
| Building a platform on it | the two doors, where each platform concern goes, a minimal authorizer, how a sandbox gets model access without holding a credential, and the conformance command, in [`plane.md`](plane.md); the runnable endpoint is [`examples/authorizer`](../examples/authorizer) and the server built from the packages is [`examples/plane`](../examples/plane) |

## Changing it

The design, and the reasoning behind each decision, is in
[`specs/`](../specs/README.md). [`CONTRIBUTING.md`](../CONTRIBUTING.md) is
how to build and test a change.
