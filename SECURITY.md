# Security

## Reporting a vulnerability

Report one to security@latere.ai. Do not open a public issue for a
vulnerability. You will hear back within three business days, and a fix
for a high severity issue ships within thirty days. Credit in the
release notes on request.

Fixes go to the two most recent minor release series. There is no
release yet; the first one is `v0.1.0`.

## What the design commits to

What Lux protects, against whom, and how each threat is answered is
written down in the [threat model](specs/016-security-and-threat-model.md),
one row per threat naming the mechanism, the spec that owns it, and the
test that proves it. So a reviewer checks the design rather than taking
it on faith, and a row whose test is not in the tree fails the build
unless it says which spec owes that test.

- A Key never works on the control plane, and an issuer token never
  opens a door.
- A provider credential never leaves the gateway: it is sealed in the
  store, opened in memory for one request, and sent only toward that
  Provider's own base URL.
- A decision the authorizer cannot give is a refusal, never an allow.
- An object a subject may not see is not found, whether it exists or
  not.
- No prompt, completion, credential, or Key value reaches a usage
  record, an event, a log line, a metric, or a span.
- No route on either plane serves HTML, and no page in a browser can
  call either plane cross-origin.
- A Provider base URL that names a private address, a single label, or
  the gateway itself is refused at resolve and again at dial.
- An installation with no authorizer and no listed administrator
  declares no upstream, so a fresh one holds no credential to take.

Not every property is proven yet. The State column of the threat table
says which rows the tree carries and which wait on the spec that owns
them, and the properties above are each a row there.

## The supply chain

Dependencies are checked for known vulnerabilities on every push, and a
new one is a reviewed row in the gate's allow list. A release will carry
an SPDX bill of materials for the module graph and one per image, cosign
signatures over the images and the checksums, and a build provenance
attestation per image, so `gh attestation verify` answers for the image
you are about to run.
