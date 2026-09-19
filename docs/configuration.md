# luxd configuration

Every variable `luxd` reads from the environment, read once at
start-up by `internal/config`. A blank value is unset, and an unknown
variable is never an error, so a deployment that sets one before its feature
lands is not refused. A start-up with anything missing or malformed fails with
one message that lists every problem, sorted by variable name, so an operator
fixes a deployment in one round.

This page and the `.env.example` at the repository root are
generated from `internal/config` and held current by
`TestConfigurationReferenceIsCurrent`, so they never drift from the
code; `.env.example` is this same set as a file to copy.

Variables that are not luxd's live elsewhere: the `lux` command's own
in [`cli.md`](cli.md), the install script's `LUX_INSTALL_*` in
[`install.md`](install.md), the conformance suite's `LUX_TEST_*`
in the [conformance spec](../specs/018-conformance-suite.md), and the
OpenTelemetry exporter's `OTEL_*`, read by `latere.ai/x/pkg/otel`,
in [`observability.md`](observability.md). In file mode a
manifest may name the operator's own variables, outside the `LUX_`
namespace, in `credential.valueFrom.env` or `Key.spec.valueFrom.env`.

## Listeners

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_PUBLIC_ADDR` | Address the public listener binds: the dialect doors, the control plane under `/v1`, and the public probes. | `:8080` | No |
| `LUX_INTERNAL_ADDR` | Address the internal listener binds for the cluster's probes; must differ from `LUX_PUBLIC_ADDR` unless both ask for port 0. | `:8081` | No |
| `LUX_PUBLIC_URL` | Absolute URL callers reach the public listener at; the base of every URL in a response and the loop check of resolve. | `none` | Yes |

## Identity

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_OIDC_ISSUERS` | Comma-separated issuer URLs whose tokens the control plane accepts. | `none` | Yes, unless file mode |
| `LUX_OIDC_AUDIENCE` | The one audience a caller token must contain. | `lux` | No |
| `LUX_OIDC_INSECURE_ISSUERS` | Issuers from the list allowed to use `http://` off loopback; the test stubs set it, never production. | `unset` | No |

## Authorizer

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_AUTHORIZE_LIST_ITEMS` | Set to 1 to require each listed object's read permission in addition to the list filter. Refused objects are skipped; an authorizer outage fails the list. | `unset` | No |
| `LUX_AUTHORIZER_URL` | The operator's authorization endpoint; unset selects the built-in owner policy. | `unset` | No |
| `LUX_AUTHORIZER_TOKEN` | The bearer luxd sends the authorizer. Never echoed. | `unset` | When `LUX_AUTHORIZER_URL` is set |
| `LUX_AUTHORIZER_TIMEOUT` | One authorization decision's deadline, the retry included. | `5s` | No |
| `LUX_ADMIN_SUBJECTS` | Comma-separated subjects the built-in owner policy lets act on every object; read and unused when an authorizer is set. | `unset` | No |

## Store

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_MANIFEST_DIR` | A directory of manifests read at start: file mode, where desired state comes from disk and its kinds are read-only through the API. | `unset` | No |
| `LUX_DB_URL` | A `postgres://` URL selecting the Postgres store; unset keeps every state in memory. Never echoed. | `unset` | No |
| `LUX_DB_POOL_URL` | Optional transaction-pooler URL for serving queries. Requires `LUX_DB_URL`, which remains the direct migration connection. Both must reach the same database. Never echoed. | `unset` | No |
| `LUX_DB_MAX_CONNS` | The Postgres pool size; read only with `LUX_DB_URL`, between 1 and 100. | `8` | No |

