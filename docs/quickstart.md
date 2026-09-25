# Quick start

`docker compose up` and a handful of `curl`s: the published `luxd` image
and the published image of the test stubs, with no checkout and no build.
It is `make run` without the toolchain: the memory store, a stub issuer,
and a stub provider per dialect, so nothing here outlives
`docker compose down`. For a real cluster, read [`install.md`](install.md).

You need Docker or Podman with Compose, `curl`, and
[`compose.yaml`](../compose.yaml), which you can download on its own.

## Pick a release

Every release publishes both images under the tag it was cut at, and the
[releases page](https://github.com/latere-ai/lux/releases) lists them. No
`latest` tag is published, so `compose.yaml` runs the newest release unless
`LUX_VERSION` names another. The line below names the newest; change it to
run an older one:

```sh
export LUX_VERSION=v0.8.0
```

`LUX_OWNER` selects the registry namespace, `latere-ai` unless you run
images a fork published under its own.

To run a build of your own instead, build both images from a checkout
under a tag no release uses, so a local image never shadows a published
one:

```sh
export LUX_VERSION=local
tools/release/build.sh v0.0.0-local dist
docker build --build-arg TARGETARCH="$(go env GOARCH)" -f Dockerfile.release -t "ghcr.io/${LUX_OWNER:-latere-ai}/lux:$LUX_VERSION" .
docker build --build-arg TARGETARCH="$(go env GOARCH)" -f Dockerfile.stubs -t "ghcr.io/${LUX_OWNER:-latere-ai}/lux-stubs:$LUX_VERSION" .
```

`build.sh` writes the Linux binaries under `dist/`, the same bytes the
release archives carry, and each `docker build` copies one into the
distroless runtime stage.

## Run it

```sh
docker compose up -d
```

`luxd` fetches the stub issuer's keys at start and exits if the issuer is
not up yet, so on a start-order race it restarts until the stubs answer.
Wait for the public door:

```sh
export LUX_URL=http://localhost:8080
for _ in $(seq 1 30); do curl -fsS -o /dev/null "$LUX_URL/readyz" && break; sleep 1; done
```

## A token

The stub issuer mints a token for any subject through `POST /mint`. Its
`iss` is `http://lux-stubs:9105`, the string `luxd` verifies a token
against in `LUX_OIDC_ISSUERS` and the subject the owner policy admits.

```sh
export LUX_TOKEN=$(curl -fsS -X POST http://localhost:9105/mint \
  -H 'Content-Type: application/json' -d '{"sub":"dev"}' \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$LUX_TOKEN" ] || { echo "the stub issuer minted no token" >&2; exit 1; }
```

## A Provider, a Model, a Budget, and a Key

The Provider points at the openai stub by its compose name; the Model is
the routable name callers use; the Key's value prints once and is then
stored as a hash. `make run` applies one manifest per dialect from
[`deploy/examples`](../deploy/examples); this is the openai path alone.

```sh
curl -fsS -X PUT "$LUX_URL/v1/providers/openai" -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' --data-binary @- <<'EOF'
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openai
spec:
  dialect: openai
  baseURL: http://lux-stubs:9101/v1
  credential:
    value: stub-credential
  discovery:
    mode: none
EOF

curl -fsS -X PUT "$LUX_URL/v1/models/stub-openai" -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' --data-binary @- <<'EOF'
apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: stub-openai
spec:
  targets:
    - provider: openai
      model: stub-openai
  pricing:
    input: "2.50"
    output: "10"
EOF

curl -fsS -X PUT "$LUX_URL/v1/budgets/dev" -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' --data-binary @- <<'EOF'
apiVersion: lux.latere.ai/v1beta1
kind: Budget
metadata:
  name: dev
spec:
  amount: "10"
  currency: USD
  window: month
EOF

export LUX_KEY=$(curl -fsS -X PUT "$LUX_URL/v1/keys/dev" -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' --data-binary @- <<'EOF' | sed -n 's/.*"value":"\([^"]*\)".*/\1/p'
apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: dev
spec:
  models: ["*"]
  budget: dev
EOF
)
[ -n "$LUX_KEY" ] || { echo "the Key's value was not returned" >&2; exit 1; }
```

Read the Key back. The value printed above is not stored and does not
appear again:

```sh
curl -fsS "$LUX_URL/v1/keys/dev" -H "Authorization: Bearer $LUX_TOKEN"
```

## One request through a door

Point any OpenAI SDK at `$LUX_URL/openai/v1` with the Key as its API key,
or `curl` the door. The stub answers `stub:openai:<model>:<digest>` and
reports 100 input and 20 output tokens, so with the Model's pricing the
request costs a fixed amount.

```sh
curl -sS "$LUX_URL/openai/v1/chat/completions" -H "Authorization: Bearer $LUX_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"stub-openai","messages":[{"role":"user","content":"hello"}]}'
```

`GET /v1/requests` shows the record that request produced, with the key,
the model, the provider, the tokens, the cost, and never the content:

```sh
curl -fsS "$LUX_URL/v1/requests" -H "Authorization: Bearer $LUX_TOKEN"
```

## Stop

```sh
docker compose down
```

## What the wiring does

- The stub issuer's `-issuer-url` is `http://lux-stubs:9105`, so the `iss`
  it mints, its discovery `issuer`, and `LUX_OIDC_ISSUERS` are one string
  that resolves from the `luxd` container. It is `http://` off loopback,
  so it is also listed in `LUX_OIDC_INSECURE_ISSUERS`.
- Permission is the built-in owner policy: `LUX_ADMIN_SUBJECTS` names the
  subject `http://lux-stubs:9105|dev` a stub token renders to. `luxd`
  accepts an authorizer only over `https://` or on loopback, so a compose
  peer cannot serve one. On a cluster, `LUX_AUTHORIZER_URL` points at an
  endpoint you write, as the end of [`install.md`](install.md) says.
- A Provider's `baseURL` names a private compose address, so
  `LUX_UPSTREAM_ALLOW_PRIVATE=1` admits it, and `LUX_PUBLIC_URL`'s host
  differs from it, or the gateway would refuse the Provider as a loop back
  to itself.
