---
title: "Identity: OIDC issuers, subjects, the authorizer webhook, the owner policy"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/003-manifest-contract.md
affects: [internal/auth/, internal/api/, internal/config/, test/stubs/, docs/]
effort: medium
created: 2026-09-13
updated: 2026-09-13
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

Nothing is built. The hosted gateway this design is extracted from
trusts one issuer with a hardcoded default hostname, reads that
issuer's organization and role claims to decide who is an
administrator, and keeps a browser session for its dashboard. Every
one of those is what this spec moves out of the gateway.

Amended on 2026-09-13 by the family decision "one platform over open
cores" (latere-ai/specs, `decisions/2026-09-13-one-platform-open-cores.md`):
the claims forwarded, the cache and retry rules, the verifier, the
subject string and the probe id are one contract shared by the three
open cores, Cella, Lux and Origo, so one authorizer serves all three.

## Design

### Subjects

A subject is the pair of the issuer and the `sub` claim, rendered as
one string everywhere it is stored or sent: `<iss>|<sub>`. `owner`
fields, event `subject` fields, `LUX_ADMIN_SUBJECTS` entries, and the
authorizer request all carry the rendered string; the authorizer
request also carries `issuer` and `sub` apart. A Key is not a subject:
a data plane request has no subject, it has a Key, and the record and
the events carry the Key's `owner`, the subject that applied it, as the
attribution ([[009-usage-and-metering]]).

### Caller identity

`LUX_OIDC_ISSUERS` lists issuer URLs. At start `luxd` fetches each
`/.well-known/openid-configuration` and its `jwks_uri` and refuses to
start when any is unreachable or lists no `RS256` or `ES256` key;
afterwards it verifies through `latere.ai/x/pkg/authkit/jwt`, which
caches a key set for its TTL, refreshes it on an unknown `kid` under
that package's rate limit, and serves the stale set while a refresh
fails, so an issuer that goes away later degrades to refusing new keys
rather than every request. A request's bearer is accepted when it is a
JWS signed by a listed issuer's key, `iss` matches, `aud` contains
`LUX_OIDC_AUDIENCE` (default `lux`), `exp` is in the future, and `nbf`
if present is past. Every claim of the verified token is handed to the
authorizer verbatim in `claims`, and none is interpreted by the
gateway: an issuer's organisation, role, or group claims mean something
to the authorizer that reads them and nothing to `luxd`. An `http://`
issuer is refused unless it is on a loopback
address or in `LUX_OIDC_INSECURE_ISSUERS`.

A control plane request without a bearer is `unauthenticated`, 401. A
control plane request whose bearer is a Key value is `unauthenticated`
too: a Key opens the doors and nothing else, so a leaked Key cannot
read or change desired state. There is no API key for the control
plane and no anonymous access; a caller that wants a long-lived
control plane credential gets one from its issuer. A platform that
provisions Keys for its sandboxes does so with a service token from
its own issuer whose `aud` is `lux`, one credential kind and one hop
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
issuer commonly stamps beside two a platform's issuer adds. `resource`
per action:

