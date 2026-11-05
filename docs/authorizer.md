# Writing an authorizer

For whoever writes the endpoint `LUX_AUTHORIZER_URL` points at. Lux
stores four kinds of object, Provider, Model, Key and Budget, and serves
the doors. Who may do what to them is yours: a plan, a team, a quota, a
role. Before a `/v1` request reads or changes an object, or reads usage,
`luxd` sends one question to your endpoint and follows its answer. The
doors ask nothing: a request with a Key is held to the Key, not to your
endpoint. The endpoint is optional: without one, `luxd` decides by the
[built-in owner policy](#without-an-authorizer). This page is the
contract, and [Building a platform](plane.md#the-minimal-authorizer)
carries a complete program that keeps it.

## The question

For each decision, `luxd` sends one request to `LUX_AUTHORIZER_URL`,
presenting `LUX_AUTHORIZER_TOKEN` as the bearer. Both are set in the
[configuration](configuration.md#authorizer). `luxd` refuses to start
when the URL is set and the token is not, and when the URL is `http://`
on a host that is not loopback.

```
POST <LUX_AUTHORIZER_URL>
Authorization: Bearer <LUX_AUTHORIZER_TOKEN>
Content-Type: application/json
Accept: application/json
```

A question for `key.create`, when a caller applies a Key named
`run-42` that selects the Models `gpt-*`:

```json
{
  "subject": "https://login.example.com|alice",
  "issuer": "https://login.example.com",
  "sub": "alice",
  "claims": {"aud": "lux", "exp": 1793982589, "iss": "https://login.example.com", "plan": "team", "sub": "alice"},
  "action": "key.create",
  "resource": {
    "kind": "Key",
    "name": "run-42",
    "labels": {"team": "web"},
    "models": ["gpt-*"],
    "budget": "",
    "budgets": [],
    "proposed": {
      "owner": "https://login.example.com|alice",
      "metadata": {"name": "run-42", "labels": {"team": "web"}},
      "spec": {"models": ["gpt-*"], "allowUnpriced": false, "passthrough": false, "disabled": false},
      "credential": {"mode": "absent"}
    }
  },
  "request": {"id": "req_01J9...", "ip": "203.0.113.4", "user_agent": "curl/8.7"}
}
```

- `subject` is the issuer, with a trailing slash removed, and the `sub`
  joined by `|`. `issuer` and `sub` are its two halves. A request with
  no subject, which only the [reserved probe](#the-reserved-probe) is,
  sends them empty.
- `claims` is every claim of the caller's token, as it was signed.
  `luxd` reads none of them to decide, so a plan, a team or a role is
  read here. A personal access token carries the grants its holder
  chose as `authorization_details`, and `luxd` forwards that claim
  verbatim. An endpoint written on the `authz/server` scaffold (see
  [Writing one](#writing-one)) intersects its answer with them for you.
- `workload` is never sent. The member exists in the shared envelope for
  a control plane whose callers can be sandboxes, and `luxd` leaves it
  out.
- `action` is one of [the vocabulary](#the-vocabulary).
- `resource` is one flat object: its `kind`, its `id` when it has one,
  and the fields of that kind. A create carries no `id`, because the
  object does not exist yet, and a list action carries the `kind` and
  nothing else. Every list and map field is present and never `null`.
  - A Provider carries `name`, `dialect`, `baseURL`, `tunnel` and
    `labels` on a create, and adds `id` and `owner` once it exists. A
    Model carries `name`, `targets` (each a `provider` and a `model`)
    and `labels` on a create, and `id`, `name`, `owner`, `source` and
    `labels` once it exists. A Key carries `name`, `labels`, `models`,
    `budget` and `budgets` on a create, and `id`, `name`, `owner`,
    `prefix` and `labels` once it exists. A Budget carries `name`,
    `amount` (a string), `currency`, `window` and `labels` on a create,
    and `id`, `name`, `owner` and `labels` once it exists.
  - `model.use` carries no `id`. It carries the `selector` a Key names
    and `matched`, the Models the selector matches now, each as `id`,
    `name`, `owner` and `labels`.
  - `usage.read` is on the kind `Usage` and carries `keys` and `owners`,
    the Key ids and owners a query names. `usage.redact` carries the
    `owner` whose rows lose their identity.
  - `owner.assign` is on the kind `Ownership` and carries `target_kind`,
    `name`, `owner` and `proposed`. `key.fence` and `key.fence.read` are
    on the kind `KeyFence`, with the Key's `name` as the `id`;
    `key.fence` also carries `name`, `owner` and `labels`, and the read
    carries `name`.
  - `owner` is the subject that created the object. A create has none:
    the owner it will have is `proposed.owner`.
  - `proposed` is added on the create and the update of every kind and
    on a Key rotation. It holds the object the request would write: its
    `owner`, `metadata` and `spec`. Status, credential values, supplied
    Key hashes and Provider header values are left out. A Key's
    proposal adds `credential`, whose `mode` is `absent`, `inline`,
    `sha256`, `reference` or `conflict`, and for `sha256` a `commitment`
    that is the hex SHA-256 of the exact verifier string. A Provider's
    `spec` adds `headerNames` and `credentialSupplied`. A rotation sends
    the unchanged Key as `proposed` with `operation: "rotate"` on
    `key.update`. See [what an authorizer is
    asked](api.md#what-an-authorizer-is-asked).
  - `binding` is added on the reference decisions `provider.read`,
    `model.use` and `budget.draw` that an apply makes. It holds the
    `kind` and `proposed` of the enclosing mutation. A direct read omits
    it.
  - `authorize_items` is `true` on a list action when
    `LUX_AUTHORIZE_LIST_ITEMS=1`, and absent otherwise.
- `request` is the request's `id`, the caller's address and the user
  agent. The `id` is the `req_` id `luxd` gives the request and is also
  the `Lux-Request-Id` the caller sees; it is never empty.

An answer is cached on everything in the question except `request.id`
([the answer](#the-answer) says for how long).

## The vocabulary

Twenty-eight actions over seven kinds: `Provider`, `Model`, `Key`,
`Budget`, `Usage`, `Ownership` and `KeyFence`. In Go, import
`latere.ai/x/lux/authorizer` for the action names rather than copying
the strings from this page. An action acts on exactly one kind, the one
in the `resource.kind` of its question.

| Action | What it allows | Asked by |
|---|---|---|
| `provider.create` | declaring a Provider | `PUT /v1/providers/{name}` when the name is free |
| `provider.read` | reading a Provider | `GET /v1/providers/{id-or-name}`; also asked as a reference for each target of a Model an apply names. A deny on the reference is `not_found` |
| `provider.update` | changing a Provider | `PUT /v1/providers/{name}` when it exists |
| `provider.delete` | deleting a Provider | `DELETE /v1/providers/{id-or-name}` |
| `provider.tunnel` | attaching a local runtime to a tunneled Provider | `POST /v1/providers/{id-or-name}/tunnel` |
| `provider.list` | listing Providers | `GET /v1/providers` |
| `model.create` | declaring a Model | `PUT /v1/models/{name}` when the name is free |
| `model.read` | reading a Model | `GET /v1/models/{id-or-name}`, and each row of a Model list when `LUX_AUTHORIZE_LIST_ITEMS=1` |
| `model.update` | changing a Model | `PUT /v1/models/{name}` when it exists |
| `model.delete` | deleting a Model | `DELETE /v1/models/{id-or-name}` |
| `model.list` | listing Models | `GET /v1/models` |
| `model.use` | a Key selecting Models | an apply of a Key, once for each selector in its `models`. A deny is `not_found` |
| `key.create` | creating a Key | `PUT /v1/keys/{name}` when the name is free |
| `key.read` | reading a Key | `GET /v1/keys/{id-or-name}`, and each row of a Key list when `LUX_AUTHORIZE_LIST_ITEMS=1` |
| `key.update` | changing, disabling or rotating a Key | `PUT /v1/keys/{name}` when it exists, `POST /v1/keys/{id-or-name}/rotate` |
| `key.delete` | deleting a Key | `DELETE /v1/keys/{id-or-name}` |
| `key.list` | listing Keys | `GET /v1/keys` |
| `key.fence` | permanently closing a Key name to credential changes | `POST /v1/keys/{name}/fence` |
| `key.fence.read` | reading a Key name's fence | `GET /v1/keys/{name}/fence` |
| `budget.create` | declaring a Budget | `PUT /v1/budgets/{name}` when the name is free |
| `budget.read` | reading a Budget | `GET /v1/budgets/{id-or-name}`, and each row of a Budget list when `LUX_AUTHORIZE_LIST_ITEMS=1` |
| `budget.update` | changing a Budget | `PUT /v1/budgets/{name}` when it exists |
| `budget.delete` | deleting a Budget | `DELETE /v1/budgets/{id-or-name}` |
| `budget.list` | listing Budgets | `GET /v1/budgets` |
| `budget.draw` | a Key drawing on a Budget | an apply of a Key whose `budget` or `budgets` names it. A deny is `not_found` |
| `usage.read` | reading usage and request records | `GET /v1/usage`, `GET /v1/requests` |
| `usage.redact` | taking one owner out of every usage row | `POST /v1/usage/redact` |
| `owner.assign` | choosing the owner of a new object | a create whose `Lux-Owner` header names a subject other than the caller, asked after the create action |

A deny on the action a request asked is `403 forbidden`. A deny on a
reference, which is `provider.read`, `model.use` and `budget.draw` during
an apply, is `404 not_found` with a detail that names the reference and
not the reason, so a manifest cannot probe for objects another tenant
owns. The endpoint's `reason` reaches the caller as developer detail on
a `forbidden`, never in its message. See [the API
guide](api.md#what-an-authorizer-is-asked) for the proposals and the
[Key fences](api.md#permanent-key-fences) for the two fence actions.

## The answer

The answer is `200` either way:

```
200 {"allow": true, "ttl": 60, "limits": {"max_key_spend": "50", "max_keys": 100}}
200 {"allow": true, "filter": {"owners": ["https://login.example.com|alice"]}}
200 {"allow": false, "reason": "not_the_owner"}
```

| Field | Meaning |
|---|---|
| `allow` | required; the decision |
| `reason` | on a deny, a short reason, carried to the caller as developer detail |
| `ttl` | seconds `luxd` may reuse an allow for the same question; 60 when absent or not above zero, at most 600. A deny is reused for 5 seconds. A question about a resource with no id, such as a create, a `model.use`, a list or a usage query, is never reused |
| `limits` | the figures an allow carries, below |
| `filter` | on a list action and on `usage.read`, narrows the result to the named `owners` and `labels` |

The cache is per replica and holds at most 65,536 answers. An answer
that failed is never kept, so a recovered endpoint is asked again on the
next request. `luxd` passes `LUX_AUTHORIZER_TIMEOUT` to its client and
sets no other figure: the TTL figures above are the shared client's, and
no `LUX_*` variable changes them.

### Limits

Import `latere.ai/x/lux/authorizer` and write the figures through
`authorizer.WireLimits`, rather than through string literals. Leave a
member out for no limit. Six members are read:

- `requests_per_minute` replaces `LUX_REQUESTS_PER_MINUTE` as the
  control plane rate of this subject. It is read from the subject's most
  recent allow, for that allow's `ttl`.
- `max_key_requests_per_minute`, `max_key_tokens_per_minute`,
  `max_key_spend` and `max_key_ttl` are ceilings on a Key this subject
  applies, carried on the allow of `key.create` or `key.update`. A Key
  that asks for more is refused `ceiling_exceeded`, `422`. A ceiling on
  spend refuses a Key that names no spend limit, and a ceiling on ttl
  refuses a Key that never expires. `max_key_spend` is a money string
  such as `"50"` and `max_key_ttl` a duration such as `"720h"`.
- `max_keys` on the allow of `key.create` caps the Keys the new Key's
  owner holds. A create at the cap is refused `ceiling_exceeded`.

A ceiling changes no Key that exists. A figure below zero, a spend that
is not a money string, a ttl that is not a duration, or a `limits`
object that does not parse is not a decision: see [Failing
closed](#failing-closed). An exact disable of a Key, which changes
nothing but `disabled: true`, is not held to the ceilings.

### Lists

A list action (`provider.list`, `model.list`, `key.list`,
`budget.list`) answers an allow that may carry a `filter` of `owners`
and `labels`. `luxd` intersects the filter with the query's own
selectors and lists only the objects it admits. With
`LUX_AUTHORIZE_LIST_ITEMS=1` it then asks the kind's read for each
candidate and skips the ones that read refuses, without using up page
capacity; an authorizer failure on a read fails the whole list. The
filter narrows a list and never widens it. `usage.read` takes a
`filter` the same way and narrows the usage it answers. A `filter` on
any other action is not read.

## Failing closed

An answer `luxd` cannot use is a refusal, and the caller sees `503
authorizer_unavailable`. There is no fail-open, and nothing is changed
by a request the endpoint did not allow. These are all refusals:

- an error, or a connection that cannot be made;
- no answer within the timeout, 5 seconds by default, set by
  `LUX_AUTHORIZER_TIMEOUT`, and covering the retry as well;
- a status other than `200`, a `400` for an action outside the
  vocabulary included;
- a body that is not a JSON decision, a body without `allow`, or one
  longer than 64 KiB;
- an allow whose `limits` does not parse or holds a figure below zero.

`luxd` asks a second time only when the connection failed before any
response began, such as a refused or reset connection, and never after a
response line arrived or after a timeout. An action outside Lux's
vocabulary is refused inside `luxd` and never sent.

So answer a deny as a decision and not as an error: an error is read as
"no decision" and refuses the request either way. Every request that is
not answered from the cache waits on the endpoint, so run it near
`luxd` and answer from memory. An endpoint that shares a lock with the
caller of `/v1` can stall a request for the timeout, as
[Building a platform](plane.md#the-minimal-authorizer) explains.

## Without an authorizer

With `LUX_AUTHORIZER_URL` unset, `luxd` decides in process by the
built-in owner policy, in this order:

- The reserved probe and a request with no subject are denied.
- The subjects in `LUX_ADMIN_SUBJECTS`, written `<issuer>|<sub>` and
  separated by commas, may do everything.
- Declaring, changing or deleting a Provider or a Model is an
  administrator's alone, except a Provider with `tunnel: true`, which
  any subject may create and whose owner may update, delete and tunnel.
- Every subject may read and list Providers and Models, use a Model, and
  create a Key or a Budget.
- `key.list`, `budget.list` and `usage.read` are allowed with the filter
  narrowed to the caller's own objects.
- `owner.assign`, `key.fence`, `key.fence.read` and `usage.redact` are
  an administrator's alone.
- Every other action on an object is allowed to its owner, and denied to
  anyone else whether or not the object exists.

The policy sends no `limits`, so it sets no ceiling. It also intersects
its answer with the grants a personal access token carries.
`LUX_ADMIN_SUBJECTS` is read and unused once an endpoint is set. In the
file mode (`LUX_MANIFEST_DIR`) nothing is authorized, because the
directory is the desired state. The owner policy is the right choice for
a single team. Run an authorizer when who may do what is a decision your
product makes.

## The reserved probe

The resource id `00000000-0000-0000-0000-000000000001` is reserved. Deny
it for every subject and every action, the anonymous subject included.
An endpoint that allows it is one that does not read the request.

`luxd check` sends one question, `provider.read` on a `Provider` whose id
is the probe and which carries no subject, to the endpoint with its own
configuration. The `authorizer` line passes when the endpoint answers a
deny, and fails when it answers an allow or gives no decision, and the
command exits `1` when any line fails. With no endpoint set it asks the
owner policy, which denies the probe too. The [first
check](install.md#the-first-check) lists the other lines. Run it before
pointing `luxd` at a new endpoint.

The [conformance command](plane.md#the-conformance-command) tests a
server's doors and control plane. It does not test your endpoint. The
suite that does is `latere.ai/x/pkg/authz/conformance`, which runs
against the vocabulary with
`conformance.WithVocabulary(authorizer.Vocabulary())`.

## Writing one

[Building a platform](plane.md#the-minimal-authorizer) carries a
complete endpoint in Go, written on `latere.ai/x/pkg/authz/server`,
which checks the bearer, decodes the question, refuses an action outside
the vocabulary, denies the probe id and applies a token's grants to your
decision. It is also `examples/authorizer`, and a test holds the two
equal. An endpoint in another language is any HTTP server that does what
that scaffold does: check the bearer, read the body of [the
question](#the-question), answer `200` with [the answer](#the-answer),
and deny the probe.
