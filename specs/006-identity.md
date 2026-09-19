---
title: "Identity: OIDC issuers, subjects, the authorizer webhook, the owner policy"
status: complete
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/003-manifest-contract.md
affects: [internal/auth/, internal/api/, internal/config/, test/stubs/, docs/]
effort: medium
created: 2026-09-13
updated: 2026-09-17
author: changkun
---

# Identity

## Overview

Two questions, two planes. On the control plane, who is applying this
manifest, and may they. On the data plane, which Key is this, and what
was it resolved to. The first is answered by any OpenID Connect issuer
the operator lists and by an authorizer endpoint the operator writes,
with a built-in owner policy when there is none. The second is answered
by the store alone: the Key's hash, its resolved manifest, and its
windows, with no webhook and no issuer on the path
([[001-architecture]], invariant 3). This spec owns the first question
whole and the boundary between the two; [[007-keys-and-limits]] owns
the second.

`luxd` verifies identity and issues none for people. It never holds a
session, never stores a password, never reads a group claim to decide
anything, and never calls an issuer for anything but discovery and
keys. The values it mints are Keys, which identify workloads and are
not tokens ([[001-architecture]], invariant 4). A dashboard that needs
sessions, an organization that needs roles, a plan that needs quotas,
each is a platform's, expressed through the authorizer.

## Current state

Built on 2026-09-14 in `internal/auth` and `internal/config`, on pkg
v0.66.0: the verifier over the listed issuers, the authorizer over the
shared client with Lux's vocabulary, the `Lookup` for `Resolve`, the
owner policy, and the start-up check. The wiring into `serve` lands
with [[010-state]]'s store, which supplies the object lookups the
package takes as interfaces; the Outcome below names the one call.

Amended on 2026-09-13: the verifier, the authorizer envelope, its cache
and retry rules, the subject string and the probe id are the shared
contract of `latere.ai/x/pkg/authz`, so one authorizer can serve several
open cores.

Amended on 2026-09-17: on pkg v0.73.0 the key a token is verified
against is the one its `kid` names and no second key is ever tried, so
the two refusals are apart. A `kid` the issuer's set does not hold is
reason `unknown_key`, decided before any signature is read, and a `kid`
miss against a reachable issuer forces one key set refresh first; a
signature made by a foreign key under a `kid` the set does hold is
reason `signature`, as an unsupported algorithm is. Both are
`unauthenticated` to the caller, with the finding in the developer
detail as every other refusal is.

## Design

### Subjects

A subject is the pair of the issuer and the `sub` claim, rendered as
one string everywhere it is stored or sent: `<iss>|<sub>`, the issuer
with any trailing slash removed, which is `authz.Subject` in
`latere.ai/x/pkg/authz`, so `https://login.example.com/` and
`https://login.example.com` name one issuer. `owner` fields, event
`subject` fields, `LUX_ADMIN_SUBJECTS` entries, and the authorizer
request all carry the rendered string; the authorizer request also
carries `issuer` and `sub` apart. The empty string is the anonymous
subject, which the verifier never produces (a token without `sub` is
refused) and the owner policy always denies. A Key is not a subject:
a data plane request has no subject, it has a Key, and the record and
the events carry the Key's `owner`, the subject that applied it, as the
attribution ([[009-usage-and-metering]]).

### Caller identity

`LUX_OIDC_ISSUERS` lists issuer URLs. At start `luxd` fetches each
`/.well-known/openid-configuration` and its `jwks_uri` and refuses to
start when any is unreachable or lists no `RS256` or `ES256` key;
afterwards it verifies through `latere.ai/x/pkg/authkit/jwt`, one
`jwt.Validator` per issuer, which caches a key set for its TTL,
refreshes it on an unknown `kid` at most once per fifteen seconds, and
serves the stale set while a refresh fails, so an issuer that goes away
later degrades to refusing new keys rather than every request. A
request's bearer is accepted when it is a JWS signed by a listed
issuer's key, `iss` matches, `aud` contains `LUX_OIDC_AUDIENCE`
(default `lux`), `exp` is present and in the future, and `nbf` if
present is past. `LUX_OIDC_AUDIENCE` is one value: the audience is the
gateway's own name, and no second audience is needed because a
platform's own developer credential never reaches `/v1` as a token; it
reaches the doors as a Key ([[020-building-a-plane]]). Every claim of
the verified token, read from the payload with `jwt.DecodePayload` after
`Validate` accepted it, is handed to the authorizer verbatim in
`claims`, and none is interpreted by the gateway: an issuer's
organisation, role, or group claims mean something to the authorizer
that reads them and nothing to `luxd`. An `http://` issuer is refused
unless it is on a loopback address or in `LUX_OIDC_INSECURE_ISSUERS`.

The start-up check reads each issuer's discovery document, which must
name the listed issuer as its `issuer` and a `jwks_uri`, and counts the
keys the shared verifier will use: an RSA key with its coordinates and
`alg` absent or `RS256`, or a P-256 key with its coordinates and `alg`
absent or `ES256`; a set with none is the refusal above. Per request,
the bearer's payload is decoded unverified for its `iss`, which selects
the issuer's validator, and a token whose `iss` is not listed or that
carries no `exp` is refused before any signature is checked; the
validator then resolves the one key the token's `kid` names, refusing a
`kid` its set does not hold with reason `unknown_key` before any
signature is read, and verifies the signature, `exp`, `nbf`, `iss`, and
`aud` against that key alone.
Every refusal is one code, `unauthenticated`, with the finding, no
bearer, not a JWS, an unlisted issuer, or the validator's error, in the
developer detail alone. `auth.Verifier` is the type; `Authenticate`
returns an `auth.Caller` with the rendered subject, the issuer without
its trailing slash, the `sub`, and the claims.