| Action | `resource` |
|---|---|
| `provider.create` | `{"kind": "Provider", "name", "dialect", "baseURL", "tunnel", "labels"}` from the manifest; no `id` yet |
| `provider.read`, `.update`, `.delete`, `.tunnel` | `{"kind": "Provider", "id", "name", "owner", "dialect", "baseURL", "tunnel", "labels"}`; `tunnel` is asked when an agent opens a session for a tunnelled Provider ([[013-tunnelled-runtimes]]) |
| `provider.list` | `{"kind": "Provider"}`; the response may carry `filter` |
| `model.create` | `{"kind": "Model", "name", "targets": [{"provider", "model"}], "labels"}` |
| `model.read`, `.update`, `.delete` | `{"kind": "Model", "id", "name", "owner", "source", "labels"}` |
| `model.list` | `{"kind": "Model"}`; `filter` applies |
| `model.use` | `{"kind": "Model", "selector": "anthropic/*", "matched": [{"id", "name", "owner"}]}`; asked once per selector at Key resolve through `Lookup.Models` ([[003-manifest-contract]]); the decision binds the selector, and the data plane matches it at request time against the catalog then |
| `key.create` | `{"kind": "Key", "name", "labels", "models", "budget"}` |
| `key.read`, `.update`, `.delete` | `{"kind": "Key", "id", "name", "owner", "prefix", "labels"}` |
| `key.list` | `{"kind": "Key"}`; `filter` applies |
| `budget.create` | `{"kind": "Budget", "name", "amount", "currency", "window", "labels"}` |
| `budget.read`, `.update`, `.delete` | `{"kind": "Budget", "id", "name", "owner", "labels"}` |
| `budget.list` | `{"kind": "Budget"}`; `filter` applies |
| `budget.draw` | the Budget as above; asked at Key resolve through `Lookup.Budget` |
| `usage.read` | `{"kind": "Usage", "keys": [ids], "owners": [subjects]}` from the query; the response's `filter` narrows the aggregation ([[009-usage-and-metering]]) |

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

`ttl` is optional, the seconds this allow may be cached, default
`LUX_AUTHORIZER_CACHE`, capped at `600`. `limits` is optional and every
field in it is optional: an absent field means the configured value or
no limit. `requests_per_minute` overrides
`LUX_REQUESTS_PER_MINUTE` for this subject on the control plane
([[011-api]]); the four `max_key_*` fields reach `Resolve` as `Limits`
([[003-manifest-contract]]) and cap what a Key this subject applies may
ask for; `max_keys` caps the subject's live Keys, checked by the API at
`key.create` and refused with `ceiling_exceeded`. `filter`, on a
`list` action or `usage.read`, narrows the result to the owners and
labels named.

Rules:

- A decision is any of the actions' outcomes. Everything else is
  `authorizer_unavailable`, 503, and never an allow: connection
  refused, a TLS failure, a non-200 status, a body that does not parse,
  a body without `allow`, and a timeout of `LUX_AUTHORIZER_TIMEOUT`
  (default `5s`). The call is retried once when the connection failed
  before a response line arrived, a refused or reset connection or a
  dial timeout, and never on a non-200, a timeout after the request
  was sent, or a body that does not parse. An `http://` authorizer URL is
  refused at start unless it is on a loopback address.
  `LUX_AUTHORIZER_URL` without `LUX_AUTHORIZER_TOKEN` is a start-up
  failure. Availability is not a readiness check: a flapping endpoint
  fails control plane requests, not replicas, and never a data plane
  request.
- A `deny` on a request's own action is `forbidden`, 403, with the
  authorizer's `reason` as the developer detail and never in the user
  sentence. A `deny` on `model.use`, `budget.draw`, or `provider.read`
  for a target, asked through `Lookup` at resolve, is `not_found`, so a
  refused object and a missing one are the same answer
  ([[003-manifest-contract]], [[016-security-and-threat-model]]).
- The API constructs `Lookup` per request from the caller's subject and
  the cache below; an importer constructs its own.
- An allow is cached per replica for the answer's `ttl`,
  `LUX_AUTHORIZER_CACHE` (default `60s`) when the answer names none,
  capped at `600s`; a deny for `5s`; unavailability never; under the
  key of subject, action, and resource id (empty for `create` and
  `list`; the selector string for `model.use`), with the `limits` and
  `filter` that came with them. A revocation at the authorizer
  therefore takes effect on the control plane within the allow's
  `ttl`, which the authorizer chooses. It takes effect on the data
  plane only through the Keys: a platform
  that revokes a subject deletes or disables its Keys, and the gateway
  stops serving them within `LUX_KEY_CACHE` ([[007-keys-and-limits]]).
  The authorizer is never asked about a data plane request, by design
  and by test ([[001-architecture]], `TestHotPathDialsNoWebhook`).
