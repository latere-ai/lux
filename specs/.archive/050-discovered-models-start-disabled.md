---
title: "Discovered Models start disabled: a Provider may ask that every Model discovery creates for it start disabled, so none is served until an operator declares it"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/005-providers.md
  - specs/.archive/039-disabled-models.md
affects: [manifest/v1/, manifest/, internal/serve/discovery.go, api/openapi.yaml, specs/003-manifest-contract.md, specs/005-providers.md]
effort: small
created: 2026-10-23
updated: 2026-10-24
author: changkun
---

# Discovered Models start disabled

## Overview

Discovery ([[005-providers]]) lists a Provider's upstream models and
declares each as a discovered Model, served at once. An operator who
charges for calls, or who reviews what callers may reach, has no way to
connect a Provider without every model it lists becoming callable the
moment the list lands: a discovered Model carries no pricing, and a
Model a caller can name before anyone priced or reviewed it is refused
or, worse, served.

A Provider gains `spec.discovery.startDisabled`. When true, every Model
discovery creates for the Provider is created with `spec.disabled`
([039-disabled-models](039-disabled-models.md)), so no door
serves it and no model list carries it. An operator serves one by
declaring a Model of the same name, which shadows the discovered one
in place as it does today.

## Current state

- A discovered Model is created with `spec.disabled` false. An update
  by discovery keeps whatever `spec.disabled` the stored Model has, and
  the API never writes a discovered Model: an apply of its name
  declares a Model that replaces it in place, keeping its id.
- `spec.disabled` is documented as written by an apply alone, never by
  discovery or health ([[003-manifest-contract]]).

## Design

| Field | Type | Default | Mutable | Meaning |
|---|---|---|---|---|
| `discovery.startDisabled` | bool | `false` | yes | every Model discovery creates for this Provider is created with `spec.disabled` true; a Model discovery already created keeps its own `spec.disabled` |

- The flag is read when discovery creates a Model, and only then. A
  Model discovery updates keeps its stored `spec.disabled`, as today,
  so turning the flag on or off changes no Model that exists.
- Deleting a declared Model that shadowed a discovered one lets the
  next run restore the discovered one, which is a create: it starts
  disabled when the flag is on.
- The flag is rendered only when true, as `spec.disabled` is.
- [[003-manifest-contract]]'s row for `spec.disabled` changes to:
  written by an apply, and by discovery on a create under
  `discovery.startDisabled`, never by health.

## Not in this spec

- Enabling a discovered Model in place. A discovered Model stays
  unwritable through the API; the operator declares one over it.
- Pricing a discovered Model from any list. Pricing is the declaring
  operator's.
- Any change to what a disabled Model does at call time or in a list
  ([039-disabled-models](039-disabled-models.md)).

## Acceptance criteria

- A Provider with `discovery.startDisabled: true` whose upstream lists
  two models has two discovered Models with `spec.disabled` true after
  one tick; a disabled Model is refused at call time as
  [039-disabled-models](039-disabled-models.md) proves
  (`TestDiscoveryStartsDisabled`).
- Turning the flag off and ticking again leaves both disabled; a model
  the upstream adds after that is created enabled
  (`TestDiscoveryStartsDisabled`).
- A declared Model applied over a disabled discovered one is served at
  its own `spec.disabled`, and deleting it restores a discovered Model
  that starts disabled while the flag is on
  (`TestDiscoveryStartsDisabled`).
- The manifest round-trips the flag, renders it only when true, and the
  OpenAPI document names it (`TestProviderStartDisabledRoundTrip`, the
  openapi drift check).

## Outcome

Built as designed in v0.19.0 and live in production since v0.19.1 on
2026-10-24.

| # | Test |
|---|---|
| 1 | `TestDiscoveryStartsDisabled`, the first tick |
| 2 | `TestDiscoveryStartsDisabled`, the flag turned off and a model added |
| 3 | `TestDiscoveryStartsDisabled`, the declared Model and its deletion |
| 4 | `TestProviderStartDisabledRoundTrip` and the OpenAPI drift check |

Notes on the build:

- Declaring a Model over a discovered one through the API, which
  criterion 3 relies on, was refused `already_exists` until v0.19.1:
  the API left `status.source` empty on the Model it wrote, so the
  store did not take it as declared. The store's own test covered the
  replacement and missed the API path. `TestDeclaringOverADiscoveredModel`
  now covers it.
- In production the platform declared eleven Anthropic models over
  their discovered Models, each enabled and priced, and a call to one
  of them answered.

