---
title: "Workload identity federation: an anthropic Provider's credential is a short-lived token the gateway gets for its own workload identity at Anthropic's token endpoint, so no long-lived key is stored"
status: testing
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/005-providers.md
affects: [manifest/v1/, manifest/, gateway/, internal/federation/, internal/serve/, internal/api/, internal/config/, internal/check/, cmd/luxd/, api/openapi.yaml, docs/, test/conformance/, specs/002-repository-scaffold.md, specs/003-manifest-contract.md, specs/005-providers.md]
effort: medium
created: 2026-10-24
updated: 2026-10-24
author: changkun
---

# Workload identity federation

## Overview

A Provider's credential today is a value the gateway holds: sealed in
the store in server mode, read from the environment in the file mode,
and written into a header of every request toward the upstream. A
dump of the store's credential rows and the key encryption keys is
every upstream's API key, valid until someone revokes it by hand.

Anthropic accepts a second way in. A workload presents a JWT its own
identity provider signed, such as a Kubernetes service account token,
at `POST /v1/oauth/token` with the RFC 7523 `jwt-bearer` grant, and
gets back an `sk-ant-oat01-` access token that lasts from one minute
to a day and acts as an Anthropic service account. The Anthropic
organization trusts the issuer through a federation issuer and a
federation rule it configures. Nothing long-lived is stored.

A Provider's credential gains `federation`: the identifiers of the
rule, the organization, the service account and optionally the
workspace. The gateway mints or reads its own workload's identity
token, exchanges it, keeps the access token in memory, sends it as
`Authorization: Bearer`, and exchanges again before it expires.

## Current state

- `Credential` carries a write-only value or `valueFrom.env`, with a
  header and a scheme; the gateway opens the value for each request
  through spec 004's `CredentialSource` (`gateway/forward.go`), and the
  discovery and health jobs open it the same way (`internal/serve`).
- The anthropic dialect sends the value as `x-api-key`, raw.
- Nothing in the gateway exchanges a token or refreshes one.

## Design

### The manifest

```yaml
spec:
  dialect: anthropic
  baseURL: https://api.anthropic.com/v1
  credential:
    federation:
      ruleID: fdrl_01AbC...
      organizationID: 00000000-0000-0000-0000-000000000000
      serviceAccountID: svac_01AbC...
      workspaceID: wrkspc_01AbC...   # optional
```

| Field | Rule |
|---|---|
| `credential.federation` | an anthropic Provider's alone; with `value` or `valueFrom` it is `exclusive_fields`; on any other dialect `invalid_field` |
| `federation.ruleID` | `fdrl_` and 1 to 128 letters, digits, `_` or `-` |
| `federation.organizationID` | a UUID |
| `federation.serviceAccountID` | `svac_` and 1 to 128 letters, digits, `_` or `-` |
| `federation.workspaceID` | optional; `wrkspc_` and 1 to 128 letters, digits, `_` or `-`; Anthropic needs it when the rule covers more than one workspace |
| `credential.header`, `credential.scheme` | default `Authorization` and `bearer` with `federation`; any other value is `invalid_field`, since the access token travels nowhere else |

A Provider with `federation` is refused `invalid_field` where the
gateway has no workload identity (below), as `tunnel: true` is refused
without the tunnel.

The identifiers are not secret and render like every other field. A
static header named `Authorization` on such a Provider is the
credential header and refused `reserved_prefix`.

### The stored value

A Provider with `federation` holds no value. An apply that sets it
deletes a stored value in the same transaction and leaves
`status.credential.set` false, at the version the value had, so a value
applied later takes the next version and a version is never reused. An
apply switching back to a value seals it as any value is sealed.

### The workload identity

The identity token is the operator's to configure, never a Provider's:

| Variable | Means |
|---|---|
| `LUX_WORKLOAD_IDENTITY_FILE` | a path holding a JWT, read again for every exchange, for a token another process keeps fresh |
| `LUX_WORKLOAD_IDENTITY_KUBERNETES` | `1`: for every exchange, mint a token for the pod's own service account through the TokenRequest API, valid 30 minutes |
| `LUX_WORKLOAD_IDENTITY_AUDIENCE` | the audience a minted token names; default `https://api.anthropic.com` |