A control plane request without a bearer is `unauthenticated`, 401. A
control plane request whose bearer is a Key value is `unauthenticated`
too: a Key opens the doors and nothing else, so a leaked Key cannot
read or change desired state. There is no API key for the control
plane and no anonymous access; a caller that wants a long-lived
control plane credential gets one from its issuer. A platform calls
`/v1` with one of two tokens from its issuer, both with `aud` equal to
`LUX_OIDC_AUDIENCE`, and `luxd` tells them apart by nothing but their
claims: an actor token, minted for a signed-in person and carrying that
person's `sub`, when a person acts, for a console or a CLI; a service
token from the `client_credentials` grant, whose `sub` is the platform's
own service account, for unattended work such as provisioning a Key for
a run (`oidc.MintActorToken` and `oidc.ServiceTokenSource` in
`latere.ai/x/pkg/authkit/oidc` mint them). The consequence is the
`owner`: every object's `owner` is the rendered subject of the token
that applied it, so a Key applied with an actor token is owned by the
person and its usage is attributed to the person, and one applied with a
service token is owned by the service account, and a platform that wants
the person on the record puts them in a label under its own prefix
([[009-usage-and-metering]]). The two are one credential kind on this
hop, a bearer from a listed issuer, verified one way
([[020-building-a-plane]]).

With `LUX_OIDC_ISSUERS` unset, `luxd` refuses to start unless
`LUX_MANIFEST_DIR` is set: in file mode the control plane is read-only
and serves the four kinds to any caller on the internal listener and
to none on the public one ([[010-state]]), so there is nothing to
authorize.

### The authorizer

```
POST {LUX_AUTHORIZER_URL}
Authorization: Bearer {LUX_AUTHORIZER_TOKEN}
Content-Type: application/json

{
  "subject":  "https://login.example.com|alice",
  "issuer":   "https://login.example.com",
  "sub":      "alice",
  "claims":   {"email": "alice@example.com", "name": "Alice", "groups": ["research"], "org_id": "…", "roles": ["owner"]},
  "action":   "key.create",
  "resource": {"kind": "Key", "name": "run-42", "owner": "https://login.example.com|alice",
               "labels": {"run": "r_42"}, "models": ["gpt-5", "anthropic/*"], "budget": "team-research"},
  "request":  {"id": "req_01J9...", "ip": "203.0.113.4", "user_agent": "lux/0.1"}
}
```

`claims` is every claim of the token; the example shows three an
issuer commonly stamps beside two a platform's issuer adds. The
envelope is `authz.Request`: `resource` is one flat object with `kind`
and, when the object exists, `id` beside the fields, so an authorizer
reads `resource.owner` and never `resource.fields.owner`; the
envelope's optional `workload` field is for a core whose workloads ask,
and `luxd` never sends it, because a data plane request asks nothing.
`request.ip` is the client address as [[011-api]] determines it: the
peer's, or the forwarded one behind a proxy listed in
`LUX_TRUSTED_PROXIES`.
`resource` per action:

| Action | `resource` |
|---|---|
| `provider.create` | `{"kind": "Provider", "name", "dialect", "baseURL", "tunnel", "labels"}` from the manifest; no `id` yet |
| `provider.read`, `.update`, `.delete`, `.tunnel` | `{"kind": "Provider", "id", "name", "owner", "dialect", "baseURL", "tunnel", "labels"}`; `tunnel` is asked when an agent opens a session for a tunnelled Provider ([[013-tunnelled-runtimes]]) |
| `provider.list` | `{"kind": "Provider"}`; the response may carry `filter` |
| `model.create` | `{"kind": "Model", "name", "targets": [{"provider", "model"}], "labels"}` |
| `model.read`, `.update`, `.delete` | `{"kind": "Model", "id", "name", "owner", "source", "labels"}` |
| `model.list` | `{"kind": "Model"}`; `filter` applies |
| `model.use` | `{"kind": "Model", "selector": "anthropic/*", "matched": [{"id", "name", "owner", "labels"}]}`; asked once per selector at Key resolve through `Lookup.Models` ([[003-manifest-contract]]); the decision binds the selector, and the data plane matches it at request time against the catalog then |
| `key.create` | `{"kind": "Key", "name", "labels", "models", "budget"}` |
| `key.read`, `.update`, `.delete` | `{"kind": "Key", "id", "name", "owner", "prefix", "labels"}` |
| `key.list` | `{"kind": "Key"}`; `filter` applies |
| `budget.create` | `{"kind": "Budget", "name", "amount", "currency", "window", "labels"}` |
| `budget.read`, `.update`, `.delete` | `{"kind": "Budget", "id", "name", "owner", "labels"}` |
| `budget.list` | `{"kind": "Budget"}`; `filter` applies |
| `budget.draw` | the Budget as above; asked at Key resolve through `Lookup.Budget` |
| `usage.read` | `{"kind": "Usage", "keys": [ids], "owners": [subjects]}` from the query, `keys` being ids because the API resolves names first ([[011-api]]); the response's `filter` is intersected with the query and never widens it ([[009-usage-and-metering]]) |
| `owner.assign` | `{"kind": "Ownership", "target_kind", "name", "owner", "proposed"}`; additional permission to assign the owner of a new object ([025-mutation-authorization](.archive/025-mutation-authorization.md)) |

