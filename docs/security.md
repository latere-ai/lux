# Security

What you do to run `luxd` safely, and what it does for you without your
help. The full [threat model](../specs/016-security-and-threat-model.md),
one row per threat with the mechanism and the test that answers it, is
written for contributors. To report a vulnerability, see
[`SECURITY.md`](../SECURITY.md).

## What the gateway protects

- **A provider credential never leaves the gateway.** It is sealed in the
  store, opened in memory for one outbound request, and sent only toward
  that Provider's own `baseURL`. No read, list, event, or log line
  returns it.
- **A Key is not a provider key.** The store keeps a SHA-256 of each Key
  value, never the value. A Key works only on a door and an issuer token
  only on `/v1`; each is `unauthenticated` on the other surface.
- **Secrets stay out of telemetry.** No prompt, completion, credential,
  or Key value reaches a usage record, an event, a log line, a metric, or
  a span. A minted `lux_` value logged by accident is truncated to its
  prefix.
- **The two planes are separated.** A workload on a door reaches only the
  models its Key names; changing desired state needs an issuer token and
  an authorizer allow. An object a subject may not see is `not_found`,
  not `forbidden`.
- **The gateway is not an SSRF primitive.** A Provider `baseURL` naming a
  private address, a bare hostname, or the gateway itself is refused at
  resolve and again at dial, unless `LUX_UPSTREAM_ALLOW_PRIVATE` is set.

## What you must do

### Set and protect the KEK

`LUX_SECRETS_KEK` wraps every provider credential at rest. Generate 32
random bytes, base64-encoded, and keep them: a gateway restarted without
the key cannot open the credentials it stored.

```sh
head -c 32 /dev/urandom | base64
```

