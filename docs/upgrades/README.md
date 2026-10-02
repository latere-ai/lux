# Upgrades

How a release of `luxd` follows the one before it, what a version number
promises, and what to read before crossing a major.

## What a version number promises

Lux uses semantic versioning on the release tag. From `v1.0.0` the table
below binds, and the release pipeline checks every tag against it: a
release whose changes need a larger bump than its tag carries is refused.
Before `v1.0.0` a minor release may break any row, and its CHANGELOG
entry names the break.

| Surface | A patch may | A minor may | A major may |
|---|---|---|---|
| the manifest schema | fix a rule that was refusing a valid manifest | add an optional field whose default keeps the old behavior, or an enum value | move to a new API group, `lux.latere.ai/v2`, served beside the old one for a period with a conversion in both directions |
| `/v1` routes | fix a status or a message that was wrong | add a route, a parameter, or a response field | remove or repurpose one |
| error codes | nothing | add a code | remove a code or change what one means |
| `LUX_*` variables | fix a default that was wrong | add a variable, or widen what one accepts | remove a variable or change its meaning; the first release of the major reads a removed variable, ignores it, and logs one warning naming the replacement |
| event types and payloads | nothing | add a type or a `data` member | remove a type or a member |
| the usage record | nothing | add a field | remove or repurpose a field |
| the exported Go packages | nothing | add a function, a type, or a field | break a call site |
| the store schema | nothing | add a migration an older binary of the same major can still read | add one it cannot |

A cost computed from one pricing is computed the same way by every later
release, so a bill does not change under an upgrade.

## Inside a major: no step

Any release upgrades from any earlier release in the same major with no
step. Apply the next release's deploy archive, or change the image
reference in your own kustomization, and roll. The store's migrations
are embedded in the binary and applied at start; the Deployment rolls
one replica at a time with none surging, so during a roll the two
versions serve together. A minor's migrations are readable by the
previous minor's binary, which is what makes a rollback inside a minor
series `kubectl rollout undo` and nothing else: the older binary finds a
newer schema of its own major, warns, and serves.

## When permanent Key fences are in use

Upgrade every writable replica before installing a Key fence. The new table is
readable alongside older schema tables, but an older binary does not enforce its
records. Once fences exist, rolling back to a writer without fence support would
reopen credential mutations and is unsupported. Continue serving with a
fence-capable binary; this feature changes the usual rollback guarantee.

## What a rollback turns off

An older binary leaves in the store what a newer one wrote and ignores
what it does not know: a manifest field, a table, or a `LUX_*` variable. A rollback
below the release that brought a feature therefore turns that feature off
while the data stays in place, and rolling forward again turns it back
on.

- Below `v0.8.0`, a Model with `spec.disabled: true` is served again on
  every door and listed again; delete it before rolling back if it must
  stay out of service.
- Below `v0.8.0`, `GET /v1/usage` reads the hourly rows alone, so the
  months `LUX_USAGE_RETENTION` has folded into monthly rows are missing
  from its totals, and with `LUX_REQUESTLOG_PARTITION_LABEL` set,
  `GET /v1/requests` does not find the archived records written under a
  partition.
- Below `v0.9.0`, `LUX_BASE_PATH_MODE=replace` is ignored and the control
  plane answers at `<base>/v1` again; a `lux` command of `v0.9.0` or newer
  follows it through `/.well-known/lux`, and a client that wrote the
  `replace` addresses down does not.

## Across a major: one document

A change that needs a drop, a rename, or a retype is the first
migration of a new major, and a binary that finds a schema of another
major refuses to start and names both. Each major gets a document here,
`docs/upgrades/<major>.md`, saying what to verify before, in what order
to move, and what cannot be undone. No major has been cut yet, so there
is no such document; `v1` gets `v1.md` when it is cut.

## A key rotation is not an upgrade

Rotating `LUX_SECRETS_KEK` has its own sequence, at any version: deploy
with `LUX_SECRETS_KEK=new,old`, run `luxd rewrap` once against the
store, deploy with `LUX_SECRETS_KEK=new`. Every stored credential is
re-wrapped under the new key without a value being decrypted, and the
run is idempotent, so an interrupted rotation is repeated rather than
repaired. `luxd check`'s `credentials` line says whether every stored
row opens under the keys a deployment carries.
