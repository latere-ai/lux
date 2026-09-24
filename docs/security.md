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
replica within `LUX_KEY_CACHE`. A Key's spend limit and its Budget bound
what a leak can cost before you notice.

### Verify released artifacts

Every release is signed by the release workflow's own identity. Verify
the image and the archives before you run them, with `cosign verify`,
`cosign verify-blob`, and `gh attestation verify` against the build
provenance; [`SECURITY.md`](../SECURITY.md#verifying-a-release) has the
commands.

## Reporting a vulnerability

Do not open a public issue. [`SECURITY.md`](../SECURITY.md) has the
address and the response times.
