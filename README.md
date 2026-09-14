# Lux

**An open source LLM gateway.** One address serves every model
provider's API. A manifest in the shape of a Kubernetes object declares
the upstreams, the routable model names, the credentials a workload
holds, and the spend windows they draw from. `luxd` accepts a request in
the OpenAI, Anthropic, Gemini, or lux-native dialect, resolves the model
name to a target, translates the request into that provider's dialect,
injects the provider credential the caller never sees, streams the
answer back in the dialect it was asked in, and records what it cost.
Identity comes from any OpenID Connect issuer. Permission comes from an
endpoint you write.

Run it standalone, or build a platform on its Go packages and webhooks
instead of forking it.

[![CI](https://github.com/latere-ai/lux/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/lux/actions/workflows/verify.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/lux)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Status: pre-release](https://img.shields.io/badge/status-pre--release-orange.svg)](#project-status)

## The problem

A team that uses more than one model ends up with more than one client
library, more than one request shape, and a provider key in every
service that calls a model. Rotating a key means finding every service
that holds it. Giving a new agent access means handing it a provider
key with no limit on it, or writing a proxy. Knowing what a workload
spent means reconciling four billing pages against four sets of logs,
after the month has closed. Moving a model name from one provider to
another, or splitting it across two, means changing the callers.

Lux makes the provider a document, the model name a routing decision,
and the credential a workload holds a separate object with its own
limits, expiry, and budget.

## How it works

- **One manifest, one meaning.** `apiVersion: lux.latere.ai/v1beta1`,
  and four kinds: `Provider` for an upstream with its dialect, base
  URL, and write-only credential; `Model` for a routable name with
  targets, weights, fallback, and pricing; `Key` for the credential a
  workload holds; `Budget` for a spend window several keys draw from.
  Every surface, the API, the `lux` command, and a platform importing
  the packages, resolves a manifest through one function, and what you
  read back is what runs, defaults included.
- **Every dialect at one address.** The OpenAI, Anthropic, and Gemini
  request shapes are doors into the same gateway, and so is the
  lux-native one. A caller keeps the client library it already has; the
  translation between shapes is
  [`latere.ai/x/pkg/llmdialect`](https://github.com/latere-ai/pkg), a
  package with its own test corpus rather than a branch per provider in
  the request path.
- **The provider credential never leaves the gateway.** A caller holds
  a Key, which is not a provider key: it names the models it may reach
  and the limits it runs under, its value is shown once at creation,
  and revoking it takes effect without touching an upstream. The
  gateway injects the provider credential toward that provider's base
  URL and nowhere else.
- **A model name is a routing decision.** One name, several targets
  with weights, a fallback list when a target fails or is rate
  limited, and the prices the usage record is costed with. Moving a
  name to a second provider is an edit to one `Model`.
- **Every request is accounted.** One usage record per request, with
  the key, the model, the target, the tokens, and the cost, and never
  the content. Budgets are enforced against those records, so a spend
  window is a limit rather than a report.

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: anthropic
spec:
  dialect: anthropic
  baseURL: https://api.anthropic.com
---
apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: sonnet
spec:
  targets:
    - { provider: anthropic, model: claude-sonnet-4-5, weight: 100 }
    - { provider: anthropic-eu, model: claude-sonnet-4-5, weight: 0, priority: 1 }
  fallback: onError
  pricing: { input: "3", output: "15" }   # per million tokens, USD
---
apiVersion: lux.latere.ai/v1beta1
kind: Budget
metadata:
  name: research-q4
spec:
  amount: "500"
  window: month
---
apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: research-agent
spec:
  models: [sonnet, "anthropic/*"]
  limits: { requestsPerMinute: 120, tokensPerMinute: 200000 }
  budget: research-q4
  ttl: 720h
```

```sh
lux apply -f anthropic.yaml --credential-from-env ANTHROPIC_API_KEY
lux apply -f models.yaml -f budget.yaml
lux apply -f research-agent.yaml          # prints the key value once

# the Anthropic SDK's base URL is the /anthropic door; any dialect's SDK works the same way
curl https://lux.example.com/anthropic/v1/messages \
  -H "authorization: Bearer $LUX_KEY" \
  -H "content-type: application/json" \
  -d '{"model":"sonnet","max_tokens":256,
       "messages":[{"role":"user","content":"hello"}]}'
```

## Try it

From a checkout, `make run` builds the gateway and its stubs, starts them
on loopback with every store in memory, applies the manifest above, and
prints `LUX_URL`, `LUX_TOKEN`, and `LUX_KEY` for a first request. The
Key opens any door in the dialect its SDK already speaks:

```sh
curl -sS "$LUX_URL/openai/v1/chat/completions" \
  -H "Authorization: Bearer $LUX_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"stub-openai","messages":[{"role":"user","content":"hello"}]}'
```

`make` runs the quality gate and `make run-down` stops the stack. To
install a release on a cluster, see [`docs/install.md`](docs/install.md).

## What you get

- Four kinds with strict decoding, server-side defaults, and a `status`
  the server writes, evolving under written rules.
- Four dialects on the data plane, translated through one package with
  a golden corpus, and the conformance suite any server must pass.
- Routing by weight and priority, fallback on a failure or a rate
  limit before the first byte, and a health probe per provider that
  takes a target out of rotation.
- Keys with model selectors, per-minute request and token limits, an
  expiry, and a value shown once; budgets several keys draw from.
- Streaming in every dialect, translated as it arrives rather than
  buffered.
- Usage records and per-key, per-model, per-target metering, costed
  from the prices in the `Model`.
- A request log with the bodies kept out of the gateway's own store and
  archived where you point it.
- Provider credentials wrapped by a key-encryption key, write-only
  through the API, never returned through it and never handed to a
  caller.
- State in memory, in Postgres, or read from a directory of manifests
  on disk.
- OIDC from any issuer, an authorizer webhook with a built-in owner
  policy, and a signed event sink.
- A reverse tunnel so a model running on your own machine is a
  `Provider` like any other.
- Go packages a platform imports: `manifest`, `gateway`, `metering`.
- The `lux` command and a skill file that teaches an agent to use it.
- Signed images, SBOMs, and provenance on every release.

## Documentation

| Page | |
|---|---|
| [Specs](specs/README.md) | the design, one spec per component, with the build order |
| [Architecture](specs/001-architecture.md) | the planes, the packages, what the gateway owns and what a platform supplies |
| [Manifest contract](specs/003-manifest-contract.md) | every field, every rule, every error code |
| [Request path](specs/004-request-path.md) | the dialect doors, routing, translation, streaming, credential injection |
| [Keys and limits](specs/007-keys-and-limits.md) | what a `Key` may reach and what a `Budget` stops |
| [Building a plane](specs/020-building-a-plane.md) | how a platform composes the packages and the webhooks |
| [docs/](docs/README.md) | for people who run `luxd` or build against it |

## Project status

Pre-release, built in the open. Every capability above is implemented and
covered by tests, and `main` passes the full quality gate on every
commit. What remains before `v1` is the first tagged release, so there is
no published image or binary to pull yet and the manifest schema may
still change; the [CHANGELOG](CHANGELOG.md) records every change to it.
Until then, run it from a checkout as [Try it](#try-it) shows.

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) is how to build, the bar, and
where a package belongs. [`SECURITY.md`](SECURITY.md) is where to
report a vulnerability.

## License

Apache-2.0. See [LICENSE](LICENSE).
