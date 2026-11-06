# Running it locally

`luxd` on your own machine, from a clone: a stand-in provider for each
model dialect, a stand-in issuer, a stand-in authorizer and a stand-in
event receiver, and requests that show a call going through each of
the gateway's four doors. One command does the setup, and the same
terminal sends the requests.

Everything here is for trying Lux. The stand-in providers answer with a
fixed string and never call a model, the stand-in issuer signs a token
for anyone who asks, and the gateway keeps its state in memory, so
stopping it forgets everything. [Install](install.md) runs a build
against a real provider and installs a release on a cluster, and the
[quick start](quickstart.md) runs the published images with no build.

## What you need

- Go 1.27 or newer, to build `luxd`, the stand-ins and the `lux`
  command.
- `make` and `curl`.
- A checkout of this repository. No container engine, no database and
  no API key.

## Start it

From the root of a clone:

```sh
git clone https://github.com/latere-ai/lux.git
cd lux
make run
```

`make run` builds, starts everything in the background and returns
when the gateway is ready. It does these things, in this order:

1. **Builds** `out/luxd`, the gateway, and `out/lux-stubs`, the
   stand-ins. It does not build the `lux` command; a step below does.
2. **Stops** anything an earlier `make run` left behind, by the
   process ids under `out/run`.
3. **Starts the stand-ins** in one process, `lux-stubs`: a provider for
   each of the four dialects (OpenAI, Anthropic, Gemini and lux), an
   issuer, an authorizer and an event sink. The issuer signs a token
   for whoever asks at its `/mint` address and serves the keys that
   verify it. The authorizer allows every request it is asked. The sink
   checks each event's signature, keeps it, and lets you read it back.
   Their log, `out/run/stubs.log`, names each address:

   ```
   lux-stubs: openai http://127.0.0.1:33727
   lux-stubs: anthropic http://127.0.0.1:33728
   lux-stubs: gemini http://127.0.0.1:33729
   lux-stubs: lux http://127.0.0.1:33730
   lux-stubs: issuer http://127.0.0.1:33731
   lux-stubs: authorizer http://127.0.0.1:33732
   lux-stubs: sink http://127.0.0.1:33733
   lux-stubs: index http://127.0.0.1:58090
   lux-stubs: issuer url http://127.0.0.1:33731
   lux-stubs: ready
   ```

   The index answers with a document that names the other seven. It
   listens on a port the operating system picks, so yours differs.
4. **Starts the gateway**, `luxd serve`, with the memory store, the
   stand-in issuer as the only issuer it trusts, and the stand-in
   authorizer deciding every `/v1` request. It waits until the
   gateway's readiness check answers. Its log is `out/run/luxd.log`.
5. **Applies the example manifests.** It asks the issuer for a token
   for `dev`, then sends each file in `deploy/examples` to the control
   plane in turn: four Providers, four Models, one Budget and one Key.
   The files name the stand-ins at fixed ports, and `make run` rewrites
   them to your ports first. The copies are in `out/run/examples`. It
   then waits, a few seconds, until the gateway has checked each
   stand-in and reads every Model available, so the doors list the
   Models by the time the exports print. Here the gateway checks its
   Providers every 5 seconds; the default is 30.
6. **Prints three exports**, and a few commented lines to try:

   ```
   export LUX_URL=http://localhost:33725
   export LUX_TOKEN=eyJhbGciOiJSUzI1NiIs...
   export LUX_KEY=lux_ERwqZairQfqv7LdwSNROfso1gMjjo4R_EFqMmO2H
   # openai SDK: OPENAI_BASE_URL=http://localhost:33725/openai/v1 OPENAI_API_KEY=$LUX_KEY
   # curl -sS http://localhost:33725/openai/v1/chat/completions -H "Authorization: Bearer $LUX_KEY" -H 'Content-Type: application/json' -d '{"model":"stub-openai","messages":[{"role":"user","content":"hello"}]}'
   # events arrive at http://127.0.0.1:33733/_events; stop with make run-down
   ```

   The token is cut here; yours is a long signed string, good for one
   hour. The Key's value is shown once, in the answer that created it,
   and not again. Paste the three `export` lines into your shell.

