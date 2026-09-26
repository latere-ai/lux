# API

`luxd` serves two HTTP surfaces on the public listener: a control plane
under `/v1`, and the dialect doors for inference. Every URL is built from
`LUX_PUBLIC_URL`, and `GET /.well-known/lux` names them all without a
credential. This page orients you; the authoritative reference is the
OpenAPI document, below.

## The control plane

`/v1` is one grammar over four kinds, `Provider`, `Model`, `Key`, and
`Budget`, each with the same four routes.

| Method | Path | Does |
|---|---|---|
| `PUT` | `/v1/{kind}s/{name}` | apply by name: `201` on create, `200` on update; the body is a manifest |
| `GET` | `/v1/{kind}s/{id-or-name}` | read one object with its `status` |
| `GET` | `/v1/{kind}s` | list, paged |
| `DELETE` | `/v1/{kind}s/{id-or-name}` | delete; `204` |

A Key also has `POST /v1/keys/{id-or-name}/rotate`, which mints a new
value and keeps the id. `GET /v1/usage` and `GET /v1/requests` read what
was spent and what ran; `GET /v1/self` reports the caller's identity.
There is no `PATCH` and no creating `POST`: the loop is `GET`, edit,
`PUT`, and a `PUT` repeated with one body is `201` then `200` on the same
object.

An optional `Lux-Owner: issuer|subject` header assigns the immutable owner on
create. Assigning another principal requires both the ordinary create permission
and `owner.assign`. Updates may omit the header or repeat the stored owner.
The authenticated writer remains the audit actor.

## Permanent Key fences

`POST /v1/keys/{name}/fence` permanently closes a name to creation, rotation and
policy expansion. Send JSON with the expected `owner` (`issuer|subject`) and
exact `labels` (omitted means empty). It requires `key.fence`; ordinary Key
permissions do not grant it. `GET` on the same path requires `key.fence.read` and
returns the original `{name, owner, labels, createdAt}` record. Both use a Key
name, never an id, and return 200. Identical POSTs are safe to retry.

A different assertion or mismatched occupant returns `fence_conflict` (409).
Credential changes after installation return `key_fenced` (409). The existing
Key can still be read, deleted or updated to `disabled: true` with all other
metadata, policy and credential identity preserved. Deleting it does not reopen
the name. First installation writes one `key.fenced` audit event atomically;
retries do not duplicate it, including after event retention has elapsed.

Fencing alone does not disable an enabled Key. A controller must close its own
admission, fence every affected name, read and conditionally disable each current
Key, verify the result, and wait for the maximum serving-cache lifetime before
reporting revocation complete. Already admitted requests are a separate lifecycle.
Upgrade every writer before using fences; older writers do not enforce them and
cannot participate in a rollout or rollback once fences are in use.

## The doors

Inference does not go through `/v1`. Each provider dialect has its own
door, so an existing SDK points at it unchanged.

| Dialect | Base |
|---|---|
| OpenAI | `/openai/v1` |
| Anthropic | `/anthropic/v1` |
| Gemini | `/gemini/v1beta` |
| Lux | `/lux/v1` |

`GET /{door}/v1/models` is that dialect's own model list, distinct from
`/v1/models`, which is the `Model` kind in manifest shape.

## Under a base path

An installation that shares an origin with other services answers under
a prefix, `LUX_BASE_PATH`, which is the path of `LUX_PUBLIC_URL`.
`LUX_BASE_PATH_MODE` decides how the routes sit under it. With the base
`/v1/models` at `https://api.example.com`:

| Route at the root | `prefix`, the default | `replace` |
|---|---|---|
| a control plane route, `/v1/keys` | `/v1/models/v1/keys` | `/v1/models/keys` |
| the Model kind, `/v1/models/{name}` | `/v1/models/v1/models/{name}` | `/v1/models/models/{name}` |
| the OpenAPI document, `/v1/openapi.json` | `/v1/models/v1/openapi.json` | `/v1/models/openapi.json` |
| a door, `/openai/v1/chat/completions` | `/v1/models/openai/v1/chat/completions` | the same |
| `/.well-known/lux` and `/version` | `/v1/models/.well-known/lux`, `/v1/models/version` | the same |
| `/livez` and `/readyz` | `/v1/models/livez`, `/v1/models/readyz` | the internal listener only |

Under `replace` the base takes the place of the control plane's `/v1`,
so every address carries one version segment. The Model kind keeps its
`models` segment: a Model's name may contain slashes, and at the base
itself a name such as `openai/gpt-5` would collide with a door. The
doors do not move, so an SDK's base URL is the same under both values.

`GET /.well-known/lux` tells the two apart. Its `api` member is the
public URL plus `/v1` at the root and under `prefix`, and the public URL
itself under `replace`; each door is the public URL plus the dialect
under both. The `lux` command and the tunnel agent read it, so `LUX_URL`
is the public URL whichever value the installation runs.

