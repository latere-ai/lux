# Documentation

For people who run `luxd`, write manifests, or build a platform on the
packages.

## Running it

| Page | |
|---|---|
| Install | not written yet; owned by the [release and installation spec](../specs/017-release-and-installation.md) |
| Configuration | the table in the [repository scaffold spec](../specs/002-repository-scaffold.md) until `docs/configuration.md` is generated from the code |
| Security | what the design protects and what it does not, in the [threat model](../specs/016-security-and-threat-model.md); how to report a vulnerability, in [`SECURITY.md`](../SECURITY.md) |

Trying it out before there is anything to install takes one command,
`make run`: the gateway on loopback, serving its probes.

## Building against it

| Page | |
|---|---|
| Manifest reference | the schema in the [manifest contract spec](../specs/003-manifest-contract.md) |
| API | the endpoints and error codes in the [API spec](../specs/011-api.md), and the webhooks an operator writes in the [identity spec](../specs/006-identity.md) |
| The packages | the [architecture spec](../specs/001-architecture.md) names the exported packages and what each promises |

## Changing it

The design, and the reasoning behind each decision, is in
[`specs/`](../specs/README.md). [`CONTRIBUTING.md`](../CONTRIBUTING.md) is
how to build and test a change.
