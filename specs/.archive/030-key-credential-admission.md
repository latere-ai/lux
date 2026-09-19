---
title: Admit supplied Key credentials by commitment
status: complete
track: core
depends_on:
  - 025-mutation-authorization.md
affects:
  - authorizer/
  - internal/api/
  - docs/
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Admit supplied Key credentials by commitment

## Overview

A provisioning authorizer cannot distinguish its registered supplied Key hash
from another hash or a request to mint a new credential. The sanitized policy
is identical in all three cases.

## Design

Key proposals add a top-level `credential` descriptor. Its `mode` is `absent`,
`inline`, `sha256`, `reference`, or `conflict` when multiple inputs are present.
For `sha256` only, `commitment` is lowercase hex SHA-256 of the exact UTF-8 bytes
of the supplied verifier string. Neither the credential nor its original verifier
is included. Invalid verifier syntax remains subject to normal manifest validation.

`absent` means no credential input: create normally generates a value, while
update retains the existing value. Rotation remains distinguished by its existing
operation marker. The descriptor is identical in the main mutation, owner
assignment and all reference bindings. It is additive and changes no action names,
manifest shape, storage format or default owner-policy behavior.

## Acceptance criteria

- An authorizer can allow only the registered supplied hash and refuse a different
  hash, inline secret, reference, omitted input or conflicting inputs.
- Updating without credential input remains distinguishable from supplying a hash.
- Secret and verifier canaries remain absent from authorization requests.
- HTTP integration verifies creation, owner assignment and reference checks use
  the same descriptor, and refusal prevents an installed credential.

## Outcome

Key proposals now carry the input mode and a commitment only for a sole supplied
verifier. Invalid combinations are explicit conflicts. No raw credential or
original verifier is sent. Mutation, ownership and reference requests share the
descriptor; updates with omitted credential input remain distinguishable.

The registered-credential HTTP regression failed before the change and now
passes. A real Lux process admits the registered hash, serves inference with
its credential, and refuses replacement hashes, inline values and omitted input.
Authorizer and API race coverage are 96.4% and 94.3%. Tagged lint and vet pass.