## Keys and limits

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_KEY_CACHE` | How long a Key lookup, positive or negative, is cached per replica on the data plane (1s to 10m). | `10s` | No |
| `LUX_DEFAULT_REQUESTS_PER_MINUTE` | The requests-per-minute a Key gets when it names none; 0 is no limit. | `0` | No |
| `LUX_DEFAULT_TOKENS_PER_MINUTE` | The tokens-per-minute a Key gets when it names none; 0 is no limit. | `0` | No |
| `LUX_REQUESTS_PER_MINUTE` | Control-plane requests one subject may send in a minute; 0 is no limit. | `600` | No |
| `LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE` | Requests one client address may send before authentication, on both planes; 0 is no limit. | `60` | No |
| `LUX_TRUSTED_PROXIES` | Comma-separated CIDR ranges of front proxies whose `X-Forwarded-For` names the client; unset trusts no header. | `unset` | No |
| `LUX_MAX_MANIFEST_BYTES` | The largest manifest or JSON body accepted on the control plane (1Ki to 16Mi). | `64Ki` | No |
| `LUX_MAX_BODY_BYTES` | The largest data-plane request body, and the cap on an upstream body read whole (4Ki to 1Gi). | `64Mi` | No |
| `LUX_METERING_FLUSH` | How often a replica writes its spend deltas and usage aggregates to the store (100ms to 1m). | `1s` | No |

## Providers and upstream

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_UPSTREAM_ALLOW_PRIVATE` | 1 admits a Provider base URL, and the address it resolves to, on a loopback, link-local, or private network. | `unset` | No |
| `LUX_UPSTREAM_TIMEOUT` | The deadline of one upstream request including its stream (1s to 1h). | `10m` | No |
| `LUX_DISCOVERY_INTERVAL` | How often a Provider's model list is refreshed (1m to 24h). | `1h` | No |
| `LUX_HEALTH_INTERVAL` | How often a Provider is health-probed (5s to 10m). | `30s` | No |

## Events and request log

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_EVENTS_URL` | The event sink; unset turns events off. Reached over `https://` or on loopback. | `unset` | No |
| `LUX_EVENTS_SECRET` | The HMAC-SHA256 key every delivery's `Lux-Signature` is signed with. Never echoed. | `unset` | When `LUX_EVENTS_URL` is set |
| `LUX_REQUESTLOG_EXPORTER` | Where the request log is archived: `none` or `s3`. | `none` | No |
| `LUX_S3_ENDPOINT` | The S3-compatible endpoint URL of the request-log archive; there is no default endpoint. | `unset` | When exporter is `s3` |
| `LUX_S3_REGION` | The request-log archive's region. | `us-east-1` | No |
| `LUX_S3_BUCKET` | The request-log archive's bucket. | `unset` | When exporter is `s3` |
| `LUX_S3_ACCESS_KEY` | The request-log archive's access key; there is no credential chain. Never echoed. | `unset` | When exporter is `s3` |
| `LUX_S3_SECRET_KEY` | The request-log archive's secret key. Never echoed. | `unset` | When exporter is `s3` |
| `LUX_S3_PREFIX` | The key prefix request-log objects are written under. | `lux/` | No |

## Tunnel

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_TUNNEL_ENABLED` | 1 serves the tunnel routes and admits `spec.tunnel`. | `unset` | No |
| `LUX_TUNNEL_REGISTRY_TTL` | The liveness window of a registry row; the agent heartbeats at a third of it (5s to 5m). | `30s` | No |
| `LUX_TUNNEL_FORWARD_ADDR` | The `host:port` other replicas reach this one's internal listener at; unset serves a tunnelled Provider on the holding replica only. | `unset` | No |
| `LUX_TUNNEL_FORWARD_SECRET` | Comma-separated bearers of the forward route: the first is sent, every one is accepted, so a rotation is prepending. Each at least 32 bytes. Never echoed. | `unset` | When `LUX_TUNNEL_FORWARD_ADDR` is set |

## Secrets

| Variable | Meaning | Default | When required |
|---|---|---|---|
| `LUX_SECRETS_KEK` | One to eight 32-byte keys, standard base64, comma separated; the first wraps every new data key, every key is tried to open one, so rotation is prepending a key and running `luxd rewrap`. Never echoed. | `none` | Yes for serve and rewrap, except file mode |
