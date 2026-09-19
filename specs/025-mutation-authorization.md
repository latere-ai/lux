---
title: Authorize proposed mutations and explicit owner assignment
status: in-progress
track: core
depends_on:
  - 006-identity.md
  - 011-api.md
  - 022-authorizer-vocabulary-package.md
affects:
  - authorizer/
  - internal/api/
  - internal/auth/
  - docs/plane.md
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Authorize proposed mutations and explicit owner assignment

## Overview

An external control plane must inspect the proposed policy before accepting an
edit and may need to create an object for another principal. Authenticating as
a service must not make that service the permanent owner of every object it
manages, or require impersonating the intended owner.

## Design

Every create and update authorization resource gains `proposed`: the requested
owner, metadata, and sanitized spec. Existing update resource fields continue
to describe the stored object, preserving current authorizers' meaning. The
proposal excludes status, provider credential values, provider header values,
Key values, and supplied hashes. Header names and credential presence may be
reported without their values. The owner policy also checks the proposed
tunnel mode so a non-admin cannot turn a tunnelled Provider into a remote one.
The shared client fingerprints the complete serialized decision input, so a
previous allow cannot authorize a different proposal or changed claims.

`Lux-Owner` is an optional header on a create PUT. It names a rendered subject
and requires an additional `owner.assign` decision with resource kind
`Ownership`, target kind, name, requested owner and sanitized proposal. The
ordinary create decision remains required and supplies all ceilings. The
assignment check never replaces the authenticated caller, audit subject,
reference-check identity, or rate-limit memo. Without the header the caller
owns the object. An old authorizer that does not know the action fails closed.
The built-in owner policy permits assignment only for configured admins.

An update may omit `Lux-Owner` or repeat the current owner; a different owner
is refused. Ownership remains immutable. The stored status is authoritative;
owner assignment is a request concern, so reusable manifests gain no owner
field and file-mode loading is unchanged. Key quotas count the assigned owner.

## Acceptance criteria

- A warmed update decision cannot authorize changed tenancy or policy.
- Proposals contain the requested limits, selectors and switches, but no secret,
  supplied hash, status value or provider header value.
- Owner assignment requires both ordinary create and owner.assign authorization.
- Assigned objects record the target owner and the original audit actor.
- A target-owner Key quota cannot be bypassed by changing the writer identity.
- Existing owner cannot be changed, even by an administrator.
- Ordinary creates retain their existing owner and legacy authorization behavior.
- A non-admin tunnel owner cannot change it into a remote Provider.

## Not in this spec

Object transfers, tenant policy, identity impersonation, or deployment grants.