With `LUX_AUTHORIZE_LIST_ITEMS=1`, list resources additionally carry
`authorize_items: true`. An authorizer relying on per-object checks must require
this marker; it is absent when the checks are disabled.

Create and update decisions additionally carry `proposed`, the requested owner,
metadata and sanitized spec. Existing update fields retain the stored state.
Secrets and status are omitted as specified in [025-mutation-authorization](.archive/025-mutation-authorization.md).

Every list and map field of a resource is present and never `null`, so
an authorizer in any language reads `resource.labels` as an object and
`resource.models` as a list whether or not the manifest set them; a
create's `amount` is `""` when the manifest names none, because the
authorizer is asked before `Resolve` refuses the manifest. The builders
are the `authorizer` package's ([[022-authorizer-vocabulary-package]]),
one per row, and `ResourceFor(action, object)` picks the row for an
action on a manifest object.

Response, 200:

```json
{
  "allow": true,
  "reason": "",
  "ttl": 60,
  "limits": {"requests_per_minute": 1200, "max_key_requests_per_minute": 600,
             "max_key_tokens_per_minute": 1000000, "max_key_spend": "50", "max_key_ttl": "720h",
             "max_keys": 100},
  "filter": {"owners": ["https://login.example.com|alice"], "labels": {"team": "research"}}
}
```

`ttl` is optional, the seconds this allow may be cached, default `60`
and capped at `600`, both the contract's constants (`authz.DefaultTTL`,
`authz.MaxTTL`) and not an operator setting, so one authorizer's answer
is held for the same time by every core that asks it. `limits` is optional and every
field in it is optional: an absent field means the configured value or
no limit. `requests_per_minute` overrides
`LUX_REQUESTS_PER_MINUTE` for this subject on the control plane
([[011-api]]); the four `max_key_*` fields reach `Resolve` as `Limits`
([[003-manifest-contract]]) and cap what a Key this subject applies may
ask for; `max_keys` caps the subject's live Keys, checked by the API at
`key.create` and refused with `ceiling_exceeded`. A Key that names no
limit under a ceiling is refused the same way, because no limit exceeds
every cap; a platform under ceilings sets the limits it wants
explicitly, and the refusal's detail names the ceiling so a client can
retry with it ([[003-manifest-contract]]). `filter`, on a
`list` action or `usage.read`, narrows the result to the owners and
labels named.

Rules:

- A decision is any of the actions' outcomes. Everything else is
  `authorizer_unavailable`, 503, and never an allow: connection
  refused, a TLS failure, a non-200 status, a body that does not parse,
  a body without `allow`, a body over 64 KiB, a `limits` object whose
  fields do not read as the figures above, a figure below zero, a spend
  that is not a money string, or a ttl that is not a duration, and a
  timeout of `LUX_AUTHORIZER_TIMEOUT` (default `5s`), which bounds one
  decision with its retry included. The call is retried once when the connection
  failed before a response line arrived, a refused or reset connection
  or a dial timeout, and never on a non-200, a timeout after the request
  was sent, or a body that does not parse (`authz.Retryable`). An
  `http://` authorizer URL is refused at start unless it is on a
  loopback address. `LUX_AUTHORIZER_URL` without `LUX_AUTHORIZER_TOKEN`
  is a start-up failure. Availability is not a readiness check: a
  flapping endpoint fails control plane requests, not replicas, and
  never a data plane request.
- `LUX_AUTHORIZER_TOKEN` is the bearer of this one endpoint and of no
  other: it is not `LUX_EVENTS_SECRET`, not `LUX_TUNNEL_FORWARD_SECRET`,
  and not a token any issuer minted, because every cross-service
  credential is per endpoint and rotates. `luxd` reads it at start, so
  rotating it is setting the new value on both sides and restarting; an
  authorizer that accepts the old and the new bearer during the swap
  loses no decision. The endpoint should be reachable from inside the
  installation only, which is the operator's network policy
  ([[017-release-and-installation]]) and nothing `luxd` can check; `luxd
  check` reports the URL's scheme and host so an operator sees what it
  dials.
- The authorizer is asked inside a control plane request, so it must
  answer from the bearer above and its own state alone: an endpoint
  that requires a session refuses every call, and one that calls this
  gateway's `/v1` or the issuer while deciding asks a question whose
  answer waits on its own, which the timeout ends as
  `authorizer_unavailable`, never as a hang and never as an allow. A
  platform whose authorizer and `/v1` caller are one process keeps the
  two paths free of each other's locks for the same reason.
- A `deny` on a request's own action is `forbidden`, 403, with the
  authorizer's `reason` as the developer detail and never in the user
  sentence. A `deny` on `model.use`, `budget.draw`, or `provider.read`
  for a target, asked through `Lookup` at resolve, is `not_found`, so a
  refused object and a missing one are the same answer
  ([[003-manifest-contract]], [[016-security-and-threat-model]]): the
  same code and the same developer detail, which names the reference
  and not the reason, because a detail that differed would disclose
  which of the two it was. `model.use` is asked for every selector,
  one that matches nothing included, since the decision binds the
  selector and a Model declared later will match it.
