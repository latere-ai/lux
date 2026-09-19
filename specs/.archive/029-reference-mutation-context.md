---
title: Bind reference authorization to the proposed mutation
status: complete
track: core
depends_on:
  - 006-identity.md
affects:
  - internal/auth/
  - internal/api/
  - docs/
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Bind reference authorization to the proposed mutation

## Overview

A policy must bind a Key's chosen models to that same Key's Budget and grant.
Independent permissions for a Model and Budget do not establish permission to
combine them. Credential rotation also needs the unchanged desired state so a
policy can admit it without accepting an incomplete update envelope.

## Design

Reference decisions during apply carry `binding: {kind, proposed}`. The proposal
is the same sanitized owner, metadata and spec sent for the enclosing mutation.
The authenticated writer remains the caller. Direct reads carry no binding.
No credential values, supplied key hashes or provider header values are sent.

Key rotation includes the sanitized existing Key as `proposed` and
`operation: rotate` on key.update. The stored owner and policy cannot change.

## Acceptance criteria

- A model.use denial based on the enclosing Key's grant refuses the apply.
- Provider and Budget reference decisions carry the same mutation binding.
- A policy requiring proposed state can authorize rotation; its deny prevents it.
- Reference and rotation envelopes contain no credential values or hashes.
- Existing default owner-policy behavior and direct-read envelopes are preserved.

## Outcome

Reference lookups now carry the sanitized enclosing mutation without changing
the authenticated writer. Rotation carries the unchanged proposed state and an
explicit operation marker. Regressions failed without these fields, then passed.
The real-process integration test creates a grant-bound Key, refuses an unbound
one, performs inference, rotates the credential and performs inference again.
API and auth race coverage is 94.3% and 95.8%; lint and package suites pass.
