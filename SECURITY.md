# Security

## Reporting a vulnerability

Report one privately, through
[GitHub's private vulnerability reporting](https://github.com/latere-ai/lux/security/advisories/new)
or by email to security@latere.ai. Do not open a public issue for it. The
[organization's security policy](https://github.com/latere-ai/.github/blob/main/SECURITY.md)
sets the response times, how disclosure works, and the safe harbor for
research done in good faith. Credit in the release notes on request.

Fixes go to the two most recent minor release series, which the
[releases page](https://github.com/latere-ai/lux/releases) lists.

## What is protected

What Lux protects, against whom, and how each threat is answered is
written down in the [threat model](specs/016-security-and-threat-model.md),
one row per threat naming the mechanism and the test that proves it. So
a reviewer checks the design rather than taking it on faith, and a row
whose test is not in the tree fails the build unless it says which spec
owes that test. [`docs/security.md`](docs/security.md) is the same
ground for an operator: what the gateway sees, how long it keeps it, and
what running it safely takes.

### What the design commits to

- A Key never works on the control plane, and an issuer token never
  opens a door.
- A provider credential never leaves the gateway: it is sealed in the
  store, opened in memory for one request, and sent only toward that
  Provider's own base URL.
- A decision the authorizer cannot give is a refusal, never an allow.
- A reference in a manifest to an object its subject may not see is
  not found, whether the object exists or not.
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

### The process

The released image is `gcr.io/distroless/static-debian12:nonroot` with
`luxd` and nothing else to run: no shell and no package manager. The
manifests in [`deploy/base`](deploy/base) run it as a non-root user with
a read-only root file system, every capability dropped, privilege
escalation off, the `RuntimeDefault` seccomp profile, and no service
account token mounted.

## What is not defended

- **A compromised provider.** Whatever an upstream does with a prompt it
  was sent happens outside the gateway. A translated response is decoded
  and encoded again rather than relayed, and that is the extent of it.
- **A caller that holds a Key.** A Key exists to call models. What its
  holder does with an answer is theirs, and no gateway can tell a useful
  answer from a stolen one. Its limits, its Budget, and its expiry bound
  what it costs.
- **The operator.** Whoever can read the store, the process environment,
  or the process memory holds the key-encryption key, the authorizer
  token, and the events secret, and with them every provider credential.
- **The issuer, the authorizer, and the event sink.** Each is an
  endpoint the operator writes or runs, and the gateway trusts what it
  says. An authorizer that does not answer is a refusal, never an allow.
- **Traffic analysis.** An observer of the gateway's outbound traffic
  learns which providers an installation uses and how often. TLS hides
  the bodies, not the destinations.
- **Isolation of whatever runs beside it.** The gateway is a network
  service, not a sandbox, and gives no kernel or hypervisor boundary to
  the processes it shares a machine with.

## Audit

Lux has had no external security audit. The evidence for every control
above is the threat model and the tests its rows name, both in this
repository.

## Supply chain

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

### Verifying a release

Set the tag you are about to run and the account that published it, then
check the release's files and the images. The release workflow runs the
same checks against every release it publishes.

```sh
TAG=v0.22.0
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