- The API constructs `Lookup` per request from the caller's subject and
  the cache below, `auth.Authorizer.Lookup(caller, request, catalog)`
  over a `Catalog` of the store's Providers, Budgets, and the Models a
  selector matches; an importer constructs its own. A `Catalog` failure
  passes through `Resolve` as the store's own error, never as a
  refusal ([[003-manifest-contract]]), and the API answers
  `store_unavailable` for it ([[011-api]]).
- An allow is cached per replica for the answer's `ttl`, `60s` when
  the answer names none, capped at `600s`; a deny for `5s`;
  unavailability never; keyed by the complete serialized decision input
  except the correlation request id, including claims and proposed mutations,
  with the `limits` and `filter` that came with them
  (`authz.Client`). An answer about a resource with no id is never
  cached, because nothing names a key to remember it by: `create`,
  `list`, `usage.read`, and `model.use`, whose resource is a selector
  and not an object, are asked every time, so a Key resolve naming `n`
  selectors sends `n` `model.use` calls per apply, a control plane cost
  and never a data plane one. The cache holds at most 65 536 entries
  and evicts the least recent. A revocation at the authorizer therefore
  takes effect on the control plane within the allow's `ttl`, which the
  authorizer chooses. It takes effect on the data plane only through
  the Keys: a platform that revokes a subject deletes or disables its
  Keys, and the gateway stops serving them within `LUX_KEY_CACHE`
  ([[007-keys-and-limits]]). The authorizer is never asked about a data
  plane request, by design and by test ([[001-architecture]],
  `TestHotPathDialsNoWebhook`).
- The resource id `00000000-0000-0000-0000-000000000001`,
  `authz.ProbeID`, is reserved as a probe: every authorizer denies it
  for every subject and every action, the anonymous subject included,
  and `luxd check` ([[017-release-and-installation]]) sends it through
  `authz.Check` and reads an allow as an endpoint that does not read the
  request. It is one id for the three open cores, so one authorizer
  serves all three with one rule, and it is deliberately not a Lux id:
  it has no kind prefix and can never be minted, so no object ever has
  it and an item route given it answers `not_found`. The owner policy
  denies it too, as the first row of `authz.Policy.Decide`.
- The envelope (`authz.Request`, `authz.Decision`), the client with the
  cache and the retry (`authz.Client`), the owner policy's frame
  (`authz.Policy`), the subject rendering, the probe, and the stub
  authorizer (`authz/stub`) are `latere.ai/x/pkg/authz`, shared with
  the sibling open cores; `luxd` adds its action vocabulary and its
  `resource` shapes and nothing else. The conformance test an
  authorizer passes is `authz/conformance`, below.

### The owner policy

With `LUX_AUTHORIZER_URL` unset, `LUX_ADMIN_SUBJECTS` is read and the
log says `owner policy` at start. The policy is `authz.Policy`'s frame
with Lux's rows in front of it. The frame, which is the contract's and
which every core applies the same way, decides one request about one
object the gateway looked up: the probe id is denied (`probe`), the
anonymous subject is denied (`anonymous`), a subject in `Admins` is
allowed everything, the owner of an object that exists is allowed, and
everything else is denied `not_owner`, one reason whether the object is
another subject's or does not exist, so a deny discloses nothing. The
frame's create row, an id the caller chose on an object that does not
exist, never fires in Lux, because Lux mints every id; creation is
decided by Lux's rows. The rows, in order, before the frame:

- a subject in `LUX_ADMIN_SUBJECTS`, matched on the rendered subject
  string, is the frame's `Admins` and may do every action on every
  object;
- `provider.create`, `provider.update`, `provider.delete`,
  `model.create`, `model.update`, and `model.delete` are denied to
  every other subject, because those objects hold the operator's
  credentials and the operator's prices; the one exception is a
  Provider with `tunnel: true`, which holds neither, so any subject may
  create one, and `update`, `delete`, and `tunnel` fall to the frame,
  which allows the owner; its discovered Models are then usable by
  every subject under the rule below ([[013-tunnelled-runtimes]] states
  the consequence and the remedy, an authorizer);
- `provider.read`, `provider.list`, `model.read`, `model.list`, and
  `model.use` are allowed to every subject that is not anonymous: the
  catalog is the operator's and is offered to everyone the issuer
  admits, and a read returns no credential;
- `key.create` and `budget.create` are allowed to every subject that is
  not anonymous;
- `key.list`, `budget.list`, and `usage.read` are allowed with
  `filter.owners` set to the subject alone, so a list and the usage
  surface return the subject's own objects;
- every other action, `read`, `update`, `delete`, `draw`, and `tunnel`
  on a Key, a Budget, or a tunnelled Provider named by id, is the
  frame's: the owner is allowed and everyone else is `not_owner`;
- no limits are granted: `Limits` is zero, so `Defaults` and the
  manifest's own values hold.

