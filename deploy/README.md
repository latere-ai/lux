# Deploy manifests

Kustomize manifests for an installation of `luxd`, and the example
manifests a first gateway is fed. [`docs/install.md`](../docs/install.md)
walks an installation from nothing to a request through a door, and CI
runs that document against a kind cluster on every push, so what it says
works.

| Directory | Holds |
|---|---|
| `base/` | the Deployment, hardened as the [threat model](../specs/016-security-and-threat-model.md) says; its Service; a ServiceAccount with no token; the NetworkPolicy; the PodDisruptionBudget; the PrometheusRule of the [observability spec](../specs/019-observability.md). No namespace and no image registry: an overlay names the namespace, and the image `luxd` is pointed at a real reference by the release pipeline, which pins the tag's digest into the base of the deploy archive |
| `components/hpa/` | a HorizontalPodAutoscaler an overlay adds with `components:`; not in the base, because each replica opens `LUX_DB_MAX_CONNS` store connections and the count is a decision against the cluster's ceiling |
| `overlays/kind/` | a laptop cluster: one replica, the memory store, the public port on NodePort 30080, applied with `kubectl apply -k` once `luxd.env` names your issuer |
| `overlays/generic/` | any cluster: two replicas over a Postgres store named in the bootstrap Secret, behind an ingress you add |
| `bootstrap/` | the Secret template for the four values no manifest in a repository should carry: `LUX_SECRETS_KEK`, `LUX_AUTHORIZER_TOKEN`, `LUX_EVENTS_SECRET`, `LUX_DB_URL`; filled once, applied by hand |
| `examples/` | one Provider per dialect, one Model each, a Budget, and a Key: what `make run` applies to a gateway on a laptop and what an operator edits first once the gateway serves. They point at the stub providers of the test binary and are not part of an installation |

The configuration is a ConfigMap named `luxd` that each overlay
generates from its `luxd.env`; every `LUX_*` variable is in the table of
the [repository scaffold spec](../specs/002-repository-scaffold.md).
The Deployment rolls one replica at a time with none surging, so a
rollout never asks the store for more connections than the running
replicas already hold; the reasoning and the version promise are in the
[release and installation spec](../specs/017-release-and-installation.md).
