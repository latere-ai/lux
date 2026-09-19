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

## The full reference

The authoritative, machine-readable reference is the OpenAPI document. It
is committed at [`api/openapi.yaml`](../api/openapi.yaml) and served live,
generated from the code, at `GET /v1/openapi.json`:

```sh
curl -s "$LUX_URL/v1/openapi.json" | less
```

Open either in an OpenAPI viewer (Swagger UI, Redoc, or an editor plugin)
for the full schema of every kind, every route, and every error code.
`GET /.well-known/lux` names the served document's URL, so a client finds
it before it holds a token.

Reference decisions during apply (`provider.read`, `model.use`, `budget.draw`)
include `resource.binding: {kind, proposed}` for the enclosing mutation. This is
the same sanitized desired state sent to the create/update decision, including
the target owner; the caller remains the authenticated writer. Policies can bind
model permissions to that Key's selected Budget. Direct reads omit `binding`.
Key rotation sends its unchanged sanitized Key as `resource.proposed`, with
`resource.operation: "rotate"`, on `key.update`.