The rows deny with the frame's reasons, `probe`, `anonymous`, and
`not_owner`, and two of Lux's: `admin_only` for a Provider or Model
declared, changed, or deleted by a subject that is not an admin, and
`unknown_action` for a string outside the vocabulary, which no row
allows. The probe and the anonymous subject are denied before the admin
row, so an admin is denied the probe like everyone. The frame's rows
look the object up by the resource's kind and id through an
`ObjectLookup` the store supplies at wiring, `auth.OwnerPolicy`
importing no store; a resource with no id names nothing and is
`not_owner`, and a lookup that fails is no decision,
`authorizer_unavailable`, because the policy could not decide.

With an authorizer set, `LUX_ADMIN_SUBJECTS` is read and unused, and
the start-up line says so. Every kind carries an `owner`, the rendered
subject that applied it; a discovered Model's owner is its Provider's.
This is a policy with tests, not the absence of one.

### The boundary between the planes

| | Control plane `/v1` | Data plane doors |
|---|---|---|
| Credential | a bearer from a listed issuer | a Key value in `Authorization: Bearer`, `x-api-key`, or `x-goog-api-key` ([[004-request-path]]) |
| Decided by | the authorizer, or the owner policy | the Key's resolved manifest and its windows, from the store |
| Dials | the issuer's keys (cached), the authorizer (cached) | nothing but the provider |
| Refusal when a decision is unavailable | `authorizer_unavailable`, 503 | none: there is no decision to be unavailable |
| Attribution | the subject | the Key's `owner` |

A Key presented on `/v1` is `unauthenticated`: it is not a JWS a listed
issuer signed. A door hashes whatever bearer it is given and asks the
store for that hash, reading nothing else about it ([[004-request-path]],
[[007-keys-and-limits]]), so an issuer token presented on a door is
`unauthenticated` too, because no Key has its hash; the doors take Keys
and nothing else, a person's token never leaves a person's tooling to
sit in a workload's environment, and a workload's Key never reaches
desired state. The one string that crosses is a platform's doing, not
the gateway's: a platform may create a Key whose `spec.value` is the
same string its developer presents to the platform's own control plane
as a token ([[007-keys-and-limits]], [[020-building-a-plane]]). At the
doors that string is opaque bytes matched by hash, it has no `lux_`
prefix, its expiry is the Key's `expiresAt` and nothing inside it, and
its revocation at the gateway is `DELETE` of the Key by the platform;
the doors fetch no revocation list and decode no token, which is
invariant 3 of [[001-architecture]] holding for a value a person also
holds.

### Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `LUX_OIDC_ISSUERS` | yes, unless `LUX_MANIFEST_DIR` | none | comma separated issuer URLs whose tokens are accepted on the control plane; each is an `http://` or `https://` URL with a host, rendered without its trailing slash, and one listed twice is a configuration error |
| `LUX_OIDC_AUDIENCE` | no | `lux` | the one audience a caller token must contain; a value with a comma is a configuration error, because a list is not accepted |
| `LUX_OIDC_INSECURE_ISSUERS` | no | unset | issuers from the list that may use `http://` on a host other than loopback; set by the test stubs, never in production; an entry that is not in `LUX_OIDC_ISSUERS` is a configuration error, since it permits nothing |
| `LUX_AUTHORIZER_URL`, `LUX_AUTHORIZER_TOKEN` | no | unset | the operator's authorization endpoint and the bearer `luxd` sends it; unset selects the owner policy; the URL without the token is a start-up failure, and the token without the URL is read and unused |
| `LUX_AUTHORIZE_LIST_ITEMS` | no | unset | `1` intersects a list with each candidate's read permission ([026-object-scoped-discovery](.archive/026-object-scoped-discovery.md)) |
| `LUX_AUTHORIZER_TIMEOUT` | no | `5s` | one decision's deadline, the retry included, a duration above zero; the cache times are the contract's, an allow for its `ttl` or `60s`, capped at `600s`, a deny `5s`, and are not settings |
| `LUX_ADMIN_SUBJECTS` | no | unset | comma separated rendered subjects, each `<iss>\|<sub>`, the owner policy lets act on every object and declare Providers and Models; read and unused when an authorizer is set; an entry without the separator is a configuration error |

Each problem is one clause without a semicolon, so the one sorted
message of [[002-repository-scaffold]] reads unambiguously. `auth.Startup`
builds the verifier and the client from the loaded configuration, or
nothing in the file mode, and its `String` is the start-up line: the
issuers, the audience, and who decides, never the token.

### What this spec needed in `latere.ai/x/pkg`

Both changes landed in pkg v0.66.0, which this repository pins, with
two more the stubs needed.

| Package | Change | Why |
|---|---|---|
| `authkit/jwt` | verifies `ES256` beside `RS256`: `alg: ES256` in the header is checked against the key set's `kty: EC`, `crv: P-256` keys and `RS256` against its RSA keys, never the other way round; `issuertest.WithES256` mints such tokens | the start-up check and `TestBearerAcceptance` name `ES256`, and the shared verifier accepts it |
| `authz/conformance` | `conformance.Run(t, url, token, opts...)`, the test every authorizer passes: the probe is denied for every subject and action, a wrong bearer and no bearer are refused, a well-formed request answers a 200 with `allow` a boolean, `ttl` when present a positive integer, `limits` an object, `filter` owners and labels, and a deny carries a reason; `WithActions` and `WithSubjects` name a core's vocabulary | this spec's `TestProbeIdIsAlwaysDenied` runs it against the stub under Lux's twenty-four actions, and [[020-building-a-plane]]'s `TestPlaneDocAuthorizerConforms` runs it against the document's authorizer |
| `authz/stub` | `FailBody(BodyMalformed\|BodyNoAllow)` and `PUT /fail {"body": ...}`, the outage whose answer is a 200 that is no decision | two of the six forms of unavailability above, driven from a test here and from [[015-test-stubs-and-tiers]]'s binary |
| `authkit/issuertest` | `GET /requests` and `DELETE /requests` | [[001-architecture]]'s `TestHotPathDialsNoWebhook` reads the issuer's record across a process boundary ([[015-test-stubs-and-tiers]]) |