- The resource id `key_00000000000000000000000000` is reserved as a
  probe: every authorizer denies it for every subject and every action,
  and `luxd check` ([[017-release-and-installation]]) sends it and
  reads an allow as an endpoint that does not read the request. The
  owner policy denies it too.
- The envelope, the client, the cache, the retry, the owner policy's
  frame, the stub authorizer, and the conformance test an authorizer
  passes are `latere.ai/x/pkg/authz`, shared with the sibling open
  cores; `luxd` adds its action vocabulary and its `resource` shapes
  and nothing else.

### The owner policy

With `LUX_AUTHORIZER_URL` unset, `LUX_ADMIN_SUBJECTS` is read and the
log says `owner policy` at start:

- a subject in `LUX_ADMIN_SUBJECTS`, matched on the rendered subject
  string, may do every action on every object, and is the only subject
  that may `create`, `update`, or `delete` a `Provider` or a `Model`,
  because those hold the operator's credentials and the operator's
  prices; the one exception is a Provider with `tunnel: true`, which
  holds neither, so any subject may create, update, delete, and `tunnel`
  one it owns, and its discovered Models are then usable by every
  subject under the rule below ([[013-tunnelled-runtimes]] states the
  consequence and the remedy, an authorizer);
- every subject may `read` and `list` every `Provider` (without its
  credential, which no read returns) and every `Model`, and may `use`
  every `Model`: the catalog is the operator's and is offered to
  everyone the issuer admits;
- a subject may `create` a `Key` or a `Budget`, and may `read`,
  `update`, `delete`, and `draw` an object whose `owner` is that
  subject; `list` returns the subject's own objects; `usage.read`
  returns the subject's own Keys' usage;
- no limits are granted: `Limits` is zero, so `Defaults` and the
  manifest's own values hold.

With an authorizer set, `LUX_ADMIN_SUBJECTS` is read and unused. Every
kind carries an `owner`, the rendered subject that applied it; a
discovered Model's owner is its Provider's. This is a policy with
tests, not the absence of one.

### The boundary between the planes

| | Control plane `/v1` | Data plane doors |
|---|---|---|
| Credential | a bearer from a listed issuer | a Key value in `Authorization: Bearer`, `x-api-key`, or `x-goog-api-key` ([[004-request-path]]) |
| Decided by | the authorizer, or the owner policy | the Key's resolved manifest and its windows, from the store |
| Dials | the issuer's keys (cached), the authorizer (cached) | nothing but the provider |
| Refusal when a decision is unavailable | `authorizer_unavailable`, 503 | none: there is no decision to be unavailable |
| Attribution | the subject | the Key's `owner` |

A Key presented on `/v1` is `unauthenticated`. An issuer token presented
on a door is `unauthenticated` too ([[004-request-path]]): the doors
take Keys and nothing else, so a person's token never leaves a person's
tooling to sit in a workload's environment, and a workload's Key never
reaches desired state.

### Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `LUX_OIDC_ISSUERS` | yes, unless `LUX_MANIFEST_DIR` | none | comma separated issuer URLs whose tokens are accepted on the control plane |
| `LUX_OIDC_AUDIENCE` | no | `lux` | the audience a caller token must contain |
| `LUX_OIDC_INSECURE_ISSUERS` | no | unset | issuers from the list that may use `http://` on a host other than loopback; set by the test stubs, never in production |
| `LUX_AUTHORIZER_URL`, `LUX_AUTHORIZER_TOKEN` | no | unset | the operator's authorization endpoint and the bearer `luxd` sends it; unset selects the owner policy; the URL without the token is a start-up failure |
| `LUX_AUTHORIZER_TIMEOUT`, `LUX_AUTHORIZER_CACHE` | no | `5s`, `60s` | one decision's deadline; the `ttl` of an allow whose answer names none, capped at `600s`; a deny is held `5s` |
| `LUX_ADMIN_SUBJECTS` | no | unset | comma separated rendered subjects the owner policy lets act on every object and declare Providers and Models; read and unused when an authorizer is set |

