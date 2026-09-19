# Upgrades

How a release of `luxd` follows the one before it, and what to read
before crossing a major.

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

What a version number promises is the table in the
[release and installation spec](../../specs/.archive/017-release-and-installation.md):
before `v1.0.0` a minor may break a row with a CHANGELOG entry naming
the break; from `v1.0.0` the table binds, and the release pipeline
checks every tag against it.

## Across a major: one document

A change that needs a drop, a rename, or a retype is the first
migration of a new major, and a binary that finds a schema of another
major refuses to start naming both. Each major that has ever been cut
gets a document here, `docs/upgrades/<major>.md`, saying what to
verify before, in what order to move, and what cannot be undone. There
is no such document yet: nothing has been released, and the first
release is `v0.1.0`. The first major, `v1`, gets `v1.md` when it is
cut, and every major after it the same.

## A key rotation is not an upgrade

Rotating `LUX_SECRETS_KEK` has its own sequence, at any version: deploy
with `LUX_SECRETS_KEK=new,old`, run `luxd rewrap` once against the
store, deploy with `LUX_SECRETS_KEK=new`. Every stored credential is
re-wrapped under the new key without a value being decrypted, and the
run is idempotent, so an interrupted rotation is repeated rather than
repaired. The [providers spec](../../specs/005-providers.md) has the
rule; `luxd check`'s `credentials` line says whether every stored row
opens under the keys a deployment carries.