### What the gateway never does

It never issues a token to a person, never stores a password, never
reads a group or role claim to decide anything, never calls an issuer
for anything but discovery and keys, never holds a session, and never
asks the authorizer about a data plane request. A dashboard that needs
sessions is a platform's; a plan that needs quotas is an authorizer's
`limits` and a Budget the platform applies.

### Design changes

**2026-09-17.** A person's token may now say what their credential may
do, and `luxd` reads it, on `latere.ai/x/pkg` v0.75.0. This is inside the
design above rather than beside it: the claims of a verified token
already reach the decision point verbatim, and the decision point is
already `latere.ai/x/pkg/authz`.

- **The claim.** A personal access token carries `token_use: pat` and
  RFC 9396's `authorization_details`: a set of grants, each naming
  actions of a published vocabulary qualified by their core,
  `lux:model.read`, and either one resource id or every resource of the
  kind. The gateway interprets none of it, the way it interprets no
  other claim. `jwt.Config.ReadsGrants` is set on every issuer's
  validator, and the claim reaches the authorizer in `claims` beside
  every other claim.
- **Why the flag is not optional.** The shared library refuses a token
  carrying grants when the verifier has not promised to read them,
  reason `grants_unread`, 401. The claim says what the credential may
  *not* do, so a verifier that accepts the token and applies nothing
  grants more than the holder asked for, silently: a refusal is visible,
  an ignored restriction is not. Setting the flag is a promise, kept at
  the site that decides.
- **The owner policy intersects.** `OwnerPolicy.Authorize` applies
  `authz.Restrict(core, decision, request, grants)` to its own answer.
  An allow no grant covers becomes a deny with reason `grant`; a deny is
  never turned into an allow, so the rows above stay the ceiling and a
  grant is never authority. `TestOwnerPolicyRestrictsToTheTokensGrants`
  is that case in process and `TestOwnerPolicyConforms` runs
  `authz/conformance` under `WithVocabulary` against the policy behind
  the shared scaffold, which drives the grant case with an admin
  subject: every row is allowed there, so a grant qualified by the wrong
  core denies the action its own grant names and the suite says so.
- **An operator's authorizer.** The request envelope carries the claims
  verbatim and always did, so an endpoint written on
  `latere.ai/x/pkg/authz/server` applies the same intersection with no
  code of its own, and [[020-building-a-plane]]'s example, which is on
  that scaffold, gets it by the version bump. An endpoint written by
  hand applies `authz.Restrict` to its own answer. The front that
  document prints reads the claim as `luxd` does and forwards it, so a
  platform built by copying it accepts a narrowed key rather than
  refusing it at the door.
- **A heading per resource kind.** `authorizer.Vocabulary()` declares
  `Providers`, `Models`, `Keys`, `Budgets`, and `Usage` through
  `authz.Vocabulary.WithLabels`, so a console offering a person a
  narrowed key groups the table by function and hard codes no heading.
  The action table itself is unchanged.
- **The start-up fetch is paid once.** The start-up check read each
  issuer's key set with its own client and left the validator holding
  nothing, so the first request fetched the same set again.
  `jwt.Validator.Warm` reads it into the validator inside the start-up
  the gateway already pays for; a failure there is the unreachable
  issuer the check already refuses.
- **What the gateway still does not do.** It mints no such token, offers
  no picker, and reads no claim for meaning. A token that is not a
  personal access token is decided exactly as before, whatever its
  claims say. A personal access token carrying no grant at all reaches
  nothing: an absent claim is not full access, which is the shared
  library's rule and not this gateway's.

## Not in this spec

