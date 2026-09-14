---
title: "Identity: OIDC issuers, subjects, the authorizer webhook, the owner policy"
status: in-progress
track: core
depends_on:
  - specs/001-architecture.md
  - specs/002-repository-scaffold.md
  - specs/003-manifest-contract.md
affects: [internal/auth/, internal/api/, internal/config/, test/stubs/, docs/]
effort: medium
created: 2026-09-13
updated: 2026-09-14
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

Nothing is built. The repository holds the scaffold of
[[002-repository-scaffold]]: the binary serving its probes, typed
configuration, and the gate, on pkg v0.65.0.

Amended on 2026-09-13: the verifier, the authorizer envelope, its cache
and retry rules, the subject string and the probe id are the shared
contract of `latere.ai/x/pkg/authz`, so one authorizer can serve several
open cores.

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
| `model.use` | `{"kind": "Model", "selector": "anthropic/*", "matched": [{"id", "name", "owner"}]}`; asked once per selector at Key resolve through `Lookup.Models` ([[003-manifest-contract]]); the decision binds the selector, and the data plane matches it at request time against the catalog then |
| `key.create` | `{"kind": "Key", "name", "labels", "models", "budget"}` |
| `key.read`, `.update`, `.delete` | `{"kind": "Key", "id", "name", "owner", "prefix", "labels"}` |
| `key.list` | `{"kind": "Key"}`; `filter` applies |
| `budget.create` | `{"kind": "Budget", "name", "amount", "currency", "window", "labels"}` |
| `budget.read`, `.update`, `.delete` | `{"kind": "Budget", "id", "name", "owner", "labels"}` |
| `budget.list` | `{"kind": "Budget"}`; `filter` applies |
| `budget.draw` | the Budget as above; asked at Key resolve through `Lookup.Budget` |
| `usage.read` | `{"kind": "Usage", "keys": [ids], "owners": [subjects]}` from the query, `keys` being ids because the API resolves names first ([[011-api]]); the response's `filter` is intersected with the query and never widens it ([[009-usage-and-metering]]) |

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
  a body without `allow`, a body over 64 KiB, and a timeout of
  `LUX_AUTHORIZER_TIMEOUT` (default `5s`), which bounds one decision
  with its retry included. The call is retried once when the connection
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
  ([[003-manifest-contract]], [[016-security-and-threat-model]]).
- The API constructs `Lookup` per request from the caller's subject and
  the cache below; an importer constructs its own.
- An allow is cached per replica for the answer's `ttl`, `60s` when
  the answer names none, capped at `600s`; a deny for `5s`;
  unavailability never; under the key of subject, action, and resource
  id, with the `limits` and `filter` that came with them
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
  authorizer passes is owed by that package and not written yet, below.

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
| `LUX_OIDC_ISSUERS` | yes, unless `LUX_MANIFEST_DIR` | none | comma separated issuer URLs whose tokens are accepted on the control plane |
| `LUX_OIDC_AUDIENCE` | no | `lux` | the one audience a caller token must contain; a list is not accepted |
| `LUX_OIDC_INSECURE_ISSUERS` | no | unset | issuers from the list that may use `http://` on a host other than loopback; set by the test stubs, never in production |
| `LUX_AUTHORIZER_URL`, `LUX_AUTHORIZER_TOKEN` | no | unset | the operator's authorization endpoint and the bearer `luxd` sends it; unset selects the owner policy; the URL without the token is a start-up failure |
| `LUX_AUTHORIZER_TIMEOUT` | no | `5s` | one decision's deadline, the retry included; the cache times are the contract's, an allow for its `ttl` or `60s`, capped at `600s`, a deny `5s`, and are not settings |
| `LUX_ADMIN_SUBJECTS` | no | unset | comma separated rendered subjects the owner policy lets act on every object and declare Providers and Models; read and unused when an authorizer is set |

### Changes this spec needs in `latere.ai/x/pkg`

