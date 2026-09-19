---
title: Object-scoped model discovery and list authorization
status: in-progress
track: core
depends_on:
  - 006-identity.md
  - 011-api.md
  - 025-mutation-authorization.md
affects:
  - authorizer/
  - manifest/v1/
  - internal/auth/
  - internal/api/
  - internal/serve/
  - internal/config/
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Object-scoped model discovery and list authorization

## Overview

An owner can hold objects for several tenants. A control plane cannot infer
object tenancy from owner membership, and a funded catalogue cannot be listed
safely by granting access to every object of its provisioning service.

## Design

Model references in model.use include labels, copied from each actual Model.
Discovered Models inherit Provider labels as well as ownership. Provider label
changes trigger discovery, refreshing inherited labels on discovered Models.

Optional LUX_AUTHORIZE_LIST_ITEMS (default false) intersects the list decision
and query filters with a read decision on every candidate object. Denials skip
objects; authorizer errors fail the entire list without partial results. File
mode remains read-only and needs no authorizer. The list decision alone sets
request rate ceilings; candidate decisions never replace that request memo.

Filtered pagination fetches only the remaining visible capacity. This fixes
an existing overflow bug which advances past permitted objects in a partially
filled page. A nonempty cursor therefore ends on a returned object; a cursor
never names the final denied candidate of a page.

## Acceptance criteria

- model.use reports each Model's labels without aliasing the source maps.
- Discovery copies labels and refreshes them after Provider label changes.
- Opt-in lists omit denied objects, preserve query/filter intersection, and
  fail closed on an unavailable per-object decision.
- Multiple pages return every permitted object exactly once, including when
  denied candidates surround them or all remaining candidates are denied.
- The existing multi-owner pagination overflow has a failing-before regression.
- Default list behavior and file-mode behavior remain compatible.
- Configuration and public contracts document the additional calls and fields.
