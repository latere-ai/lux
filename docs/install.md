# Install

From nothing to a request through a door: a cluster, the four secrets,
the gateway, its first check, a Provider and a Model, a Key, and one
`curl`. Every command below is in a fenced block, and CI runs the blocks
in order against a fresh kind cluster on every push and, after every
release, against the published image and archive, so a command that
stopped working fails a build rather than you.

The walk uses a [kind](https://kind.sigs.k8s.io/) cluster on your
machine with the memory store: one replica, nothing kept across a
restart, which is the right first installation and the wrong second one.
The [deploy manifests](../deploy/README.md) hold the shape of a real one
beside it, `deploy/overlays/generic`, two replicas over a Postgres store
behind an ingress you add.

## What you need

`kubectl`, `kind`, `curl`, and a container runtime `kind` can use. From
the release page of the version you are installing: the `lux` command
for your platform, `lux_<tag>_<os>_<arch>.tar.gz`, unpacked onto your
`PATH`; the deploy archive, `deploy-<tag>.tar.gz`, unpacked into the
directory you work in, which gives you `deploy/`; and the image
reference, `ghcr.io/<owner>/lux:<tag>`, where `<owner>` is the account
the release was published under. Every release is signed, and
[`SECURITY.md`](../SECURITY.md) says how to verify one before you run it.

You also need an OpenID Connect issuer whose tokens carry the audience
`lux`, a token from it, and one model provider's API key. The gateway
verifies identity and issues none of its own, so the issuer is yours: a
company login, or a small one you run.

## Inputs

The blocks read these and nothing else about your environment. Set the
ones without a default before you start.

```sh
# The release you are installing: its image, and where you unpacked its
# deploy archive, a path relative to this directory.
export LUX_INSTALL_IMAGE="${LUX_INSTALL_IMAGE:?set LUX_INSTALL_IMAGE to the release's image, ghcr.io/<owner>/lux:<tag>}"
export LUX_INSTALL_MANIFESTS="${LUX_INSTALL_MANIFESTS:-deploy}"
# Your issuer, a token from it, and the subject that token renders to,
# <issuer>|<sub>, which the gateway lets declare Providers and Models.
export LUX_INSTALL_ISSUER="${LUX_INSTALL_ISSUER:?set LUX_INSTALL_ISSUER to your OpenID Connect issuer's URL}"
export LUX_INSTALL_TOKEN="${LUX_INSTALL_TOKEN:?set LUX_INSTALL_TOKEN to a token from that issuer with the audience lux}"
export LUX_INSTALL_ADMIN="${LUX_INSTALL_ADMIN:?set LUX_INSTALL_ADMIN to the token's subject, <issuer>|<sub>}"
# The first provider: its base URL, its API key, and a model it serves.
export LUX_INSTALL_UPSTREAM="${LUX_INSTALL_UPSTREAM:-https://api.openai.com/v1}"
export LUX_INSTALL_UPSTREAM_KEY="${LUX_INSTALL_UPSTREAM_KEY:?set LUX_INSTALL_UPSTREAM_KEY to the provider's API key}"
export LUX_INSTALL_MODEL="${LUX_INSTALL_MODEL:-gpt-4o-mini}"
command -v lux >/dev/null || { echo "lux is not on PATH; unpack lux_<tag>_<os>_<arch>.tar.gz from the release" >&2; exit 1; }
```

## A cluster

The kind cluster maps node port 30080 to `localhost:30080`, where the
gateway's public port lands. If you already have a cluster named `lux`,
the second block leaves it alone.

```yaml file=kind-lux.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: lux
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30080
        hostPort: 30080
```

```sh
kind get clusters 2>/dev/null | grep -qx lux || kind create cluster --config kind-lux.yaml --wait 120s
kubectl config use-context kind-lux
kubectl create namespace lux --dry-run=client -o yaml | kubectl apply -f -
```

## The four secrets

Four values no manifest in a repository should carry live in one Secret
you apply by hand: the key that encrypts every provider credential at
rest, the bearer for your authorizer, the signing key for events, and
the store's URL. A laptop cluster with the memory store, the built-in
owner policy, and no sink needs only the first; a blank value is unset.
[`deploy/bootstrap/luxd-secrets.yaml`](../deploy/bootstrap/luxd-secrets.yaml)
is the same Secret as a file, for when you keep manifests in a
repository.

```sh
kubectl -n lux create secret generic luxd-secrets \
  --from-literal=LUX_SECRETS_KEK="$(head -c 32 /dev/urandom | base64 | tr -d '\n')" \
  --from-literal=LUX_AUTHORIZER_TOKEN= \
  --from-literal=LUX_EVENTS_SECRET= \
  --from-literal=LUX_DB_URL= \
  --dry-run=client -o yaml | kubectl apply -f -
```

Keep the key. A gateway restarted without it cannot open the credentials
it stored, and rotating it is a documented sequence, not a re-install:
deploy with `LUX_SECRETS_KEK=new,old`, run `luxd rewrap`, deploy with
`LUX_SECRETS_KEK=new`.

## The gateway

The kind overlay in the deploy archive is one replica on the memory
store behind the NodePort, without the alert rules, since a laptop
cluster runs no Prometheus Operator to read them. Your own kustomization
over it names your issuer, the subject that may declare upstreams, and
the image, and is what you apply, now and after every edit. A plain `http://` issuer
inside the cluster is a lab's and is listed as insecure so the gateway
accepts it; a real issuer speaks TLS and the second line is empty.

```sh
mkdir -p lux-install
insecure=""
case "$LUX_INSTALL_ISSUER" in http://*) insecure="$LUX_INSTALL_ISSUER" ;; esac
cat > lux-install/kustomization.yaml <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: lux
resources:
  - ../${LUX_INSTALL_MANIFESTS}/overlays/kind
configMapGenerator:
  - name: luxd
    behavior: merge
    literals:
      - LUX_OIDC_ISSUERS=${LUX_INSTALL_ISSUER}
      - LUX_OIDC_INSECURE_ISSUERS=${insecure}
      - LUX_ADMIN_SUBJECTS=${LUX_INSTALL_ADMIN}
patches:
  - target:
      kind: Deployment
      name: luxd
    patch: |-
      - op: replace
        path: /spec/template/spec/containers/0/image
        value: ${LUX_INSTALL_IMAGE}
EOF
kubectl apply -k lux-install
kubectl -n lux rollout status deployment/luxd --timeout=180s
```

The Deployment rolls one replica at a time and never surges, because
each replica opens `LUX_DB_MAX_CONNS` connections to a Postgres store
and a surge would ask for a third replica's worth; on the memory store
that costs nothing and changes nothing. The base's network policy lets
the gateway reach DNS, TLS endpoints, Postgres, and its own replicas and
nothing else; the kind overlay widens that to every destination, since
a laptop's issuer and model runtime listen on ports of their own over
plain HTTP, and kind enforces the policy. A real installation keeps the
base's list and adds the port of any endpoint of its own.

## The first check

`luxd check` prints one line per requirement of the installation and
exits 1 when any one fails. It runs inside the serving replica with the
same configuration, dials the issuer and the providers, and changes
nothing, so it is safe now and safe later when something is wrong. The
`store` line warns that state lives in memory, which is true of this
cluster and is the reason the generic overlay exists.

```sh
kubectl -n lux exec deploy/luxd -- /usr/local/bin/luxd check
```

## A Provider, a Model, and a Key

The `lux` command speaks the gateway's `/v1` API with your token. The
Provider carries your API key once; the gateway seals it, returns it to
no one, and sends it toward that provider's base URL and nowhere else.
The Model is the name callers use, routed to one upstream model. The Key
is what a workload holds: its value prints once and is stored as a hash.

```sh
export LUX_URL=http://localhost:30080
export LUX_TOKEN="$LUX_INSTALL_TOKEN"
for _ in $(seq 1 30); do curl -fsS -o /dev/null "$LUX_URL/readyz" && break; sleep 1; done
lux whoami
lux providers create openai -dialect openai -base-url "$LUX_INSTALL_UPSTREAM" -credential-from-env LUX_INSTALL_UPSTREAM_KEY
lux models create "$LUX_INSTALL_MODEL" -target "openai/$LUX_INSTALL_MODEL"
LUX_KEY=$(lux keys create dev -models "$LUX_INSTALL_MODEL" | sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
[ -n "$LUX_KEY" ] || { echo "the Key's value was not returned" >&2; exit 1; }
export LUX_KEY
lux get key dev
```

## One request through a door

Point any OpenAI SDK at `http://localhost:30080/openai/v1` with the Key
as its API key, or `curl` the door. The gateway probes a new Provider on
its next health tick, at most `LUX_HEALTH_INTERVAL` away, so the loop
below retries until the first answer arrives.

```sh
body="{\"model\":\"$LUX_INSTALL_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in five words.\"}]}"
for _ in $(seq 1 40); do
  if answer=$(curl -sS "$LUX_URL/openai/v1/chat/completions" -H "Authorization: Bearer $LUX_KEY" -H 'Content-Type: application/json' -d "$body") \
     && printf '%s' "$answer" | grep -q '"choices"'; then
    printf '%s\n' "$answer"
    break
  fi
  sleep 2
done
printf '%s' "${answer:-}" | grep -q '"choices"' || { echo "no answer through the door: ${answer:-}" >&2; exit 1; }
lux requests
```

`lux requests` shows the one record that request produced: the key, the
model, the provider, the tokens, the cost when the Model is priced, and
never the content.

## Where to go from here

- Set `LUX_DB_URL` in the Secret and apply `deploy/overlays/generic`
  for two replicas over Postgres; the gateway applies its schema at
  start, and `luxd check`'s `store`, `migrations`, and `db conns` rows
  verify it against the cluster ([`configuration.md`](configuration.md)
  has the `LUX_DB_*` knobs). That
  overlay keeps the alert rules, a `PrometheusRule` the Prometheus
  Operator reads; a cluster without the operator drops it the way the
  kind overlay does.
- Point `LUX_AUTHORIZER_URL` at an endpoint you write, so permission is
  your decision rather than the built-in owner policy; [`security.md`](security.md) is how to write
  it and lock it down.
- Set `LUX_EVENTS_URL` and `LUX_EVENTS_SECRET` to receive one signed
  event per mutation, and `LUX_REQUESTLOG_EXPORTER=s3` to archive every
  request record; [`configuration.md`](configuration.md) has the knobs
  and [`observability.md`](observability.md) the metrics for the sink and
  the archive.
- Every `LUX_*` variable is in the table of the
  [configuration reference](configuration.md), and
  every alert to start with is in
  [`deploy/base/prometheusrule.yaml`](../deploy/base/prometheusrule.yaml).
- Upgrading is applying the next release's archive: any release upgrades
  from any earlier one in the same major with no step, and rolling back
  inside a minor series is `kubectl rollout undo`. Across a major, read
  [`docs/upgrades/`](upgrades/README.md) first.

When you are done with the laptop cluster:

```sh
kind delete cluster --name lux
```