| Package | Change | Why |
|---|---|---|
| `authkit/jwt` | verify `ES256` beside `RS256`: accept `alg: ES256` in the header and `kty: EC` keys in the key set | the start-up check and `TestBearerAcceptance` above name `ES256`; the shared verifier is expected to accept it; today the package refuses the algorithm and skips the keys |
| `authz` | a conformance test an authorizer passes, `authz/conformance` or a `Conformance(t, url, token)` in the package: the probe is denied for every subject, a wrong bearer is refused, a well-formed request answers a 200 with `allow`, `ttl` when present is a positive integer, and `filter` and `limits` when present have the contract's shape | the package is where a shared conformance test belongs, and this spec's authorizer criteria and [[020-building-a-plane]]'s `TestPlaneDocAuthorizerConforms` run it; nothing in the package does yet |

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
| A Key value on `/v1` is `unauthenticated`; an issuer token on a door is `unauthenticated` when no Key's hash matches it and opens the door when a Key was created with it as `spec.value`, with the door decoding nothing | `TestPlanesRefuseEachOthersCredential`, `TestSuppliedTokenIsAKeyAtTheDoor` | not built |
| An issuer whose keys become unreachable after start keeps verifying tokens signed by the cached keys and refuses one with an unknown `kid` | `TestStaleKeySetServesUntilRefresh` | not built |
| With the stub authorizer, every action in the table is sent with the `resource` shape in the table, and the request carries `subject`, `issuer`, `sub`, and every claim of the token in `claims` verbatim | `TestAuthorizerRequestShapes`, table-driven over every action | not built |
| Each unavailability form, refused connection, TLS failure, non-200, unparseable body, body without `allow`, and timeout, is `authorizer_unavailable` and none is an allow; a connection failure before a response line is retried once and nothing else is; a data plane request during each is served | `TestAuthorizerUnavailability`, `TestAuthorizerRetriesOnlyBeforeAResponseLine`, `TestDataPlaneServesWhileAuthorizerIsDown` | not built |
| A `deny` on `model.use`, `budget.draw`, or a target's `provider.read` at resolve is `not_found` naming the field; a `deny` on the request's own action is `forbidden` with the reason in the developer detail only | `TestLookupDenyIsNotFound`, `TestDenyReasonStaysOutOfTheUserSentence` | not built |
| Every `limits` field reaches its consumer: the control plane rate, the four `Resolve` limits refusing with `ceiling_exceeded`, and `max_keys` refusing the next `key.create` | `TestAuthorizerLimitsReachTheirConsumers` | not built |
| `filter` narrows `list` and `usage.read` to the owners and labels named | `TestAuthorizerFilter` | not built |
| An allow is cached for the answer's `ttl`, `60s` when it names none, and at the `600s` cap, a deny for `5s`, unavailability never, and an answer about a resource with no id never, so ten applies of a Key naming one selector send ten `model.use` calls; a revoked subject is refused on the control plane within the allow's `ttl` | `TestDecisionCache`, `TestNoIdIsNeverCached` | not built |
| `authz.ProbeID` is denied by the stub authorizer and by the owner policy for every subject and action, `luxd check` reports an authorizer that allows it, and an item route given the probe id answers `not_found` | `TestProbeIdIsAlwaysDenied` | not built |
| The owner policy: an admin declares a Provider and a Model and a non-admin cannot, except a `tunnel: true` Provider, which its creator owns; every subject reads the catalog and uses every Model in a Key; an owner reads, updates, deletes, and draws its own objects and no other subject's, the deny reason being `not_owner` whether the object exists or not; `list` returns only the subject's own Keys and Budgets; no `Limits` are granted | `TestOwnerPolicy`, table-driven over every action and both roles | not built |
| A Key applied with a token whose `sub` is a person is owned by `<iss>\|<person>`; one applied with a service token is owned by `<iss>\|<service account>`; the issuer is rendered without a trailing slash in both | `TestOwnerIsTheTokensSubject` | not built |
| `LUX_AUTHORIZER_TOKEN` is sent as the bearer of every authorizer call and never as a bearer to any other endpoint; the issuer's and the sink's requests carry other credentials | `TestAuthorizerTokenStaysOnItsEndpoint`, over the e2e capture | not built |
| During one thousand data plane requests with the stub authorizer and issuer wired, both receive zero calls | [[001-architecture]]'s `TestHotPathDialsNoWebhook` | not built |
| A read of a Provider through any route or event returns no credential value | [[005-providers]]'s `TestProviderCredentialNeverLeavesTheGateway` | not built |
| With an authorizer set, a subject listed in `LUX_ADMIN_SUBJECTS` receives no allow the authorizer did not give: the variable is read, reported as unused at start, and consulted by no decision | `TestAdminSubjectsIgnoredUnderAnAuthorizer` | not built |