The first two are exclusive; with neither the gateway has no workload
identity. The Kubernetes source reads the pod's mounted service
account token, namespace and CA, takes the service account's name from
the token's `sub`, and needs a Role that lets that service account
`create` `serviceaccounts/token` on itself.

A token minted for every exchange carries a new `jti`, so Anthropic's
single-use check holds across restarts, retries and several federated
Providers. A file read again carries whatever the file holds: the
operator keeps it fresh well within the access token's lifetime, or
turns the issuer's `jti` check off.

The audience is fixed by the operator so that a Provider, which an
administrator of the gateway writes, can never make the gateway mint a
token for another audience, such as the Kubernetes API server's, and
send it to a base URL of its choosing.

### The exchange

`POST <baseURL>/oauth/token`, through the Provider's own client (spec
005's pinned client), as JSON:

```json
{"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer",
 "assertion": "<identity token>",
 "federation_rule_id": "fdrl_...", "organization_id": "...",
 "service_account_id": "svac_...", "workspace_id": "wrkspc_..."}
```

A 200 with an `access_token` and a positive `expires_in` is kept in
memory for the Provider, never in the store, keyed by the Provider's id
and the identifiers and base URL it was exchanged for, so a changed
Provider exchanges again. Anything else is an exchange failure naming
the status and the upstream's error message, never the assertion.

The token is reused until 120 seconds before it expires. From then
until 30 seconds before, one request exchanges again while the others
keep the cached token, and a failed exchange keeps it; from 30 seconds
before, a request waits for an exchange and fails with it.

### Using the token

The doors, discovery and the health probe get a federated Provider's
token where they open any Provider's value: a credential source that
reads the Provider (`gateway.ProviderCredentialSource`) is asked with
the Provider itself, and the gateway's own source answers a federated
Provider from the exchange and any other from the stored value. A
source that does not read the Provider is asked by id as before.

An upstream 401 to a request that carried an exchanged token drops
that token, so the next request exchanges again. A failed exchange is
the request's `provider_unavailable`, `Provider <name>: credential:
<reason>`, and the health probe's failure, so `status.health.lastError`
names it.

## Not in this spec

- Any other vendor's federation. The block names Anthropic's fields.
- A metric for exchanges. A failure shows in the Provider's health.
- Refreshing the identity file. The operator's projection does.

## Acceptance criteria

1. The manifest admits `federation` on an anthropic Provider, renders
   it with `Authorization` and `bearer`, and refuses it with a value,
   on another dialect, with a malformed identifier, with another header
   or scheme, and where the gateway has no workload identity (the
   golden corpus, `TestFederationNeedsWorkloadIdentity`).
2. An apply that sets `federation` deletes the stored value and leaves
   `status.credential.set` false at the old version; switching back
   seals the next version (`TestFederationDropsTheStoredValue`).
3. The exchange posts the grant with the Provider's identifiers and a
   fresh identity token, keeps the token until 120 seconds before it
   expires, refreshes in the window with one exchange while others keep
   it, keeps it when a refresh fails, fails past 30 seconds before, and
   exchanges again for a changed Provider (`TestExchange`).
4. The Kubernetes source mints a token for the pod's own service
   account with the configured audience and a 30-minute lifetime, and
   the file source reads the file again each time
   (`TestKubernetesIdentity`, `TestFileIdentity`).
5. A request through a door to a federated Provider carries
   `Authorization: Bearer <token>` and no `x-api-key`; a 401 drops the
   token and the next request exchanges again; a failed exchange is
   `provider_unavailable` naming it (`TestFederatedForward`).
6. Discovery lists a federated Provider's models with its token
   (`TestDiscoveryWithFederation`).
7. The variables load as the table says, and both sources at once is a
   start-up problem (`TestWorkloadIdentityConfig`).