## Authentication

Two credentials, one per surface, and neither works on the other.

- A door takes a **Key** as its bearer, or in the dialect's own API-key
  header (`x-api-key`, `x-goog-api-key`). A minted Key value begins with `lux_`; a value the platform supplied, as itself or as its hash, is whatever the platform chose, and its prefix on the Key is `sup_`.
- `/v1` takes an **issuer token**: a bearer a listed issuer signed,
  carrying the audience `LUX_OIDC_AUDIENCE` (`lux` by default). Set
  `LUX_OIDC_ISSUERS` (see [`configuration.md`](configuration.md#identity)).

A Key on `/v1` and a token on a door are each `unauthenticated`.
`/v1/openapi.json` and `/.well-known/lux` need no credential.

## Errors

Every refusal is one JSON envelope, on both planes:

```json
{"error": {"code": "invalid_field",
           "message": "A field has a value it cannot take.",
           "details": {"request_id": "req_01J9...",
                       "paths": ["spec.baseURL"],
                       "detail": "a loopback host needs LUX_UPSTREAM_ALLOW_PRIVATE"}}}
```

One `code` (also in the `Lux-Error` header), one fixed `message`, and
`details` carrying `request_id` always, `paths` for a code that names
fields, and a developer `detail` where there is one. The codes and their
HTTP statuses are the `x-lux-errors` block of the OpenAPI document.

## Pagination

A list answers `{"items": [...], "next_cursor": "<cursor>"}`; pass
`next_cursor` back as `?cursor=` for the next page, absent on the last.
`?limit=` defaults to 50, at most 200.

With `LUX_AUTHORIZE_LIST_ITEMS=1`, each candidate must also pass its
`provider.read`, `model.read`, `key.read`, or `budget.read` decision. Denied
objects are skipped without consuming page capacity. The list decision carries
`resource.authorize_items: true` only when these checks are enabled. An authorization failure
refuses the whole list. The list filter and query selectors still apply.

## Optimistic concurrency

Every object response carries `ETag: "<version>"`; send it back as
`If-Match: "<version>"` on a `PUT`, `DELETE`, or rotate to act only at
that version, and a stale one is `conflict`, 409. Without a precondition a
`PUT` is a read-modify-write that retries a concurrent change for you;
`If-None-Match: *` creates only when the name is free.

## What an authorizer is asked

With `LUX_AUTHORIZER_URL` set, every `/v1` request is one or more
decisions your endpoint answers; [`plane.md`](plane.md) has a minimal
endpoint and the rules it keeps. A mutation carries what it would write,
so a policy can decide on the result rather than on the route.

Reference decisions during apply (`provider.read`, `model.use`, `budget.draw`)
include `resource.binding: {kind, proposed}` for the enclosing mutation. This is
the same sanitized desired state sent to the create/update decision, including
the target owner; the caller remains the authenticated writer. Policies can bind
model permissions to that Key's selected Budget. Direct reads omit `binding`.
Key rotation sends its unchanged sanitized Key as `resource.proposed`, with
`resource.operation: "rotate"`, on `key.update`.

Key proposals additionally carry `credential.mode`: `absent`, `inline`, `sha256`,
`reference`, or `conflict` for multiple credential inputs. For `sha256`,
`credential.commitment` is lowercase hex SHA-256 of the exact UTF-8 verifier
string. It exposes neither the original verifier nor the credential. An external
registry can compare this fingerprint to admit only its registered credential.
`absent` means create may mint a new value, while update retains its value;
rotation is distinguished by `resource.operation`. Reference bindings and owner
assignment carry the identical descriptor.

An exact Key disable keeps every metadata and spec field unchanged except
`disabled: true`. It still requires `key.update` permission and honors `If-Match`,
but does not require model/budget reference access or satisfy new issuance
ceilings. This lets a controller disable expired Keys or Keys whose references
were removed. Read the current Key, keep its metadata and spec, set `disabled`,
and PUT with that read's ETag. Stored budget identity and expiry remain unchanged.

## The full reference

The authoritative, machine-readable reference is the OpenAPI document. It
is committed at [`api/openapi.yaml`](../api/openapi.yaml) and served live,
generated from the code, at `GET /v1/openapi.json`:

```sh
curl -s "$LUX_URL/v1/openapi.json" | less
```

Under a base path the document is at the address the `openapi` member of
`GET /.well-known/lux` names, with every path written as a caller reaches
it and `LUX_PUBLIC_URL` as its server.

Open either in an OpenAPI viewer (Swagger UI, Redoc, or an editor plugin)
for the full schema of every kind, every route, and every error code.
`GET /.well-known/lux` names the served document's URL, so a client finds
it before it holds a token.
