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
was spent and what ran, and `POST /v1/usage/redact` with
`{"owner": "<issuer>|<subject>"}` takes one owner out of every usage row,
keeping the totals; `GET /v1/self` reports the caller's identity.
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

The OpenAI, Anthropic and Lux doors translate into one another, so each
reaches a provider of any of those dialects. The Gemini door is not
translated: it reaches only a provider that speaks Gemini, and answers
`dialect_unsupported` for any other. Every door's list shows the same
models, including ones that door cannot reach.

`GET /{door}/v1/models` is that dialect's own model list, distinct from
`/v1/models`, which is the `Model` kind in manifest shape. Each entry
also carries the figures its Model declares, so a client can size its
context and its spend from the list: the context window, the output
limit, the input modalities, and the prices per 1,000,000 tokens as
decimal strings. A figure the Model does not declare is left out.

| Door | Window | Output limit | Input modalities | Prices |
|---|---|---|---|---|
| `/openai`, `/lux` | `context_window` | `max_output_tokens` | `input_modalities` | `pricing.input`, `.output`, `.cached_input`, `.cache_write` |
| `/anthropic` | `max_input_tokens` | `max_tokens` | `input_modalities` | as on `/openai` |
| `/gemini` | `inputTokenLimit` | `outputTokenLimit` | not listed | not listed |

Every `pricing` object also names its `currency` and `per`, which is
always `1000000`.

A request that names no door, such as `POST /v1/messages` from an SDK
whose base URL left the door out, is refused with `door_not_found`, 404,
and its message names the four doors.

## The lux door