The two credentials are not interchangeable. `LUX_TOKEN` is an issuer
token and opens the control plane under `/v1`. `LUX_KEY` is a Key and
opens the doors that carry inference. A Key on `/v1` and a token on a
door are each refused. [The API](api.md#authentication) says more.

**The ports.** Every port counts up from a base that `make` computes
from the path of the directory you cloned into: `RUN_PORT`, here
33725. A clone in another place gets other ports, which lets two
clones run side by side, so read your own from the printed lines.

| Port | What listens |
|---|---|
| base | the gateway: the doors and the control plane |
| base + 1 | the gateway's probes (`/readyz`, `/livez`, `/metrics`) |
| base + 2 | the OpenAI stand-in provider |
| base + 3 | the Anthropic stand-in provider |
| base + 4 | the Gemini stand-in provider |
| base + 5 | the lux stand-in provider |
| base + 6 | the issuer |
| base + 7 | the authorizer |
| base + 8 | the event sink |

Everything listens on `127.0.0.1`, and the public URL says `localhost`
on purpose: the gateway refuses a Provider on its own hostname, and
the stand-ins sit on the other name. If a port is taken, or you want
numbers you can remember, pick the base yourself. Nine ports in a row
from it must be free:

```sh
make run RUN_PORT=30000
```

## Send requests

The Models in `deploy/examples` are named `stub-openai`,
`stub-anthropic`, `stub-gemini` and `stub-lux`. Each routes to the
stand-in provider of its dialect. A stand-in answers
`stub:<dialect>:<model>:<digest>`, where the digest is the start of
the SHA-256 of your last message, and reports 100 input tokens and 20
output tokens, so every call has a known size and price. Each Model
also gives an input window of 200,000 tokens and an output limit of
4,096, which the doors' model lists carry as they do for a real model.
The stand-ins take a request of any length; the figures are there for a
client that reads them from the list.

### One request through each door

The OpenAI door takes the Key as a bearer:

```sh
curl -sS $LUX_URL/openai/v1/chat/completions \
  -H "Authorization: Bearer $LUX_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"stub-openai","messages":[{"role":"user","content":"hello"}]}'
```

```json
{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"stub:openai:stub-openai:2cf24dba","role":"assistant"}}],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion","usage":{"completion_tokens":20,"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0},"total_tokens":120}}
```

The Anthropic door takes the Key in `x-api-key`, as the Anthropic SDK
sends it:

```sh
curl -sS $LUX_URL/anthropic/v1/messages \
  -H "x-api-key: $LUX_KEY" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"stub-anthropic","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
```

```json
{"content":[{"text":"stub:anthropic:stub-anthropic:2cf24dba","type":"text"}],"id":"msg_stub","model":"stub-anthropic","role":"assistant","stop_reason":"end_turn","stop_sequence":null,"type":"message","usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":100,"output_tokens":20}}
```

The Gemini door puts the model in the path and takes the Key in
`x-goog-api-key`:

```sh
curl -sS "$LUX_URL/gemini/v1beta/models/stub-gemini:generateContent" \
  -H "x-goog-api-key: $LUX_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}'
```

```json
{"candidates":[{"content":{"parts":[{"text":"stub:gemini:stub-gemini:2cf24dba"}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"stub-gemini","usageMetadata":{"cachedContentTokenCount":0,"candidatesTokenCount":20,"promptTokenCount":100,"totalTokenCount":120}}
```

The lux door is the gateway's own dialect, with the text of a message
in `blocks`:

```sh
curl -sS $LUX_URL/lux/v1/generate \
  -H "Authorization: Bearer $LUX_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"stub-lux","messages":[{"role":"user","blocks":[{"type":"text","text":"hello"}]}]}'
```

```json
{"blocks":[{"text":"stub:lux:stub-lux:2cf24dba","type":"text"}],"id":"lux-stub","model":"stub-lux","stop_reason":"end_turn","usage":{"cache_read_input_tokens":0,"cache_write_input_tokens":0,"input_tokens":100,"output_tokens":20}}
```

The digest is the same in all four, because the message is the same.
The OpenAI, Anthropic and lux doors translate into one another, so you
could send `stub-anthropic` to the OpenAI door too. The Gemini door
reaches only a Gemini provider. [The API](api.md#the-doors) has the
rules.

### A streamed request

Add `"stream":true` and the answer arrives as server-sent events.
`-N` stops `curl` from holding the output back:

```sh
curl -sS -N $LUX_URL/openai/v1/chat/completions \
  -H "Authorization: Bearer $LUX_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"stub-openai","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}'
```

```
data: {"choices":[{"delta":{"content":"stub:op","role":"assistant"},"finish_reason":null,"index":0}],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"enai:st"},"finish_reason":null,"index":0}],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"ub-open"},"finish_reason":null,"index":0}],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"ai:2cf2"},"finish_reason":null,"index":0}],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"4dba"},"finish_reason":"stop","index":0}],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion.chunk"}

data: {"choices":[],"created":0,"id":"chatcmpl-stub","model":"stub-openai","object":"chat.completion.chunk","usage":{"completion_tokens":20,"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0},"total_tokens":120}}

data: [DONE]

```

The five pieces of content join into the same string as the first
answer. The last chunk carries the token counts, which the gateway
reads to price the call.

### Read the control plane

Now the other credential. Put the token in a header variable:

```sh
A="Authorization: Bearer $LUX_TOKEN"
```

List the Models. Each comes back in manifest shape, with a `status`
that says whether its targets are healthy (output shortened to the
first of four):

```sh
curl -sS -H "$A" $LUX_URL/v1/models
```

```json
{"items":[{"apiVersion":"lux.latere.ai/v1beta1","kind":"Model","metadata":{"name":"stub-anthropic"},"spec":{"targets":[{"provider":"anthropic","model":"stub-anthropic","weight":100,"priority":0}],"fallback":"onError","pricing":{"currency":"USD","per":1000000,"input":"3","output":"15","cachedInput":"3","cacheWrite":"3"},"modalities":{"input":["text"],"output":["text"]},"contextWindow":200000,"maxOutputTokens":4096},"status":{"id":"mdl_01M6PZ84G1QXQFEKHJ6WT9DYHN","version":1,"owner":"http://127.0.0.1:33731|dev","source":"declared","available":true,"targets":[{"provider":"anthropic","model":"stub-anthropic","health":"Healthy"}],"createdAt":"2026-11-06T00:03:36.193215+01:00","updatedAt":"2026-11-06T00:03:36.193504+01:00","warnings":[]}}, ...]}
```

Read the Key. Its `status` holds the first characters of its value,
the models it reaches, the Budget it draws from, and what it has spent
so far. The request counts include the five calls above, so they match
what you sent:

```sh
curl -sS -H "$A" $LUX_URL/v1/keys/dev
```

```json
{"apiVersion":"lux.latere.ai/v1beta1","kind":"Key","metadata":{"name":"dev"},"spec":{"models":["*"],"limits":{"requestsPerMinute":0,"tokensPerMinute":0},"budget":"dev","allowUnpriced":false,"passthrough":false,"disabled":false},"status":{"id":"key_01M6PZ84HMMPVBQ5F6CDPDPEG3","version":1,"owner":"http://127.0.0.1:33731|dev","prefix":"lux_ERwqZair","state":"Active","selectors":[{"selector":"*","matched":["stub-anthropic","stub-gemini","stub-lux","stub-openai"]}],"budget":{"name":"dev","id":"bud_01M6PZ84H9A01WSX49X83PRF3R"},"usage":{"total":{"requests":5,"tokens":600,"spend":"0.001865"}},"lastUsedAt":"2026-11-05T23:04:00Z","createdAt":"2026-11-06T00:03:36.243608+01:00","updatedAt":"2026-11-06T00:03:36.24427+01:00","warnings":[]}}
```

Read the usage the requests recorded. Each replica writes usage to the
store once a second (`LUX_METERING_FLUSH`), so a read straight after a
call may not show it yet; the request records below appear at once.
Without a filter it is one row for everything. `cost` is in millionths of a dollar, so 1865 is
0.001865 USD, the `spend` above:

```sh
curl -sS -H "$A" $LUX_URL/v1/usage
```

```json
{"items":[{"dimensions":{},"requests":5,"ok":5,"refused":0,"failed":0,"inputTokens":500,"outputTokens":100,"cachedInputTokens":0,"cacheWriteTokens":0,"cost":1865,"currency":"USD","unpricedRequests":0}]}
```

`by` groups the rows, up to three dimensions. Grouped by door, the
OpenAI door shows two requests, the plain one and the streamed one:

```sh
curl -sS -H "$A" "$LUX_URL/v1/usage?by=door"
```

```json
{"items":[{"dimensions":{"door":"anthropic"},"requests":1,"ok":1,"refused":0,"failed":0,"inputTokens":100,"outputTokens":20,"cachedInputTokens":0,"cacheWriteTokens":0,"cost":600,"currency":"USD","unpricedRequests":0},{"dimensions":{"door":"gemini"},"requests":1,"ok":1,"refused":0,"failed":0,"inputTokens":100,"outputTokens":20,"cachedInputTokens":0,"cacheWriteTokens":0,"cost":225,"currency":"USD","unpricedRequests":0},{"dimensions":{"door":"lux"},"requests":1,"ok":1,"refused":0,"failed":0,"inputTokens":100,"outputTokens":20,"cachedInputTokens":0,"cacheWriteTokens":0,"cost":140,"currency":"USD","unpricedRequests":0},{"dimensions":{"door":"openai"},"requests":2,"ok":2,"refused":0,"failed":0,"inputTokens":200,"outputTokens":40,"cachedInputTokens":0,"cacheWriteTokens":0,"cost":900,"currency":"USD","unpricedRequests":0}]}
```

Each request has a record of its own, newest first. This is the last
one, the streamed call (output shortened to its first record):

```sh
curl -sS -H "$A" "$LUX_URL/v1/requests?limit=1"
```

```json
{"items":[{"id":"req_01M6PZ9A3FAD1YXK1PKWEMVSHV","at":"2026-11-05T23:04:14.703866Z","endedAt":"2026-11-05T23:04:14.70423Z","key":{"id":"key_01M6PZ84HMMPVBQ5F6CDPDPEG3","prefix":"lux_ERwqZair"},"owner":"http://127.0.0.1:33731|dev","zeroRetention":false,"model":{"name":"stub-openai","id":"mdl_01M6PZ84GYTRA6AD87KZ9P0EGT"},"provider":{"name":"openai","id":"prv_01M6PZ84FQ96KE4XR49RG7ECMX"},"upstreamModel":"stub-openai","door":"openai","targetDialect":"openai","route":"/openai/v1/chat/completions","translated":false,"loss":[],"attempts":[{"provider":"openai","upstreamModel":"stub-openai","status":"ok","httpStatus":200,"error":"","durationMs":0}],"status":"ok","error":"","upstreamStatus":200,"latencyMs":0,"ttfbMs":0,"tokens":{"input":100,"output":20,"cachedInput":0,"cacheWrite":0,"reasoning":0,"estimated":false},"cost":{"amount":450,"currency":"USD","priced":true},"stream":true,"labels":{},"requestLabels":{}}, ...],"next_cursor":"cmVjb3Jkc3xk..."}
```

The cost is 450 millionths of a dollar: 100 input tokens at 2.50 and
20 output tokens at 10 per million, the prices in
`model-stub-openai.yaml`.

### See the events

The gateway posts a signed event to the sink for every change to a
Provider, Model, Budget or Key, and when a Provider's health changes.
Those are the events `make run` caused when it applied the examples.
The sink lists what it received at the address `make run` printed:

```sh
curl -sS http://127.0.0.1:33733/_events
```

It answers one JSON list, each entry with the `body` of the event and
when the sink got it. The first, shortened:

```json
[{"id":"evt_01M6PZ84ERJFRBVDG2VB7754NZ","attempts":1,"receivedAt":"2026-11-06T00:03:37.037657+01:00","body":{"id":"evt_01M6PZ84ERJFRBVDG2VB7754NZ","type":"provider.created","time":"2026-11-05T23:03:36.150617Z","subject":"http://127.0.0.1:33731|dev","reason":"request","request_id":"req_01M6PZ84EP5GXJ6J3YWR6VFRBA","object":{"kind":"Provider","id":"prv_01M6PZ84ERYBE9WDPNW119PRN3","name":"anthropic","owner":"http://127.0.0.1:33731|dev","labels":{}},"data":{"baseURL":"http://127.0.0.1:33728/v1","dialect":"anthropic"}}}, ...]
```

There are fourteen: four `provider.created`, four `model.created`, one
`budget.created`, one `key.created` and four `provider.healthy`. The
requests you sent are not events. They are the usage and request
records you read above. [Configuration](configuration.md#events-and-request-log)
lists the settings that point events at your own receiver.

### The same reads with the `lux` command

[The `lux` command](cli.md) does these reads without `curl`. `make run`
does not build it, so build it from the root of the clone. It reads the
three variables you exported:

```sh
go build -o out/lux ./cmd/lux
out/lux whoami
```

```json
{"subject":"http://127.0.0.1:33731|dev","issuer":"http://127.0.0.1:33731","sub":"dev","claims":{"aud":["lux"],"exp":1793923416,"iat":1793919816,"iss":"http://127.0.0.1:33731","sub":"dev"},"policy":"authorizer"}
```

`-o table` prints a kind or a report as a table:

```sh
out/lux list models -o table
out/lux get key dev -o table
out/lux usage -by model -by door -o table
```

```
NAME            SOURCE    TARGETS                   AVAILABLE  PRICED  AGE  OWNER
stub-anthropic  declared  anthropic/stub-anthropic  true       yes     53s  http://127.0.0.1:33731|dev
stub-gemini     declared  gemini/stub-gemini        true       yes     53s  http://127.0.0.1:33731|dev
stub-lux        declared  lux/stub-lux              true       yes     53s  http://127.0.0.1:33731|dev
stub-openai     declared  openai/stub-openai        true       yes     53s  http://127.0.0.1:33731|dev
```

```
NAME  PREFIX        STATE   MODELS  BUDGET  EXPIRES  OWNER
dev   lux_ERwqZair  Active  *       dev     never    http://127.0.0.1:33731|dev
```

```
REQUESTS  OK  REFUSED  FAILED  INPUTTOKENS  OUTPUTTOKENS  CACHEDINPUTTOKENS  CACHEWRITETOKENS  COST  CURRENCY  UNPRICEDREQUESTS
1         1   0        0       100          20            0                  0                 600   USD       0
1         1   0        0       100          20            0                  0                 225   USD       0
1         1   0        0       100          20            0                  0                 140   USD       0
2         2   0        0       200          40            0                  0                 900   USD       0
```

`out/lux models` asks the lux door which models the Key may call. It
is the one command that sends the Key, and the request it makes shows
up in the request records like any other.

## The file mode

`make run-file` starts the same stand-ins and the same gateway, with
the examples read from the directory `out/run/examples` instead of
applied through the control plane. There is no issuer and no
authorizer, so there is no `LUX_TOKEN`, and the control plane is
read-only: it answers on the probe port, base + 1, with no bearer, and
a write there is refused with `read_only`. The Key's value is not
minted by the server. It is generated once, kept in `out/run/dev-key`,
and read through the `STUB_DEV_KEY` variable the Key manifest names, so
it is the same on every run. The doors work as above with the
`LUX_KEY` it prints.

The `lux` command reads this control plane as it is: with no token it
sends no bearer, and it never sends the Key to `/v1`. Point `LUX_URL`
at the address on the `# file mode` line `make run-file` printed,
without `/v1`. On this run it was port 28604:

```sh
LUX_URL=http://127.0.0.1:28604 out/lux list models -o table
```

```
NAME            SOURCE    TARGETS                   AVAILABLE  PRICED  AGE  OWNER
stub-anthropic  declared  anthropic/stub-anthropic  true       yes     1s   file|manifest-dir
stub-gemini     declared  gemini/stub-gemini        true       yes     1s   file|manifest-dir
stub-lux        declared  lux/stub-lux              true       yes     1s   file|manifest-dir
stub-openai     declared  openai/stub-openai        true       yes     1s   file|manifest-dir
```

A write such as `lux apply` exits 1 with "This server reads its
manifests from a directory and cannot change them."

## Stop it

```sh
make run-down
```

```
stopped luxd (11759)
stopped stubs (11718)
```

`make run-down` stops both processes by the ids in `out/run` and waits
for each to exit. Starting `make run` again does the same first, so you
never stack two. The gateway keeps nothing, so the next start has new
ids, a new token and a new Key value.

`make clean` removes `out/`, which holds the binaries, the `lux`
command you built, the logs, the rendered examples and the keys. It
does not stop anything, so run `make run-down` first. Run it to start
over from nothing.

## What next

- [Quick start](quickstart.md): the same stand-ins from the published
  images, with no clone and no build.
- [Install](install.md): a build in front of a real provider, and a
  release on a cluster with your own issuer.
- [Configuration](configuration.md): every setting `luxd` reads, and
  the ones `make run` sets.
- [The API](api.md): the control plane, the doors, and the errors.
- [The lux command](cli.md): applying manifests and reading usage from a
  shell.
