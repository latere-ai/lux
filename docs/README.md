# Documentation

For people who run `luxd`, write manifests, or build a platform on the
packages.

## Running it

| Page | |
|---|---|
| Quick start | the published images on your machine with no build, `docker compose up` and a few `curl`s, in [`quickstart.md`](quickstart.md); the wiring is [`compose.yaml`](../compose.yaml) |
| Install | from nothing to a request through a door on a kind cluster, in [`install.md`](install.md); the manifests it applies are [`deploy/`](../deploy/README.md), and upgrades and rollback are in [`upgrades/`](upgrades/README.md) |
| Configuration | every `LUX_*` variable `luxd` reads, its meaning, default, and when it is required, in [`configuration.md`](configuration.md); [`.env.example`](../.env.example) is the same set as a file to copy |
| Security | what the gateway protects and what you must do to run it safely, the KEK, TLS, trusted proxies, the authorizer, key rotation, and verifying releases, in [`security.md`](security.md); how to report a vulnerability, in [`SECURITY.md`](../SECURITY.md) |
| Observability | the `/metrics` endpoint and the metrics worth watching, the health probes, the log fields, traces, and the shipped alerts, in [`observability.md`](observability.md); the rules file is [`deploy/base/prometheusrule.yaml`](../deploy/base/prometheusrule.yaml) |
| Performance | what the gateway's own overhead costs and how to measure it, in [`performance.md`](performance.md); the design behind the benchmarks is the [performance and benchmarks spec](../specs/023-performance-and-benchmarks.md) |

Trying it out before there is anything to install takes one command,
`make run`: the gateway on loopback, serving its probes.

## Building against it

| Page | |
|---|---|
| The `lux` command | what `lux -help` prints, command by command, in [`cli.md`](cli.md); the skill that teaches an agent the command is [`skills/lux/SKILL.md`](../skills/lux/SKILL.md) |
| Manifest reference | each kind's fields are the schemas in the OpenAPI document the [API page](api.md) links; `luxd` validates and defaults them, and a `GET` reads back what runs |
| API | the `/v1` control plane, the dialect doors, authentication, and the error shape, in [`api.md`](api.md); the authoritative reference is the OpenAPI document [`api/openapi.yaml`](../api/openapi.yaml), served at `GET /v1/openapi.json` |
| The packages | the [architecture spec](../specs/001-architecture.md) names the exported packages and what each promises |
| Building a platform on it | the two doors, where each platform concern goes, a minimal authorizer, how a sandbox gets model access without holding a credential, and the conformance command, in [`plane.md`](plane.md); the runnable endpoint is [`examples/authorizer`](../examples/authorizer) and the server built from the packages is [`examples/plane`](../examples/plane) |

## Changing it

The design, and the reasoning behind each decision, is in
[`specs/`](../specs/README.md). [`CONTRIBUTING.md`](../CONTRIBUTING.md) is
how to build and test a change.
