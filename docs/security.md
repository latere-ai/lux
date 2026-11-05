# Security

What an installation of Lux guarantees the people who call it, what the
gateway sees and keeps, and what whoever runs `luxd` must keep safe. It
is for an operator and for a builder who calls the API. To report a
vulnerability, and for the commitments with the test behind each one,
see [`SECURITY.md`](../SECURITY.md).

## What Lux guarantees

A Key opens a door and an issuer token opens `/v1`, and neither works on
the other surface: a Key on `/v1` and a token on a door are each
`unauthenticated`. A door goes by the SHA-256 of the value it is
shown and asks the store for it, so it verifies no signature and reads no
claim. [Authentication](api.md#authentication) says which credential
each surface takes.

A provider credential never leaves the gateway. It is sealed in the
store, opened in memory for one outbound request, and sent only toward
that Provider's own base URL. No read, list, event, or log line returns
it. [What Lux sees](#what-lux-sees) says how it is kept, and
[Set and protect the KEK](#set-and-protect-the-kek) what protects it. A
Provider that reaches Anthropic through
[federation](#reach-anthropic-without-a-stored-key) holds no key at all.

A decision the authorizer cannot give is a refusal, never an allow. A
late, malformed, or missing answer, and an unreachable endpoint, are each
`authorizer_unavailable`, 503. [The minimal
authorizer](plane.md#the-minimal-authorizer) lists the rules an endpoint
keeps, and [Writing an authorizer](authorizer.md) says what yours
decides.

When a manifest names a Model, a Budget, or a Provider the subject may
not use, the answer is `not_found` with no reason, the same as for one
that does not exist, so a manifest cannot probe for another tenant's
objects. [What it does not defend](#what-it-does-not-defend) says where
that stops.

No prompt, completion, credential, or Key value reaches a usage record,
an event, a log line, a metric, or a span. [What Lux sees](#what-lux-sees)
lists what each one does carry, and [Observability](observability.md)
lists the metrics, log fields, and spans.

No route on either plane serves HTML, and no page in a browser can call
either plane cross-origin: neither plane writes an `Access-Control-*`
header. [`SECURITY.md`](../SECURITY.md#what-the-design-commits-to) names
the test behind it.

A Provider base URL that names a private address, a bare hostname, or the
gateway itself is refused when a manifest is resolved and again when the
gateway dials, unless `LUX_UPSTREAM_ALLOW_PRIVATE` is set. See
[Providers and upstream](configuration.md#providers-and-upstream).

An installation with no authorizer and no listed administrator declares
no upstream, so a fresh one holds no credential to take. See [Write and
lock down the authorizer](#write-and-lock-down-the-authorizer).

[`SECURITY.md`](../SECURITY.md#what-the-design-commits-to) names the test
behind each of these.

## What Lux sees

Lux keeps state in one of three places. With `LUX_DB_URL` set it is
Postgres. Without it, it is memory, held by one process and gone when it
ends. With `LUX_MANIFEST_DIR` set, it is the directory of manifests
loaded into memory, the file mode ([Store](configuration.md#store)).
Beyond the store, `luxd` writes logs, metrics, and traces, archives
request records to a bucket you name, sends events to a sink you name,
and asks your authorizer questions. A reader of one of these sees what
that one carries and nothing from the others.

| What | Kept in | Who can read it |
|---|---|---|
| The four object kinds (Provider, Model, Key, Budget) | Postgres, one `objects` table: each row has its kind, id, name, owner, version, the spec, the status, the labels, and a deletion time. Without a database, the same objects in memory, or in the manifest directory in file mode. A Key's status holds its first twelve characters, never the value. A Provider's `headers` are spec fields, stored and returned like the rest, so put a credential in `credential` and never in `headers` | whoever reads the store. Through `/v1`, whoever the authorizer lets read them. Under the built-in owner policy, any authenticated subject reads every Provider and Model, and only an owner or an administrator reads a Key or a Budget |
| A Provider's credential | Postgres, the `credentials` table, one row per Provider: its version and the four byte strings of the sealed value. The value is sealed with AES-256-GCM under a fresh 32-byte data key, and that data key is wrapped with AES-256-GCM under the first key of `LUX_SECRETS_KEK`. Both bind the provider id and the credential version, so a row copied onto another Provider fails to open. The store package never imports the sealing package. In every mode but file mode the value is sealed; in file mode it is read from the variable `credential.valueFrom.env` names and held in memory | whoever reads the table sees ciphertext. A replica holds the plaintext in memory while it opens the credential for a request |
| A Key's value | nowhere. Postgres keeps SHA-256 of the exact bytes in `key_hashes`, 64 hex characters against the Key's id. A minted value is `lux_` and 40 random characters, shown once in the response that creates or rotates the Key. A value the platform supplies in `spec.value` is write-once and returned by no read, and `spec.valueSHA256` supplies only the hash. In file mode a value read from `valueFrom.env` is held in memory | whoever reads the table sees hashes, which are not values |
| Usage totals | Postgres, `usage_hourly` and `usage_monthly`: one row per hour or month for each Key id, Model id, Provider id, owner, door, status, and currency, with the Key's labels, and the sums of requests, input, output, cached-input, and cache-write tokens, cost in micro-units, and unpriced requests. Without a database, memory | whoever reads the store. Through `GET /v1/usage`, whoever the authorizer lets read the Keys and owners asked about |
| Spend counters | Postgres, the `counters` table: a window's name and one integer. Memory without a database | whoever reads the store |
| A request record | one per data plane request. Its members are the id and times; the Key's id and prefix; the owner, as `issuer\|sub`; whether the Key was zero-retention; the Model and Provider by name and id; the upstream model; the door, the target dialect, the route template, and whether the request was translated, with the names of the fields and headers a translation dropped; each target tried, with its outcome, HTTP status, error code, and duration; the status, error code, latency, and time to first byte; token counts and whether they are estimates; the cost; whether it streamed; the Key's labels; and the pairs of the caller's `Lux-Labels` header, at most eight, names up to 64 and values up to 128 characters of a short alphabet. It has no member for a body, a header value, a credential, a Key value, or the caller's address | the store holds none of it. A replica keeps the newest 1000 per Key in memory for `GET /v1/requests` and loses them when it restarts. With `LUX_REQUESTLOG_EXPORTER=s3`, each record is also written as a line of newline-delimited JSON to your bucket, and `GET /v1/requests` reads from there. Whoever reads the bucket, and through the API whoever the authorizer lets read the Keys and owners asked about |
| Request bodies | not kept. A prompt and its completion pass through memory on the way to the provider and back | no one at the gateway |
| Events and their journal | Postgres, the `journal` table: per event its id, the object it names, a sequence, the type, the time, delivery attempts, and the payload, the exact bytes the sink receives. Memory without a database | whoever reads the store |
| Leases, tunnel sessions, and name fences | Postgres: a lease's name, holder, and expiry; a tunnel's Provider id, session, replica, subject, agent, and times; a fenced Key name with its owner and labels | whoever reads the store |
| Logs | JSON on standard error, one line per request. A data plane line has the request id, door, route template, Model and Provider names, status and code, the upstream status, the Key's prefix, the owner, durations, token counts, and the stream and zero-retention flags. A control plane line has the request id, route template, action, kind, the name in the path, status and code, the subject, and the duration. Every string passes one handler that cuts any minted Key value in it to its first twelve characters. The rule matches the `lux_` shape, so a supplied value is not caught by it, and none is ever written | wherever you send standard error |
| Metrics | `/metrics` on the internal listener. The labels are `door`, `model`, `provider`, `status`, `code`, `decision`, `direction`, `currency`, `result`, `op`, `kind`, `trigger`, and `state`. `model` and `provider` are an object's name or empty, never a caller's string. No Key, subject, owner, request id, or address is a label ([Metrics](observability.md#metrics)) | whoever can reach the internal listener, and your scraper |
| Traces | off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set. A request has a server span with the route template, status, and path, and `lux.request`, `lux.upstream`, `lux.api`, `lux.authorizer`, and `lux.store` spans below it, carrying the door, route, Model and Provider names, status, code, request id, flags, the action, and the decision. No span carries a subject, owner, Key, or address. The path can hold an object's name on `/v1` and a model's name on the Gemini door ([Traces](observability.md#traces)) | your collector |
| What the sink receives | one signed JSON event per change: the id, type, time, the subject that caused it, the reason, the request id, an object block with the kind, id, name, owner, and labels, and data that depends on the type. The data names a Provider's dialect and base URL, a Model's targets, a Key's prefix, models, and Budgets, a Budget's amount and window, the paths an update changed, and a spend limit reached. `provider.unreachable` carries the probe's error, which can include the first kilobyte of a provider's 5xx answer to the model-list probe with the Provider's credential replaced | the sink you run |
| What the authorizer is asked | on every `/v1` decision: the subject, its issuer and `sub`, every claim of the token as it arrived, the action, the resource, and the request's id, client address, and user agent. The resource is the object's id, name, owner, and labels, with the fields of its kind (a Provider's dialect, base URL, and tunnel flag; a Model's targets; a Key's models, Budgets, and prefix; a Budget's amount, currency, and window). A mutation adds the proposed owner, metadata, and spec with the status, every credential value, every supplied Key hash, and every Provider header value left out, and says whether a credential was supplied and what the header names are. A proposed Key adds a credential descriptor: `absent`, `inline`, `sha256`, `reference`, or `conflict`, and for `sha256` the SHA-256 of the hash string. The data plane asks nothing. The bearer `LUX_AUTHORIZER_TOKEN` rides on every call | your authorizer endpoint |
| What reaches a provider | the request body, translated into the Provider's dialect or passed through, with the Provider's `requestFields` merged in; the caller's other headers; `User-Agent: luxd/<version>`; and the Provider's credential, or the short-lived token federation exchanged, sent toward that Provider's base URL only. The hop-by-hop headers, every `Lux-*` header, every `X-Forwarded-*` and `Forwarded` header, and the caller's `Authorization`, `X-Api-Key`, `X-Goog-Api-Key`, `Proxy-Authorization`, and `Cookie` are removed. A tunneled Provider's runtime gets the same set plus a request id, over the session its agent opened | the provider, under its own terms. A Key with `zeroRetention` reaches only Providers that declare how they keep nothing |

A record the gateway writes holds no body because its type has none: the
usage record is scalars, enumerations, and two string maps, and a test
reads it by reflection for a member that could carry content. Tests also
run canary prompts, completions, credentials, and Key values through the
gateway and look for them in every event, record, log line, metric, and
span.

## How long it keeps things

| What | How long |
|---|---|
| Hourly usage rows | with `LUX_USAGE_RETENTION` unset, with no rule matching a row's Key labels, or with a rule whose `hourly` is `forever` or absent, without end. A matching rule folds the row into its month once its `hourly` period has passed, counted from the end of the hour, at least `24h`. The roll-up runs every hour on the replica that holds the usage lease |
| Monthly usage rows | folded rows are kept for the matching rule's `monthly` period after their month ends, at least `720h`, and then deleted. A rule without a `monthly` period keeps them without end. A fold can drop the `owner`, the `key`, or the `labels` from the monthly row, and a dropped label set is narrowed to the rule's `match` pairs |
| Usage without a database | gone when the process ends |
| An owner's identity in usage | `POST /v1/usage/redact` moves the sums of every hourly and monthly row of one owner onto the row with no owner, and empties the owner of the records in the memory of the replica that took the call. It does not rewrite objects already in the archive, and it does not reach the memory of another replica |
| Request records in memory | the newest 1000 per Key on each replica, the oldest dropped first. Gone when the replica restarts |
| Request records in the archive | Lux writes objects and never deletes one in service. They stay until your bucket's own rules remove them. Objects are keyed `<prefix>/yyyy/mm/dd/hh/<replica>-<id>.ndjson`, and with `LUX_REQUESTLOG_PARTITION_LABEL` set, `<prefix>/<value>/yyyy/mm/dd/hh/...`, where `<value>` is that Key label's value, or `_` for a Key without it, so a lifecycle rule can expire each partition on its own. Until written, records wait in a buffer of 50,000: a batch goes out at 5000 records or every 30 seconds, the oldest record is dropped when the buffer is full, and what is still in it when a stopping replica cannot reach the bucket is lost. `lux_requestlog_dropped_total` counts the drops |
| A deleted object | the row is marked deleted and stays, with its spec, labels, and owner, in the `objects` table. Nothing in `luxd` removes it. Its name is free at once and its id is never reused. A Key's hash row and a Provider's sealed credential row are deleted in the same transaction as the object. A fenced Key name stays fenced |
| A revoked or rotated Key | its hash row is deleted or replaced when the change commits, and a rotation stops the old value with no grace period. A replica keeps serving a Key it has cached for up to `LUX_KEY_CACHE`, 10 seconds by default. A replica that cannot reach the store keeps serving it for a further `LUX_KEY_CACHE_GRACE`, five minutes by default, and `0` refuses at the end of `LUX_KEY_CACHE`. A Key past its `expiresAt` is refused, and its row stays |
| Events in the journal | an event the sink has not taken is retried with backoff for 24 hours from its time and then dropped, with an error line. A delivered event's row stays: the code that removes delivered rows has no caller, so the journal grows without end |
| Spend counters | a row carries the end of its window, and nothing removes expired rows |
| A Provider's stored credential | until the credential changes, when the row is replaced, or the Provider is deleted or switched to federation, when it is deleted |
| A cached allow from your authorizer | in each replica's memory for the answer's `ttl`, at most 600 seconds, or 60 seconds when the answer names none. A deny is kept for 5 seconds and an unavailable answer is never kept. Only a decision about an object with an id is cached, so a create and a `model.use` are asked every time. A replica also remembers a subject's last allow, with its limits, for the same period |
| Logs, traces, and metrics | `luxd` removes none. Standard error, your collector, and your scraper keep them as you set them up. A replica's counters start again at zero when it restarts |
| Events at your sink, and questions at your authorizer | whatever you write those to do |

Without a database, everything the first rows describe lives in the
process: no state survives a restart, and the process assumes it is the
only replica ([Running it locally](local.md)).

## What it does not defend

[`SECURITY.md`](../SECURITY.md#what-is-not-defended) lists it in short.
This is what each position can still do.

**A caller holding a Key.**

- It calls every Model the Key names, on every door the Key opens, until
  the Key expires, is disabled, rotated, or deleted. A Key rate limit
  is whatever the Key sets, and with `LUX_DEFAULT_REQUESTS_PER_MINUTE`
  and `LUX_DEFAULT_TOKENS_PER_MINUTE` at their default of 0, a Key that
  names none has no rate limit.
- A spend limit or a Budget refuses before any bytes reach a provider,
  but it is checked from the store's counter plus the replica's own
  unflushed total, so spend can run past the limit by a bounded amount.
- It sends any prompt and reads any answer, and what it does with an
  answer is its own. The gateway cannot tell a useful answer from a
  stolen one.
- A Key with `passthrough` reaches the upstream's own API, including its
  storage endpoints, files, batches, and stored conversations, which
  keep what they are sent whatever a routing option says.
- It writes its own `Lux-Labels`, so it chooses what text of that shape
  lands in its records.
- A leaked value is stopped by rotating, disabling, or deleting the Key
  within the window in [Rotate and revoke
  Keys](#rotate-and-revoke-keys).

**A caller with a control plane token.** It does what your authorizer
allows its subject, and nothing else.

- With no authorizer, the built-in owner policy applies: it creates a
  Key or a Budget, acts on what it owns, reads every Provider and Model,
  and, if listed in `LUX_ADMIN_SUBJECTS`, acts on everything. An
  authorizer that allows everything allows everything.
- It cannot read a credential value or a Key value. It can tell an
  object that exists from one that does not: a read of an existing
  object the subject may not read is `forbidden`, and a read of a
  missing one is `not_found`. Only a reference inside a manifest is
  answered `not_found` either way.
- A permission you revoke is still honored by a replica for as long as it
  cached the allow, up to the window in [How long it keeps
  things](#how-long-it-keeps-things).
- A token is accepted until its own `exp`. Lux requires one and does not
  shorten it, so how long a stolen token works is your issuer's choice.
  A token `luxd token` signs lives at most 24 hours.

**A compromised provider.**

- It sees every prompt and completion routed to it, and the credential
  it is sent. A Key with `zeroRetention` reaches a Provider only because
  you declared how that upstream keeps nothing, and the gateway cannot
  check the declaration.
- What it answers is the caller's answer. A translated response is
  decoded and encoded again, and a passthrough response is relayed as
  opaque bytes with a `text/html` type changed to
  `application/octet-stream`. The gateway does not judge the content.
- A provider's 5xx answer to a probe reaches the Provider's status and
  `provider.unreachable` events, and the start of an error answer reaches
  the caller in `Lux-Error-Detail`, each with the credential removed.

**A network peer.**

- `luxd` serves plain HTTP. A peer on the path to the public listener
  reads every Key, every issuer token, and every body unless TLS ends in
  front of it ([Terminate TLS in front of the
  gateway](#terminate-tls-in-front-of-the-gateway)).
- A peer that can reach the internal listener reads `/metrics`, with the
  names of your Models and Providers and their counts, and the probes.
  In file mode it also reads the whole read-only `/v1`, which asks for no
  token and no authorizer. Nothing protects that listener but not routing
  to it. The shipped network policy admits the internal port from other
  `luxd` Pods only, and a cluster that does not enforce policies ignores
  it.
- A peer on the path to Postgres reads desired state and the counters
  unless you protect the connection; the credentials are ciphertext and
  Key rows hold hashes.
- Before authentication, each client address is limited to
  `LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE`, 60 by default. A wrong
  `LUX_TRUSTED_PROXIES` lets a caller choose its address.
- An observer of the gateway's outbound traffic learns which providers an
  installation uses and how often. TLS hides the bodies and not the
  destinations.

**The operator of a tunneled runtime.** The machine that runs `lux serve`
is a Provider's upstream.

- It sees, in the clear, every request routed to it: the body, the
  caller's other headers, and a request id, and it produces every answer.
  It sees no Key, no issuer token, and no provider credential.
- It cannot make the gateway dial anything: the agent dials its own
  `--upstream`, and the session carries one proxied request and one
  response at a time. A session belongs to the subject that opened it and
  ends with that token.
- Under the owner policy, any subject may attach a Provider it owns. With
  an authorizer, `provider.tunnel` decides.

**Whoever can read the store, the process environment, or the process
memory.** It holds `LUX_SECRETS_KEK`, the authorizer bearer, and the
events secret, and with the store, every provider credential. The
operator is trusted, and [What an operator keeps
safe](#what-an-operator-keeps-safe) is how far.

**The issuer, the authorizer, and the sink.** Each is an endpoint you
write or run, and the gateway trusts what a reachable one says. The
authorizer is told verified claims and never a Key value, a credential, or
a body, so a hostile one learns who called and not what was said.

**Whatever runs beside the gateway.** The gateway is a network service,
not a sandbox, and gives no kernel or hypervisor boundary to the
processes it shares a machine with.

## What an operator keeps safe

`luxd` reads these once, at start. The process holds the key-encryption
key, the bearers it sends to your endpoints, and a connection to the
store, which makes its host the root of the installation. A compromised
host gives every provider credential and the ability to act as the
gateway. Protect it first.

| What | If it leaks |
|---|---|
| `LUX_SECRETS_KEK` | with a copy of the store or a backup, opens every stored provider credential. Alone, it opens nothing. Recover with `luxd rewrap` under a new key, and rotate every Provider credential too, because the old ones were readable |
| The database, `LUX_DB_URL` and `LUX_DB_POOL_URL` | the URL carries the database password. A reader gets all desired state, Key hashes, usage with owners, events, and the sealed credentials, which are ciphertext. A writer changes what the gateway serves and to whom |
| The issuer: `LUX_OIDC_ISSUERS`, `LUX_OIDC_AUDIENCE`, and the signing key behind them, or `LUX_LOCAL_ISSUER_KEY` and `LUX_LOCAL_ISSUER_KEYS` | whoever signs a token for the audience can act as any subject on `/v1`, an administrator in `LUX_ADMIN_SUBJECTS` included, within what the authorizer allows that subject. List only issuers you trust that far |
| `LUX_ADMIN_SUBJECTS` | under the owner policy, each subject listed acts on every object, and only these subjects declare Providers and Models. Read and unused when an authorizer is set |
| `LUX_AUTHORIZER_TOKEN` | the bearer `luxd` sends your authorizer. Whoever holds it can put questions to your endpoint as the gateway. It makes the gateway allow nothing |
| `LUX_EVENTS_SECRET` | signs every event delivery. Whoever holds it can forge an event at your sink |
| `LUX_TUNNEL_FORWARD_SECRET` | the bearer of the route between replicas. Whoever holds it, and can reach the internal port, can send a request to a tunneled runtime |
| `LUX_S3_ACCESS_KEY` and `LUX_S3_SECRET_KEY` | read and write access to the request log archive, which holds every owner and every Key's labels |
| The workload identity: `LUX_WORKLOAD_IDENTITY_FILE` or the pod's service account | the token Anthropic exchanges for access under your federation rule |
| The internal listener, `LUX_INTERNAL_ADDR` | `/metrics`, the probes, the replicas' forward route, and in file mode the whole read-only `/v1` with no authentication. Do not publish its port |
| TLS in front of the public listener | without it, every Key and every issuer token crosses the network in the clear |
| `LUX_OIDC_INSECURE_ISSUERS`, `LUX_UPSTREAM_ALLOW_PRIVATE` | each admits what the default refuses: an issuer on `http://` off loopback, and a Provider on a private address. For a lab, not for production |

### Set and protect the KEK

`LUX_SECRETS_KEK` wraps every provider credential at rest. Generate 32
random bytes, base64-encoded, and keep them: a gateway restarted without
the key cannot open the credentials it stored.

```sh
head -c 32 /dev/urandom | base64
```

The value is never echoed by the process. Rotate by prepending a new key,
running `luxd rewrap`, then dropping the old one:
`LUX_SECRETS_KEK=new,old`, `luxd rewrap`, `LUX_SECRETS_KEK=new`. See
[`configuration.md`](configuration.md#secrets). If the old key leaked,
rotate each Provider's credential as well, since a copy of the store
opened under it.

### Terminate TLS in front of the gateway

`luxd` serves plain HTTP; without TLS a Key and an issuer token travel in
the clear. Front it with an ingress or proxy that terminates TLS, and set
`LUX_PUBLIC_URL` to the `https://` URL callers reach (see
[`configuration.md`](configuration.md#listeners)). The issuer, authorizer,
and event sink are reached over `https://`, or on loopback.

### Set the trusted proxies

Behind a proxy, per-caller rate limits and the authorizer's client
address are read from `X-Forwarded-For`. Set `LUX_TRUSTED_PROXIES` to the
CIDR ranges of the proxies you run, so the client is the last forwarded
entry outside those ranges; unset, no forwarded header is trusted and the
peer is the client. A range you do not control lets a caller spoof its
address, so name only your own. See
[`configuration.md`](configuration.md#keys-and-limits).

### Write and lock down the authorizer

With `LUX_AUTHORIZER_URL` unset, the built-in owner policy applies and
only the subjects in `LUX_ADMIN_SUBJECTS` may declare Providers and
Models; an empty list means a fresh installation holds no credential to
steal. For any real permission model, point `LUX_AUTHORIZER_URL` at an
endpoint you write and give it `LUX_AUTHORIZER_TOKEN`, the bearer `luxd`
sends it. That endpoint decides every control-plane request, and a
decision it cannot give is a refusal, never an allow, so keep it reachable
by the gateway alone. [`plane.md`](plane.md) has a minimal authorizer to
start from, [`authorizer.md`](authorizer.md) says what yours decides, and
the knobs are in [`configuration.md`](configuration.md#authorizer).

### Reach Anthropic without a stored key

An anthropic Provider can hold no API key at all. With
`credential.federation`, the gateway presents its own workload identity
at Anthropic's token endpoint and sends the short-lived token it gets
back, kept in memory and exchanged again before it expires. A copy of
the store, or of the KEK, then gives no one Anthropic access.

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: anthropic
spec:
  dialect: anthropic
  baseURL: https://api.anthropic.com/v1
  credential:
    federation:
      ruleID: fdrl_...
      organizationID: 00000000-0000-0000-0000-000000000000
      serviceAccountID: svac_...
      workspaceID: wrkspc_...   # when the rule covers more than one
```

The identity token is yours to configure, never a Provider's. On
Kubernetes, set `LUX_WORKLOAD_IDENTITY_KUBERNETES=1`: for every exchange
the gateway mints a 30-minute token for its pod's service account, with
the audience `LUX_WORKLOAD_IDENTITY_AUDIENCE` names
(`https://api.anthropic.com` by default), so each exchange presents a
token Anthropic has not seen. The service account needs this Role:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: luxd-token
rules:
  - apiGroups: [""]
    resources: [serviceaccounts/token]
    resourceNames: [luxd]   # the gateway's own service account
    verbs: [create]
```

Elsewhere, `LUX_WORKLOAD_IDENTITY_FILE` names a JWT the gateway reads
again for every exchange; keep it fresh well within an hour, since
Anthropic refuses a token it has exchanged before. In the Claude
Console, register your cluster's issuer, a service account, and a
federation rule matching the gateway's service account and audience,
then copy their IDs into the Provider. A Provider switched to
federation drops the key it stored.

Many clusters sign with an issuer only the cluster can reach, such as
`https://kubernetes.default.svc.cluster.local`, and their discovery
document names an in-cluster address for the keys, so Anthropic cannot
fetch them by discovery. Register such an issuer as a custom OIDC
issuer, with its keys either at a URL Anthropic can reach, such as the
API server's public `/openid/v1/jwks` with the cluster's CA once that
path is readable without sign-in, or pasted inline. Inline keys stop
working when the cluster rotates its signing key, until you paste the
new ones.

### Rotate and revoke Keys

A leaked Key value is contained by `POST /v1/keys/{id}/rotate`, which
replaces the value with no grace period; by `spec.disabled`, which refuses
at once; by `spec.ttl`; and by `DELETE`. Each takes effect on every
replica within `LUX_KEY_CACHE`, except on a replica cut off from the
store: it keeps serving the Keys it has cached for up to
`LUX_KEY_CACHE_GRACE` past that window, five minutes by default, and a
revocation reaches it only when the grace ends. Set
`LUX_KEY_CACHE_GRACE=0` where a revocation must hold through a store
outage; such a replica then refuses each Key `store_unavailable` once its
cached entry lapses. A Key's spend limit and its Budget bound what a leak
can cost before you notice.

### Hold an upstream to its data terms

A prompt the gateway forwards is kept by the upstream under the
upstream's terms. Where an upstream takes those terms per request in the
body, set them on the Provider with `spec.requestFields`: the gateway
merges the object into every request body it sends that Provider, after
any translation between dialects, and the Provider's value wins on each
member it names, so a caller can neither leave the option out nor turn
it off. A caller's other members, its own routing preferences beside
yours included, still reach the upstream.

For OpenRouter, this restricts every request to endpoints with zero data
retention:

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openrouter
spec:
  dialect: openai
  baseURL: https://openrouter.ai/api/v1
  requestFields:
    provider:
      zdr: true
```

A model that has no endpoint meeting the terms is refused by OpenRouter,
and the call fails rather than reaching an endpoint that keeps the
prompt.

OpenRouter's `data_collection: deny` is a separate, narrower option: it
leaves out the providers that may train on the data. Set it beside `zdr`
under `provider` when you want that exclusion as well.

To change the field on a running gateway, apply the Provider again with
`lux apply -f`, or `PUT` it to `/v1/providers/{name}`; a manifest that
carries no credential value keeps the stored one, and every replica
picks the change up as it does any other apply. A gateway that reads its
manifests from a directory takes the edited file when it starts again.
The field is returned by every read of the Provider, so it holds terms
and never a secret.

The merge reaches every request body that is a JSON object: each model
route, and each opaque route for a Key with `passthrough`. A body sent
with a `Content-Encoding` is refused toward such a Provider, because the
fields cannot be written into it. An option inside an uploaded file, such
as a batch input, is not a body the gateway reads, so keep `passthrough`
off on the Keys that reach a Provider whose terms must hold for every
call. The object cannot set `model`, `stream`, or `stream_options`,
which the gateway writes itself; the `ProviderSpec` schema in
[`api/openapi.yaml`](../api/openapi.yaml) has the full rule.

### Keep a workload's requests from upstreams that may keep them

`spec.requestFields` holds every caller of a Provider to the same terms.
When only some workloads need their requests kept from upstreams that
may store them, set `spec.zeroRetention: true` on the Keys those
workloads hold, and declare on each Provider whose upstream keeps
nothing of a request how it does so:

| The Provider's `spec.zeroRetention` | A request on a zero-retention Key |
|---|---|
| absent | is never sent to the Provider |
| `{}` | is sent as any request is: you state that the upstream keeps nothing of any request |
| `requestFields: {...}` | is sent with these fields merged into the body after `spec.requestFields`, winning on every member both name |

Requests on other Keys reach the same Providers as before and never
carry the zero-retention fields. For OpenRouter, the declaration carries
its routing option, so only the zero-retention Keys' requests are held
to endpoints with zero data retention:

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openrouter
spec:
  dialect: openai
  baseURL: https://openrouter.ai/api/v1
  zeroRetention:
    requestFields:
      provider:
        zdr: true
```

`data_collection: deny` can sit beside `zdr` under `provider`, as it can
in `spec.requestFields`. A Provider whose upstream keeps nothing of any
request, a model you serve yourself or one under a contract with no
retention, declares `zeroRetention: {}`. A direct API that keeps a
response for later retrieval unless the request says otherwise carries
that member, such as `requestFields: {store: false}`. The declaration is
your statement about the upstream's terms; the gateway holds requests to
it and cannot check the terms themselves.

The Key asks for it:

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: private-agent
spec:
  models: ["openrouter/*"]
  zeroRetention: true
```

A request on such a Key is sent only to the targets of its Model whose
Provider declares `spec.zeroRetention`, and falls over only among them.
A Model with no such target is refused `zero_retention_unavailable`,
403, before any upstream is called, and the same request keeps being
refused until a target's Provider declares it. When declaring targets
exist and none can be tried now, the answer is `provider_unavailable`,
as for any request with no target left. A caller cannot turn the fields
off: they win over the caller's members as `spec.requestFields` does,
and a body they cannot be written into, one that is not a JSON object or
carries a `Content-Encoding` other than `identity`, is refused
`invalid_request`.

An opaque route, open to a Key with `passthrough`, reaches the
upstream's own API, whose storage endpoints, files, batches, threads,
and stored conversations, keep what they are sent whatever a routing
option says. A zero-retention Key's opaque request is served only toward
a Provider that declares `{}`, and refused `zero_retention_unavailable`
toward any other.

The flag can be set or cleared on an existing Key with `lux apply -f`.
Every replica serves the change at its next read of the journal, one
second by default, and until then may serve the Key as it was. A
replica cut off from the store keeps serving the Keys it has cached for
up to `LUX_KEY_CACHE_GRACE` past `LUX_KEY_CACHE`, and a change reaches
it when the store answers again. If you build a platform whose users
turn the flag on, tell them this bound. Each request's record, its log
line, and its `lux.upstream` span say whether it was on a zero-retention
Key, so `lux requests` answers whether a past request was sent that way.

### Verify released artifacts

Every release is signed by the identity of the shared release stages,
run for this repository. Verify the images and the archives before you
run them, with `cosign verify`, `cosign verify-blob`, and `gh
attestation verify` against the build provenance;
[`SECURITY.md`](../SECURITY.md#verifying-a-release) has the commands, and
the ones for releases up to v0.20.0.