The value is never echoed by the process. Rotate by prepending a new key,
running `luxd rewrap`, then dropping the old one:
`LUX_SECRETS_KEK=new,old`, `luxd rewrap`, `LUX_SECRETS_KEK=new`. See
[`configuration.md`](configuration.md#secrets).

### Terminate TLS in front of the gateway

`luxd` serves plain HTTP; without TLS a Key and an issuer token travel in
the clear. Front it with an ingress or proxy that terminates TLS, and set
`LUX_PUBLIC_URL` to the `https://` URL callers reach (see
[`configuration.md`](configuration.md#listeners)).

### Set the trusted proxies

Behind a proxy, per-caller rate limits and the authorizer's client
address are read from `X-Forwarded-For`. Set `LUX_TRUSTED_PROXIES` to the
CIDR ranges of the proxies you run, so the client is the last forwarded
entry outside those ranges; unset, no forwarded header is trusted and the
peer is the client. A range you do not control lets a caller spoof its
address, so name only your own. See
[`configuration.md`](configuration.md#keys-and-limits).

### Write and lock down the authorizer

With `LUX_AUTHORIZER_URL` unset, the built-in owner policy applies and
only the subjects in `LUX_ADMIN_SUBJECTS` may declare Providers and
Models; an empty list means a fresh installation holds no credential to
steal. For any real permission model, point `LUX_AUTHORIZER_URL` at an
endpoint you write and give it `LUX_AUTHORIZER_TOKEN`, the bearer `luxd`
sends it. That endpoint decides every control-plane request, and a
decision it cannot give is a refusal, never an allow, so keep it reachable
by the gateway alone. [`plane.md`](plane.md) has a minimal authorizer to
start from; the knobs are in
[`configuration.md`](configuration.md#authorizer).

### Rotate and revoke Keys

A leaked Key value is contained by `POST /v1/keys/{id}/rotate`, which
replaces the value with no grace period; by `spec.disabled`, which refuses
at once; by `spec.ttl`; and by `DELETE`. Each takes effect on every
replica within `LUX_KEY_CACHE`, except on a replica cut off from the
store: it keeps serving the Keys it has cached for up to
`LUX_KEY_CACHE_GRACE` past that window, five minutes by default, and a
revocation reaches it only when the grace ends. Set
`LUX_KEY_CACHE_GRACE=0` where a revocation must hold through a store
outage; such a replica then refuses each Key `store_unavailable` once its
cached entry lapses. A Key's spend limit and its Budget bound what a leak
can cost before you notice.

### Hold an upstream to its data terms

A prompt the gateway forwards is kept by the upstream under the
upstream's terms. Where an upstream takes those terms per request in the
body, set them on the Provider with `spec.requestFields`: the gateway
merges the object into every request body it sends that Provider, after
any translation between dialects, and the Provider's value wins on each
member it names, so a caller can neither leave the option out nor turn
it off. A caller's other members, its own routing preferences beside
yours included, still reach the upstream.

For OpenRouter, this restricts every request to endpoints with zero data
retention:

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openrouter
spec:
  dialect: openai
  baseURL: https://openrouter.ai/api/v1
  requestFields:
    provider:
      zdr: true
```

A model that has no endpoint meeting the terms is refused by OpenRouter,
and the call fails rather than reaching an endpoint that keeps the
prompt.

OpenRouter's `data_collection: deny` is a separate, narrower option: it
leaves out the providers that may train on the data. Set it beside `zdr`
under `provider` when you want that exclusion as well.

To change the field on a running gateway, apply the Provider again with
`lux apply -f`, or `PUT` it to `/v1/providers/{name}`; a manifest that
carries no credential value keeps the stored one, and every replica
picks the change up as it does any other apply. A gateway that reads its
manifests from a directory takes the edited file when it starts again.
The field is returned by every read of the Provider, so it holds terms
and never a secret.

The merge reaches every request body that is a JSON object: each model
route, and each opaque route for a Key with `passthrough`. A body sent
with a `Content-Encoding` is refused toward such a Provider, because the
fields cannot be written into it. An option inside an uploaded file, such
as a batch input, is not a body the gateway reads, so keep `passthrough`
off on the Keys that reach a Provider whose terms must hold for every
call. The object cannot set `model`, `stream`, or `stream_options`,
which the gateway writes itself; the `ProviderSpec` schema in
[`api/openapi.yaml`](../api/openapi.yaml) has the full rule.

### Keep a workload's requests from upstreams that may keep them

`spec.requestFields` holds every caller of a Provider to the same terms.
When only some workloads need their requests kept from upstreams that
may store them, set `spec.zeroRetention: true` on the Keys those
workloads hold, and declare on each Provider whose upstream keeps
nothing of a request how it does so:

| The Provider's `spec.zeroRetention` | A request on a zero-retention Key |
|---|---|
| absent | is never sent to the Provider |
| `{}` | is sent as any request is: you state that the upstream keeps nothing of any request |
| `requestFields: {...}` | is sent with these fields merged into the body after `spec.requestFields`, winning on every member both name |

Requests on other Keys reach the same Providers as before and never
carry the zero-retention fields. For OpenRouter, the declaration carries
its routing option, so only the zero-retention Keys' requests are held
to endpoints with zero data retention:

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openrouter
spec:
  dialect: openai
  baseURL: https://openrouter.ai/api/v1
  zeroRetention:
    requestFields:
      provider:
        zdr: true
```

`data_collection: deny` can sit beside `zdr` under `provider`, as it can
in `spec.requestFields`. A Provider whose upstream keeps nothing of any
request, a model you serve yourself or one under a contract with no
retention, declares `zeroRetention: {}`. A direct API that keeps a
response for later retrieval unless the request says otherwise carries
that member, such as `requestFields: {store: false}`. The declaration is
your statement about the upstream's terms; the gateway holds requests to
it and cannot check the terms themselves.

The Key asks for it:

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: private-agent
spec:
  models: ["openrouter/*"]
  zeroRetention: true
```

A request on such a Key is sent only to the targets of its Model whose
Provider declares `spec.zeroRetention`, and falls over only among them.
A Model with no such target is refused `zero_retention_unavailable`,
403, before any upstream is called, and the same request keeps being
refused until a target's Provider declares it. When declaring targets
exist and none can be tried now, the answer is `provider_unavailable`,
as for any request with no target left. A caller cannot turn the fields
off: they win over the caller's members as `spec.requestFields` does,
and a body they cannot be written into, one that is not a JSON object or
carries a `Content-Encoding` other than `identity`, is refused
`invalid_request`.

An opaque route, open to a Key with `passthrough`, reaches the
upstream's own API, whose storage endpoints, files, batches, threads,
and stored conversations, keep what they are sent whatever a routing
option says. A zero-retention Key's opaque request is served only toward
a Provider that declares `{}`, and refused `zero_retention_unavailable`
toward any other.

The flag can be set or cleared on an existing Key with `lux apply -f`.
Every replica serves the change at its next read of the journal, one
second by default, and until then may serve the Key as it was. A
replica cut off from the store keeps serving the Keys it has cached for
up to `LUX_KEY_CACHE_GRACE` past `LUX_KEY_CACHE`, and a change reaches
it when the store answers again. If you build a platform whose users
turn the flag on, tell them this bound. Each request's record, its log
line, and its `lux.upstream` span say whether it was on a zero-retention
Key, so `lux requests` answers whether a past request was sent that way.

### Verify released artifacts

Every release is signed by the release workflow's own identity. Verify
the image and the archives before you run them, with `cosign verify`,
`cosign verify-blob`, and `gh attestation verify` against the build
provenance; [`SECURITY.md`](../SECURITY.md#verifying-a-release) has the
commands.

## Reporting a vulnerability

Do not open a public issue. [`SECURITY.md`](../SECURITY.md) has the
address and the response times.
