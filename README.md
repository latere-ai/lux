# Lux

**An open source LLM gateway.** Put one HTTP endpoint in front of every
model provider, and let each workload reach models through a credential
you issue and revoke rather than a provider key you copy into services.

Lux is a single Go server, `luxd`. You declare your providers, model
names, credentials, and spend limits as Kubernetes-style manifests, and
`luxd` serves them. A request arrives in the OpenAI, Anthropic, Gemini,
or lux-native dialect; Lux resolves the model name to a provider,
translates the request into that provider's dialect, adds the provider
credential the caller never sees, streams the reply back in the dialect
it was asked in, and records what it cost. Identity comes from any
OpenID Connect issuer; permission comes from an endpoint you write.

Run `luxd` on its own, or import its Go packages and drive them from
your own control plane instead of forking it.

[![CI](https://github.com/latere-ai/lux/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/lux/actions/workflows/verify.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/latere-ai/lux)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Status: pre-release](https://img.shields.io/badge/status-pre--release-orange.svg)](#project-status)

## The problem

A team that uses more than one model ends up with more than one client
library, more than one request shape, and a provider key in every
service that calls a model. Rotating a key means finding every service
that holds it. Giving a new agent access means handing it a provider key
with no limit on it, or writing a proxy. Knowing what a workload spent
means reconciling four billing pages against four sets of logs, after
the month has closed. Moving a model name from one provider to another,
or splitting it across two, means changing the callers.

Lux makes the provider a document, the model name a routing decision,
and the credential a workload holds a separate object with its own
limits, expiry, and budget.

## How it works

- **One manifest, one meaning.** Everything is a Kubernetes-style object
  under `apiVersion: lux.latere.ai/v1beta1`, in four kinds: `Provider`
  is an upstream with its dialect, base URL, and write-only credential;
  `Model` is a routable name with targets, weights, fallback, and
  pricing; `Key` is the credential a workload holds; `Budget` is a spend
  window several keys draw from. The `/v1` API, the `lux` command, and a
  platform importing the packages all resolve a manifest through the same
  function, so what you read back is what runs, defaults included.
- **Every dialect at one address.** The OpenAI, Anthropic, and Gemini
  request shapes are doors into the same gateway, and so is the
  lux-native one. A caller keeps the SDK it already has; the translation
  between shapes lives in one package with its own golden test corpus,
  [`latere.ai/x/pkg/llmdialect`](https://github.com/latere-ai/pkg),
  rather than a branch per provider in the request path.
- **The provider credential never leaves the gateway.** A `Key` is not a
  provider key: it names the models it may reach and the limits it runs
  under, its value is shown once at creation, and revoking it takes
  effect without touching an upstream. `luxd` injects the provider's own
  credential toward that provider's base URL and nowhere else.
- **A model name is a routing decision.** One name, several targets with
  weights, a fallback order for when a target fails or is rate limited,
  and the prices each request is costed against. Moving a name to a
  second provider is an edit to one `Model`, not a change to the
  callers.
- **Every request is accounted.** One usage record per request, with the
  key, the model, the target, the tokens, and the cost, and never the
  prompt or the completion. Budgets are enforced against those records,
  so a spend window is a limit rather than a monthly surprise.

A small deployment as a manifest: a model that fails over between two
regions, a monthly budget, and a key an agent holds.

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

Apply it with the `lux` command, then call the gateway with the SDK you
already use.

```sh
lux apply -f anthropic.yaml --credential-from-env ANTHROPIC_API_KEY
lux apply -f models.yaml -f budget.yaml
lux apply -f research-agent.yaml          # prints the key value once

# the Anthropic SDK's base URL is the /anthropic door; any dialect's SDK is the same
curl https://lux.example.com/anthropic/v1/messages \
  -H "authorization: Bearer $LUX_KEY" \
  -H "content-type: application/json" \
  -d '{"model":"sonnet","max_tokens":256,
       "messages":[{"role":"user","content":"hello"}]}'
```

## Try it

From a checkout, `make run` builds `luxd` and a set of stubs, starts them
on loopback with all state in memory, applies the manifest above, and
prints `LUX_URL`, `LUX_TOKEN`, and `LUX_KEY` for a first request. The key
opens any door in the dialect that door's SDK speaks:

```sh
curl -sS "$LUX_URL/openai/v1/chat/completions" \
  -H "Authorization: Bearer $LUX_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"stub-openai","messages":[{"role":"user","content":"hello"}]}'
```

Without a checkout, [`compose.yaml`](compose.yaml) runs the published
`luxd` and stub images the same way: `docker compose up`, then the
requests in [the quick start](docs/quickstart.md) to mint a token,
declare a provider, a model, and a key, and open a door. The images are
cut by the release pipeline, so until the first tag they are built from a
checkout, which the quick start shows.

`make` runs the quality gate and `make run-down` stops the stack. To
install a release on a cluster, see [Install](docs/install.md).

## What you get

- Four kinds with strict decoding, server-side defaults, and a `status`
  the server writes, evolving under written compatibility rules.
- Four dialects on the data plane, translated through one package with a
  golden corpus, and a conformance suite any server must pass.
- Routing by weight and priority, fallback on a failure or rate limit
  before the first byte, and a per-provider health probe that takes a
  target out of rotation.
- Keys with model selectors, per-minute request and token limits, an
  expiry, and a value shown once; budgets several keys draw from.
- Streaming in every dialect, translated as it arrives rather than
  buffered.
- Usage records and per-key, per-model, per-target metering, costed from
  the prices on the `Model`.
- A request log with the bodies kept out of the gateway's own store and
  archived where you point it.
- Provider credentials wrapped by a key-encryption key, write-only
  through the API, never read back and never handed to a caller.
- State in memory, in Postgres, or read from a directory of manifests on
  disk.
- OIDC from any issuer, an authorizer webhook with a built-in owner
  policy, and a signed event stream.
- A reverse tunnel, so a model on your own machine is a `Provider` like
  any other.
- The Go packages a platform imports: `manifest`, `gateway`, `metering`,
  `client`, the typed client of the `/v1` API, `client/tunnel` for attaching
  a local runtime, and `authorizer`, the
  actions and resource shapes an authorizer is written against.
- The `lux` command, and a skill file that teaches an agent to drive it.
- Benchmarks that measure the gateway's own overhead per request, in
  process against a stub upstream, so what Lux adds on top of a provider
  call is a number and not a guess.
- Signed images, SBOMs, and build provenance on every release.

## Documentation

For running Lux and building on it:

| | |
|---|---|
| [Quick start](docs/quickstart.md) | the published images on your machine, no build: `docker compose up` and a few requests to a running gateway |
| [Install](docs/install.md) | from an empty cluster to a request through a door |
| [The `lux` command](docs/cli.md) | every command and flag, for operating a gateway from a shell |
| [Configuration](docs/configuration.md) | every `LUX_*` variable `luxd` reads, its default, and when it is required; `.env.example` is the same set to copy |
| [Building a platform](docs/plane.md) | compose the packages and the webhooks, and give a workload model access without handing it a credential |
| [Performance](docs/performance.md) | what the gateway's own overhead costs per request, and how to measure it |
| [All documentation](docs/README.md) | the full index for operators and platform builders |

## Contributing

Lux's design, and the reasoning behind each decision, lives in
[`specs/`](specs/README.md) — one document per component, with the build
order. The specs are written for people who change Lux, not for people
who run it. [`CONTRIBUTING.md`](CONTRIBUTING.md) is how to build, the bar
a change meets, and where a package belongs; [`SECURITY.md`](SECURITY.md)
is how to report a vulnerability.

## Project status

Pre-release, built in the open. Every capability above is implemented and
covered by tests, and `main` passes the full quality gate on every
commit. What remains before `v1` is the first tagged release, so there is
no published image or binary to pull yet, and the manifest schema may
still change; the [CHANGELOG](CHANGELOG.md) records every change to it.
Until then, run it from a checkout as [Try it](#try-it) shows.

## License

Apache-2.0. See [LICENSE](LICENSE).