### What the gateway never does

It never issues a token to a person, never stores a password, never
reads a group or role claim to decide anything, never calls an issuer
for anything but discovery and keys, never holds a session, and never
asks the authorizer about a data plane request. A dashboard that needs
sessions is a platform's; a plan that needs quotas is an authorizer's
`limits` and a Budget the platform applies.

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
| `luxd` refuses to start with no issuer and no manifest directory, with an unreachable issuer, and with an issuer whose key set has no `RS256` or `ES256` key, each naming the issuer | `TestServeRefusesToStartWithoutAnIssuer`, `TestUnreachableIssuerIsAStartupFailure`, `TestIssuerWithoutUsableKeysIsAStartupFailure` | not built |
| A token from a listed issuer with the audience is accepted; one with another issuer, another audience, an expired `exp`, a future `nbf`, or a bad signature is `unauthenticated` | `TestBearerAcceptance`, table-driven | not built |
| A Key value on `/v1` and an issuer token on a door are each `unauthenticated` | `TestPlanesRefuseEachOthersCredential` | not built |
| An issuer whose keys become unreachable after start keeps verifying tokens signed by the cached keys and refuses one with an unknown `kid` | `TestStaleKeySetServesUntilRefresh` | not built |
| With the stub authorizer, every action in the table is sent with the `resource` shape in the table, and the request carries `subject`, `issuer`, `sub`, and every claim of the token in `claims` verbatim | `TestAuthorizerRequestShapes`, table-driven over every action | not built |
| Each unavailability form, refused connection, TLS failure, non-200, unparseable body, body without `allow`, and timeout, is `authorizer_unavailable` and none is an allow; a connection failure before a response line is retried once and nothing else is; a data plane request during each is served | `TestAuthorizerUnavailability`, `TestAuthorizerRetriesOnlyBeforeAResponseLine`, `TestDataPlaneServesWhileAuthorizerIsDown` | not built |
| A `deny` on `model.use`, `budget.draw`, or a target's `provider.read` at resolve is `not_found` naming the field; a `deny` on the request's own action is `forbidden` with the reason in the developer detail only | `TestLookupDenyIsNotFound`, `TestDenyReasonStaysOutOfTheUserSentence` | not built |
| Every `limits` field reaches its consumer: the control plane rate, the four `Resolve` limits refusing with `ceiling_exceeded`, and `max_keys` refusing the next `key.create` | `TestAuthorizerLimitsReachTheirConsumers` | not built |
| `filter` narrows `list` and `usage.read` to the owners and labels named | `TestAuthorizerFilter` | not built |
| An allow is cached for the answer's `ttl` and at the `600s` cap, a deny for `5s`, unavailability never; a revoked subject is refused on the control plane within the allow's `ttl` | `TestDecisionCache` | not built |
| The probe id is denied by the stub authorizer and by the owner policy for every subject and action, and `luxd check` reports an authorizer that allows it | `TestProbeIdIsAlwaysDenied` | not built |
| The owner policy: an admin declares a Provider and a Model and a non-admin cannot; every subject reads the catalog and uses every Model in a Key; an owner reads, updates, deletes, and draws its own objects and no other subject's; `list` returns only the subject's own Keys and Budgets; no `Limits` are granted | `TestOwnerPolicy`, table-driven over every action and both roles | not built |
| During one thousand data plane requests with the stub authorizer and issuer wired, both receive zero calls | [[001-architecture]]'s `TestHotPathDialsNoWebhook` | not built |
| A read of a Provider through any route or event returns no credential value | [[005-providers]]'s `TestProviderCredentialNeverLeavesTheGateway` | not built |
