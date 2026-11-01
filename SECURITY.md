# Security

## Reporting a vulnerability

Report one to security@latere.ai. Do not open a public issue for a
vulnerability. You will hear back within three business days, and a fix
for a high severity issue ships within thirty days. Credit in the
release notes on request.

Fixes go to the two most recent minor release series. The
[releases page](https://github.com/latere-ai/lux/releases) lists them.

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

Each property above is a row of the threat table, which names the test
that proves it.

## The supply chain

Dependencies are checked for known vulnerabilities on every push, and a
new one is a reviewed row in the gate's allow list. A release is built
from a tagged commit whose verify run passed, through the shared release
stages in [latere-ai/ci](https://github.com/latere-ai/ci). Every release
carries an SPDX bill of materials per image (`sbom-lux.spdx.json`,
`sbom-lux-stubs.spdx.json`), cosign signatures over both images and over
`checksums.txt`, which lists every other file the release attaches, and
an SBOM attestation and a build provenance attestation per image.
Nothing is signed with a key anyone holds: the images are signed by the
build stage's identity and `checksums.txt` by the publish stage's, each
run for this repository.

## Verifying a release

Set the tag you are about to run and the account that published it, then
check the release's files and the images. The release workflow runs the
same checks against every release it publishes.

```sh
TAG=v0.20.0
OWNER=latere-ai   # a fork's release: the fork's owner
ISSUER=https://token.actions.githubusercontent.com

# The files: checksums.txt is signed by the publish stage run for this
# repository, and lists every other file's SHA-256.
gh release download "$TAG" -R "$OWNER/lux" -D lux-release
cd lux-release
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer "$ISSUER" \
  --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-publish\.yml@' \
  --certificate-github-workflow-repository "$OWNER/lux" checksums.txt
sha256sum -c checksums.txt

# The images: the signature by the build stage run for this repository,
# then the provenance and SBOM attestations.
for image in lux lux-stubs; do
  cosign verify "ghcr.io/$OWNER/$image:$TAG" \
    --certificate-oidc-issuer "$ISSUER" \
    --certificate-identity-regexp '^https://github\.com/latere-ai/ci/\.github/workflows/image-build\.yml@' \
    --certificate-github-workflow-repository "$OWNER/lux"
  gh attestation verify "oci://ghcr.io/$OWNER/$image:$TAG" -R "$OWNER/lux" \
    --signer-workflow latere-ai/ci/.github/workflows/image-build.yml
done
```

Releases up to v0.20.0 were signed by this repository's own release
workflow, and their `checksums.txt` lists the archives alone. Check one
of those with `TAG`, `OWNER`, and `ISSUER` set as above, against that
workflow's identity:

```sh
IDENTITY="https://github.com/$OWNER/lux/.github/workflows/release.yml@refs/tags/$TAG"

gh release download "$TAG" -R "$OWNER/lux" -p 'checksums.txt*' -p '*.tar.gz'
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" checksums.txt
sha256sum -c checksums.txt

for image in lux lux-stubs; do
  cosign verify --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
    "ghcr.io/$OWNER/$image:$TAG"
  gh attestation verify "oci://ghcr.io/$OWNER/$image:$TAG" --repo "$OWNER/lux"
done
```

Those releases also carry `sbom-module.spdx.json`, a bill of materials
for the whole module graph, and name the gateway image's bill of
materials `sbom-luxd.spdx.json`.