Key verification, the `lux_` value, its hash, its cache, and its
states ([[007-keys-and-limits]]); the door headers and the data plane
refusals ([[004-request-path]]); the HTTP mapping of `unauthenticated`,
`forbidden`, and `authorizer_unavailable` ([[011-api]]); the stub
issuer and authorizer ([[015-test-stubs-and-tiers]]); the threat model
that motivates the plane boundary ([[016-security-and-threat-model]]);
how a platform writes an authorizer ([[020-building-a-plane]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `luxd` refuses to start with no issuer and no manifest directory, with an unreachable issuer, and with an issuer whose key set has no `RS256` or `ES256` key, each naming the issuer | `TestServeRefusesToStartWithoutAnIssuer` in `internal/config` and `cmd/luxd`, `TestUnreachableIssuerIsAStartupFailure`, `TestIssuerWithoutUsableKeysIsAStartupFailure` in `internal/auth` | passing; the call from `serve` lands with the merge |
| A token from a listed issuer with the audience is accepted; one with another issuer, another audience, an expired `exp`, a future `nbf`, or a bad signature is `unauthenticated` | `TestBearerAcceptance`, table-driven, `ES256` accepted beside `RS256` | passing |
| The family's shared audience suite passes against the verifier `luxd` installs: `LUX_OIDC_AUDIENCE` is admitted, a token addressed to the issuer itself, to another service, or to nobody is refused, a token that names no subject is refused, the issuer is called for its key set alone, and the retired `is_superadmin` flag grants no platform role | `TestConformance` in `internal/auth`, `latere.ai/x/pkg/authkit/conformance`'s `Run`, with `TestAudienceAtTheDoor` in `internal/api` on a protected route | passing |
| A Key value on `/v1` is `unauthenticated`; an issuer token on a door is `unauthenticated` when no Key's hash matches it and opens the door when a Key was created with it as `spec.value`, with the door decoding nothing | `TestPlanesRefuseEachOthersCredential`, [[004-request-path]]'s `TestSuppliedValueOpensTheDoor` | passing: the `/v1` half here, the door half in `gateway` |
| An issuer whose keys become unreachable after start keeps verifying tokens signed by the cached keys and refuses one with an unknown `kid` | `TestStaleKeySetServesUntilRefresh` | passing |
| With the stub authorizer, every action in the table is sent with the `resource` shape in the table, and the request carries `subject`, `issuer`, `sub`, and every claim of the token in `claims` verbatim | `TestAuthorizerRequestShapes`, table-driven over every action, with `TestResourceShapes`, the `authorizer` package's ([[022-authorizer-vocabulary-package]]) | passing |
| Each unavailability form, refused connection, TLS failure, non-200, unparseable body, body without `allow`, and timeout, is `authorizer_unavailable` and none is an allow; a connection failure before a response line is retried once and nothing else is; a data plane request during each is served | `TestAuthorizerUnavailability`, `TestAuthorizerRetriesOnlyBeforeAResponseLine`, `TestDataPlaneServesWhileAuthorizerIsDown` | the first two pass; the third is not built and lands with [[004-request-path]]'s doors |
| A `deny` on `model.use`, `budget.draw`, or a target's `provider.read` at resolve is `not_found` naming the field; a `deny` on the request's own action is `forbidden` with the reason in the developer detail only | `TestLookupDenyIsNotFound`, `TestDenyReasonStaysOutOfTheUserSentence` | passing |
| Every `limits` field reaches its consumer: the control plane rate, the four `Resolve` limits refusing with `ceiling_exceeded`, and `max_keys` refusing the next `key.create` | `TestAuthorizerLimitsReachTheirConsumers`, `TestDecodeLimits`, the `authorizer` package's ([[022-authorizer-vocabulary-package]]) | passing to the figures, the rate, `manifest.Limits` refusing through `Resolve`, and `max_keys`; the rate bucket and the `key.create` check are [[011-api]]'s |
| `filter` narrows `list` and `usage.read` to the owners and labels named | `TestAuthorizerFilter` | passing to the decision; the narrowing of a list is [[011-api]]'s |
| An allow is cached for the answer's `ttl`, `60s` when it names none, and at the `600s` cap, a deny for `5s`, unavailability never, and an answer about a resource with no id never, so ten applies of a Key naming one selector send ten `model.use` calls; a revoked subject is refused on the control plane within the allow's `ttl` | `TestDecisionCache`, `TestNoIdIsNeverCached` | passing |
| `authz.ProbeID` is denied by the stub authorizer and by the owner policy for every subject and action, `luxd check` reports an authorizer that allows it, and an item route given the probe id answers `not_found` | `TestProbeIdIsAlwaysDenied` | passing for the stub, the owner policy, and `Check`; `luxd check` is [[017-release-and-installation]]'s and the item route [[011-api]]'s |
| The owner policy: an admin declares a Provider and a Model and a non-admin cannot, except a `tunnel: true` Provider, which its creator owns; every subject reads the catalog and uses every Model in a Key; an owner reads, updates, deletes, and draws its own objects and no other subject's, the deny reason being `not_owner` whether the object exists or not; `list` returns only the subject's own Keys and Budgets; no `Limits` are granted | `TestOwnerPolicy`, table-driven over every action and both roles | passing |
| A Key applied with a token whose `sub` is a person is owned by `<iss>\|<person>`; one applied with a service token is owned by `<iss>\|<service account>`; the issuer is rendered without a trailing slash in both | `TestOwnerIsTheTokensSubject`, `TestSubjectRendering` | passing for the subject each token renders to; the write of `owner` is [[011-api]]'s |
| `LUX_AUTHORIZER_TOKEN` is sent as the bearer of every authorizer call and never as a bearer to any other endpoint; the issuer's and the sink's requests carry other credentials | `TestAuthorizerTokenStaysOnItsEndpoint`, over the e2e capture | passing over a recording transport for the issuer and the authorizer; the e2e capture with the sink is [[015-test-stubs-and-tiers]]'s |
| During one thousand data plane requests with the stub authorizer and issuer wired, both receive zero calls | [[001-architecture]]'s `TestHotPathDialsNoWebhook` | not built |
| A read of a Provider through any route or event returns no credential value | [[005-providers]]'s `TestProviderCredentialNeverLeavesTheGateway` | not built |
| With an authorizer set, a subject listed in `LUX_ADMIN_SUBJECTS` receives no allow the authorizer did not give: the variable is read, reported as unused at start, and consulted by no decision | `TestAdminSubjectsIgnoredUnderAnAuthorizer` | passing |

## Outcome

Built on 2026-09-14 in `internal/auth` and `internal/config`, on pkg
v0.66.0, on a branch beside [[010-state]]'s store. Coverage is 99.3% of
`internal/auth` and 100% of `internal/config`; the gate passes whole,
and the identity gate's two waivers, `verifier` and `authorizer`, are
gone, because the verifier is `authkit/jwt` and the authorizer
`pkg/authz`. The Design text above was changed wherever the code had to
say more than the dispatched text, so the two agree; the additions and
their reasons:

- The start-up check reads the discovery document's `issuer` and
  refuses one that names another issuer: a token it signs would carry
  an `iss` the validator refuses, so every request would fail late
  instead of the start failing once. A usable key is judged by `kty`,
  `crv`, and an absent or matching `alg`, which is what the shared
  verifier reads.
- The bearer's `iss` is read unverified to pick the issuer's validator,
  and a token with no `exp` is refused with that finding rather than as
  expired, because the shared verifier reads an absent `exp` as the
  epoch.
- A `limits` object the gateway cannot read is `authorizer_unavailable`.
  The text listed the forms of no decision by transport and body shape;
  a ceiling that does not parse is no ceiling the gateway can hold, and
  an allow it cannot apply whole is not an allow.
- A refused and a missing reference carry one developer detail naming
  the reference and not the authorizer's reason, since a detail that
  differed would disclose which of the two it was
  ([[016-security-and-threat-model]]).
- `model.use` is asked for a selector that matches nothing, since the
  decision binds the selector.
- Every list and map field of a resource is present and never `null`,
  and a create's `amount` is `""` when absent.
- The owner policy has two reasons of its own, `admin_only` and
  `unknown_action`, denies the probe and the anonymous subject before
  the admin row, and reads objects through an `ObjectLookup` interface
  rather than a store; a lookup failure is no decision.
- The configuration refuses an issuer listed twice, an insecure issuer
  not in the list, an audience with a comma, a timeout at or below
  zero, and an admin subject without the `|` separator, each as one
  clause without a semicolon so the one sorted line stays readable.
  `LUX_AUTHORIZER_TOKEN` without the URL is accepted and unused.
  `LUX_MANIFEST_DIR` is read through the environment for the one rule
  this spec has about it; the field is [[010-state]]'s.

What the merge wires, since the store and `serve` were built beside
this: one call, `auth.Startup(ctx, cfg, client)` after `config.Load`
in `serve`, its error a start-up failure and its `String()` the
start-up line; `Auth.Authorizer(objects)` with the store's object
lookup once, and `Authorizer.Lookup(caller, request, catalog)` per
request with the store's catalog. `cmd/luxd`'s build list then reaches
the OpenTelemetry SDK and its dependencies through the shared verifier's
instrumented client, so the `depcheck` rows for `go.opentelemetry.io/`,
`github.com/google/uuid`, `github.com/cenkalti/backoff/v5`,
`github.com/cespare/xxhash/v2`, `github.com/felixge/httpsnoop`,
`github.com/go-logr/`, `github.com/grpc-ecosystem/grpc-gateway/v2`,
`golang.org/x/`, and `google.golang.org/` land with that commit; the
gate refuses a stale allowance, so they cannot land before it.

What other specs carry from this: [[011-api]] maps `auth.Error`'s three
codes to 401, 403, and 503, reads `Decision.Limits.RequestsPerMinute`
for the rate bucket and `Decision.Limits.MaxKeys` at `key.create`,
passes `Decision.Limits.Key` as `Options.Limits`, and reports
`Auth.Policy` in `/v1/self` beside the `limits` and `filter` of the
subject's last allow, which that spec's surface memoises itself rather
than reading from the shared client's cache, since that cache is keyed
by subject, action, and resource id and cannot be read by subject
alone; a `Catalog` failure inside `Resolve` passes through as the
store's own error, which the API answers as `store_unavailable`. [[017-release-and-installation]]
says the probe's answer is not entered in the decision cache; the
shared client holds its deny for five seconds like any deny, under the
anonymous subject and the reserved id, which no request about an object
shares, so a check run changes what no later request is told and the
sentence holds in effect. [[004-request-path]] owns the door half of
the plane boundary as `TestSuppliedValueOpensTheDoor`;
[[004-request-path]] `TestDataPlaneServesWhileAuthorizerIsDown`;
[[015-test-stubs-and-tiers]] the e2e capture of
`TestAuthorizerTokenStaysOnItsEndpoint`; [[001-architecture]] and
[[005-providers]] their two rows, left not built.

Reference decisions during apply (`provider.read`, `model.use`, `budget.draw`)
include `resource.binding: {kind, proposed}` for the enclosing mutation. This is
the same sanitized desired state sent to the create/update decision, including
the target owner; the caller remains the authenticated writer. Policies can bind
model permissions to that Key's selected Budget. Direct reads omit `binding`.
Key rotation sends its unchanged sanitized Key as `resource.proposed`, with
`resource.operation: "rotate"`, on `key.update`.
