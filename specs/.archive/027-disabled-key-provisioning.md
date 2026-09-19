---
title: Provision disabled keys before model access is assigned
status: complete
track: core
depends_on:
  - 003-manifest-contract.md
  - 007-keys-and-limits.md
affects:
  - manifest/
  - internal/api/
  - test/e2e/
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Provision disabled keys before model access is assigned

## Overview

A platform credential may exist before its principal has any model entitlement.
It must be representable as a disabled Key with no selectors, without granting
a wildcard or inventing a model name that could become reachable later.

## Design

Permit an empty spec.models only when spec.disabled is true. All other validation,
reference checks, limits and expiry ceilings still apply. Enabling the Key requires
at least one valid selector in the same update. The existing disabled data-plane
refusal and hash lifecycle remain unchanged.

## Acceptance criteria

- A disabled Key with no selectors can be created and updated, including by hash.
- Its credential cannot invoke any model, including models added later.
- Enabling without selectors fails; enabling with authorized selectors succeeds.
- Active-key validation and ordinary disabled keys retain existing behavior.
- A real luxd process proves the creation, refusal and activation lifecycle.

## Outcome

Implemented on 2026-09-19. Empty selectors are accepted only for disabled Keys;
all other manifest validation and ceilings remain unchanged. API tests cover
minted and supplied-hash credentials, updates, refused empty activation and
successful explicit activation. The real-process e2e verifies the credential
refuses a subsequently added Model until it is explicitly assigned. The API
regression fails before the validation change. Full tests, lint, vet and build
pass. No deviations or deferred criteria.