The lux door is the gateway's own dialect: one request shape for every
provider, with the content of each message in typed blocks. It takes the
Key as `Authorization: Bearer`, or in `x-api-key`, and refuses with the
[error envelope](#errors) of `/v1`.

| Method | Path | Does |
|---|---|---|
| `POST` | `/lux/v1/generate` | one answer; with `"stream": true` it arrives as server-sent events |
| `POST` | `/lux/v1/count_tokens` | the input tokens of a `generate` body, as `{"input_tokens": n}` |
| `GET` | `/lux/v1/models` | the models the Key may call, in the list shape above |
| `GET` | `/lux/v1/models/{model}` | one entry of that list |

A request names a model and the conversation. Against the stand-ins of
[`make run`](local.md):

```sh
curl -sS $LUX_URL/lux/v1/generate \
  -H "Authorization: Bearer $LUX_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"stub-lux","max_tokens":64,"messages":[{"role":"user","blocks":[{"type":"text","text":"hello"}]}]}'
```

```json
{"blocks":[{"text":"stub:lux:stub-lux:2cf24dba","type":"text"}],"id":"lux-stub","model":"stub-lux","stop_reason":"end_turn","usage":{"cache_read_input_tokens":0,"cache_write_input_tokens":0,"input_tokens":100,"output_tokens":20}}
```

| Member | Type | Means |
|---|---|---|
| `model` | string | a Model the Key may call; required |
| `messages` | array | the conversation in order, each with a `role`, `user` or `assistant`, and its `blocks` |
| `system` | array of blocks | the instructions, apart from the conversation |
| `max_tokens` | integer | the most tokens the answer may take |
| `temperature`, `top_p`, `top_k` | number | sampling |
| `stop_sequences` | array of strings | text that ends the answer |
| `stream` | boolean | answer as server-sent events |
| `tools` | array | tools the model may call, each a `name`, a `description` and an `input_schema` (JSON Schema) |
| `tool_choice` | object | `mode`: `auto`, `any` (some tool), `none`, or `tool` with the tool's `name` |
| `reasoning` | object | extended thinking: an `effort` of `minimal`, `low`, `medium` or `high`, or `budget_tokens` |
| `schema` | object | a JSON Schema the answer must match, as `schema`, with an optional `name` and `strict` |
| `user_id` | string | a stable id of your end user, for the provider's abuse monitoring |
| `cache_key` | string | a prompt-cache name, for providers that key their cache by name |
| `logprobs`, `top_logprobs` | boolean, integer | each answer token's log probability, and that many alternatives |

`server_tools`, `web_search` and `reasoning_replay` complete the list.
When the gateway translates the request for another dialect, a member it
does not read is named in the answer's `Lux-Loss` header rather than
refused.

Every block has a `type`, and any block but `thinking` and
`redacted_thinking` may set `cache_hint: true` to end a prompt-cache
prefix:

| `type` | Carries |
|---|---|
| `text` | `text` |
| `image` | `image`: a `media_type` with base64 `data`, or a `url` |
| `tool_use` | `tool_use`: the `id`, `name` and `args` (a JSON object) of a call the model made |
| `tool_result` | `tool_result`: the `tool_use_id` it answers, its `blocks`, and `is_error` |
| `thinking` | `text`, and the `signature` the provider needs to take the block back |
| `redacted_thinking` | `redacted`, which only the provider can read |
| `opaque` | `opaque`: a block in a provider's own `dialect`, carried unread in `raw` |

The answer carries `id`, `model`, `blocks`, a `stop_reason` of
`end_turn`, `tool_use`, `max_tokens`, `stop_sequence` (with the
`stop_sequence` that matched) or `refusal`, and `usage`:
`input_tokens`, `output_tokens`, and, when the provider reports them,
`cache_read_input_tokens`, `cache_write_input_tokens` and
`reasoning_tokens`.

With `"stream": true` each frame is an `event:` line and a `data:` line
whose JSON repeats the `type`:

```sh
curl -sS -N $LUX_URL/lux/v1/generate \
  -H "Authorization: Bearer $LUX_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"stub-lux","stream":true,"messages":[{"role":"user","blocks":[{"type":"text","text":"hello"}]}]}'
```

```
event: message_start
data: {"id":"lux-stub","model":"stub-lux","type":"message_start","usage":{"cache_read_input_tokens":0,"cache_write_input_tokens":0,"input_tokens":100,"output_tokens":0}}

event: block_start
data: {"block":{"type":"text"},"index":0,"type":"block_start"}

event: text_delta
data: {"delta":"stub:l","index":0,"type":"text_delta"}

event: text_delta
data: {"delta":"ux:stu","index":0,"type":"text_delta"}

event: text_delta
data: {"delta":"b-lux:","index":0,"type":"text_delta"}

event: text_delta
data: {"delta":"2cf24d","index":0,"type":"text_delta"}

event: text_delta
data: {"delta":"ba","index":0,"type":"text_delta"}

event: block_stop
data: {"index":0,"type":"block_stop"}

event: message_delta
data: {"stop_reason":"end_turn","type":"message_delta","usage":{"cache_read_input_tokens":0,"cache_write_input_tokens":0,"input_tokens":100,"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}
```

| Event | Carries |
|---|---|
| `message_start` | `id`, `model`, and `usage` so far |
| `block_start` | the block's `index` and the `block`, without its content |
| `text_delta`, `thinking_delta` | the next piece of the block's text, in `delta` |
| `args_delta` | the next piece of a tool call's `args`, in `delta` |
| `signature_delta` | the thinking block's `signature`, in `delta` |
| `block_stop` | the `index` of the block that is complete |
| `message_delta` | the `stop_reason` and the final `usage` |
| `message_stop` | nothing; the answer is complete |

A stream that fails after its first frame ends with `event: error` and
the [error envelope](#errors) in `data`.

`count_tokens` takes the same body as `generate`. For a Model on an
Anthropic provider the provider counts; for any other the gateway
estimates, and says so with `Lux-Estimated: true`.

A Model joins `GET /lux/v1/models` once the health job has found its
provider reachable, which is within `LUX_HEALTH_INTERVAL`, 30 seconds by
default, of the Model being applied.

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

A Key opens every door unless its `spec.doors` lists the doors it opens,
such as `doors: [lux]` for a Key handed to a program that speaks the lux
dialect alone. On any other door, its model list included, the Key is
refused `door_not_allowed`, 403, in that door's error shape, and the
request history records the refusal. The list is not secret, so an
authorizer sees it in a Key's proposal.

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

`GET /v1/requests` pages the same way, with `?limit=` at most 1000. Its
cursor keeps the range of the first page, so a query that gives `from`
alone, or a next page that sends the cursor alone, reads that range on
every page. Read from a request log archive, a page ends after a bounded
amount of reading, so it may hold fewer records than `limit`, or none,
while `next_cursor` is present: keep following `next_cursor` until it is
absent.

`GET /v1/requests?request_label=k=v` reads the requests that carried the
pair in their `Lux-Labels` header, the record's `requestLabels`, so a
harness that labels each call with the run it belongs to reads a run's
calls without keeping their request ids. The parameter repeats, every
pair must match, and it joins the other filters; `label` is the other
map, the Key's labels. It narrows what the caller may already read and
never widens it. A pair outside the `Lux-Labels` syntax, or more names
than the eight a request keeps, is `invalid_field`. The archive is partitioned by a Key
label and not by a request label, so a query by request label alone
reads every partition: follow `next_cursor`, or name the Key or the
owner as well to read less.

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
