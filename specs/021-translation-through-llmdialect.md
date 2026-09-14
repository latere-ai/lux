---
title: "Translation through llmdialect: the codec glue leaves the gateway for an importable bridge"
status: testing
track: core
depends_on:
  - specs/004-request-path.md
  - specs/018-conformance-suite.md
affects: [gateway/, internal/arch/, .github/workflows/]
effort: medium
created: 2026-09-14
updated: 2026-09-14
author: changkun
---

# Translation through llmdialect

## Overview

[[001-architecture]] says dialect translation is not a package of this
module: `latere.ai/x/pkg/llmdialect` is the open source core for it and
`gateway` imports it. Half of that is true today. The codecs are
`llmdialect`'s; everything around them, 1438 lines in six files of
`gateway`, is this repository's: the request and response legs with the
model name rewritten between them, the streaming event loop with the
last-value usage rule, the mid-stream error frame per door, the four
error envelope shapes, the four model-list shapes, count-tokens
emulation, and the reading of each dialect's usage members. None of it
needs a Key, a route, a Provider, or an `http.Request`; all of it is
bytes in and bytes out.

That layer becomes an importable package,
`latere.ai/x/pkg/llmdialect/bridge`, and `gateway` calls it. The point
is not smaller files. It is that translating between provider APIs
stops requiring a gateway: a program that has bytes from one dialect
and wants bytes for another imports the bridge, and a second
implementation of these doors, in this repository or a fork, does not
rewrite the same 1438 lines a fourth time.

Nothing a caller sees changes. Same bytes on the wire, same headers,
same envelopes, same records, same codes. This spec is a move with a
byte-equality proof attached.

## Current state

Built on 2026-09-14, in the six commits the swap below numbers, on the
pseudo-version of the pkg commit named at the end of this section:
`gateway` calls the bridge for every leg, shape, count, usage reading,
and member edit, and holds what the design says it still decides in
`translate.go`, `doorDialect`, `targetDialect`, `bridgeFor`, and
`bridgeFailure`. The six files fell from 1438 lines to 618 in three,
`models.go`, `usage.go`, and `probe.go` are gone, and `gateway` imports
`llmdialect/bridge` and `llmdialect/ir` from the llmdialect tree and
nothing else of it. Before the move, `gateway` was
[[004-request-path]]'s handler, complete and at 96% statement
coverage, and the six files that held the layer were:

| File | Non-test lines | What it holds |
|---|---|---|
| `translate.go` | 168 | the codec pair per route and target, the codec options, the dropped-header loss entries, the re-emission of a whole response as events |
| `stream.go` | 227 | the passthrough relay, the SSE frame rewrite, the translated event loop, the stream's usage and error frame |
| `errors.go` | 321 | the codes and their statuses and sentences, the four envelope shapes, the Google status names, the stream error frame, the response headers |
| `models.go` | 121 | the four model-list and entry shapes |
| `usage.go` | 318 | every dialect's usage members, the last-value rule, the SSE and JSON sniffers |
| `probe.go` | 283 | the JSON probe and the member edits: the model name, `stream_options.include_usage`, member removal |

`gateway` imports eleven `latere.ai/x/pkg` packages, eight of them for
this layer: `llmdialect`, `llmdialect/ir`, `llmdialect/anthropic`,
`llmdialect/openaichat`, `llmdialect/openairesp`, `llmdialect/lux`,
`llmdialect/tokencount`, and `httpjson`.

The bridge is built and pushed in `latere.ai/x/pkg` at commit
`2e4ba0359ec37bdf30a753b029c914e35a5bb3fe` and not tagged yet. This
spec pins that commit, the pseudo-version `go get` writes for it, and
the tag follows: moving the pin to the tag is one `go get` with no
other change. That dependency is not in `depends_on`, which names
specs of this repository only.

## Design

### The surface the gateway uses

```go
b, err := bridge.Open(from, to ir.Dialect, bridge.Options{
	DefaultMaxTokens:       int64, // Model.spec.maxOutputTokens, 0 for the codec's 4096
	DropSampling:           bool,  // false, as [[004-request-path]] says
	UseMaxCompletionTokens: bool,  // OpenAIReasoningFamily(upstream name)
})

out, loss, err := b.Request(body, bridge.RequestOptions{Model: upstream, Loss: dropped})
out, loss, usage, err := b.Response(body, bridge.ResponseOptions{Model: shown})
usage, err := b.Stream(w, r, bridge.StreamOptions{Model: shown, FirstByte: f, Flush: g, Fail: h})
usage, err := b.StreamResponse(w, body, opts)

bridge.Envelope(wire, bridge.Failure{...}) []byte
bridge.ErrorFrame(wire, bridge.Failure{...}) []byte
bridge.ModelList(wire, []bridge.Model) []byte
bridge.ModelEntry(wire, bridge.Model) []byte
bridge.CountTokensFor(dialect, body) (n int64, estimated bool, err error)
bridge.CountBody(wire, n) []byte
bridge.UsageOf(wire, body) (bridge.Usage, bool)
bridge.NewUsageScanner(wire, framing) *bridge.UsageScanner
bridge.Probe(body) (bridge.Call, error)
bridge.SetModel(body, name) []byte
bridge.SetModelInFrame(frame, name) []byte
bridge.SetIncludeUsage(body) []byte
bridge.RemoveMember(body, key) []byte
bridge.Scope(err) llmdialect.RefusalScope
```

A `bridge.Wire` names an API family and its shapes: `WireOpenAI`,
`WireAnthropic`, `WireGoogle`, `WireLux`. It is one per door, and the
gateway's `v1.Dialect` maps onto it one to one. The bridge's own
failures carry one code, one sentence, and one detail, which the
gateway maps onto its own table below.

### What moves and what stays

| Today | After |
|---|---|
| `frontendFor`, `backendFor`, `codecs`, `codecsFor` | one `bridge.Open(door wire, target wire, options)` per attempt; the option values stay the gateway's, computed as the codec options table of [[004-request-path]] says |
| `outbound`'s translate arm: `DecodeRequest`, the name, `EncodeRequest` | `b.Request(body, RequestOptions{Model: t.Model, Loss: droppedHeaders})`; the returned `loss` is the `Lux-Loss` header and the record's |
| `respondWhole`'s translate arm: `DecodeResponse`, the name, `EncodeResponse` | `b.Response(body, ResponseOptions{Model: c.model.Metadata.Name})`; the returned `usage` is the record's tokens |
| `streamTranslated`'s event loop | `b.Stream(c.w, resp.Body, ...)` after the gateway wrote the status and the `text/event-stream` header itself, when the upstream's headers arrived, as before the move, so the caller's time to first byte does not wait for the first event and a stream that fails on its first event still answers with them; no `FirstByte`, `Flush` the response controller's flush, and no `Fail`: `endStream` writes the door's frame as before, so there is one writer of the frame and never two |
| `responseEvents` | `b.StreamResponse(c.w, body, ...)` for a target that answers a stream request with one JSON body |
| `envelope`, `openaiError`, `anthropicError`, `geminiError`, `googleStatus` | `bridge.Envelope(wire, Failure{Code, Message, Detail, RequestID, Status, Domain: "lux"})` |
| `streamErrorFrame` | `bridge.ErrorFrame(wire, Failure{...})`, still written by `endStream`, which decides whether there is anyone left to write to |
| `modelList`, `modelEntry`, the four entry structs, `marshal` | `bridge.ModelList` and `bridge.ModelEntry` over `[]bridge.Model{{Name: n, OwnedBy: "lux"}}` |
| `usageParts`, `wireUsage`, `wireEnvelope`, `merge`, `fromIR`, `bodyUsage` | `bridge.UsageOf` and the `usage` the bridge's `Response` and `Stream` return |
| `sniffer`, `sseSniffer`, `jsonSniffer`, `frameEnd`, `frameData` | `bridge.NewUsageScanner(wire, FramingSSE\|FramingJSON)`, the same `io.Writer` beside the passthrough relay |
| `probe`, `probeBody` | `bridge.Probe`, returning `bridge.Call` |
| `rewriteModel`, `rewriteFrame`, `setIncludeUsage`, `removeMember`, and the JSON scanner under them (`member`, `skipValue`, `splice`, `insertMember`) | `bridge.SetModel`, `bridge.SetModelInFrame`, `bridge.SetIncludeUsage`, `bridge.RemoveMember` |
| `tokencount.Estimate` over the decoded request, in `estimate` and `estimatedTokens` | `bridge.CountTokensFor(dialect, body)` in the route's dialect and `bridge.CountBody(wire, n)` |

What stays, untouched: `forward.go`'s attempt order, retries, and
header policy; `handler.go`'s pipeline and its stages; `key.go`,
`route.go`, `router.go`, `upstream.go`; the `Code` table with its
statuses and fixed sentences; `Lux-*` header names and
`detailHeader`'s encoding; `writeFailure` and `WriteRefusal`;
`relayChunks`, `relayFrames`, and `streamPassthrough`; `Record`,
`Tokens`, and the metrics. `OpenAIReasoningFamily` stays: it is
[[008-routing-and-models]]'s predicate over a model name, and the
bridge carries no model table. `route.bridgeable` stays: which door
can reach which target is this repository's route rule.

`gateway.Tokens` is not replaced by `bridge.Usage`. The record's block
carries `Estimated` and is [[009-usage-and-metering]]'s shape; the
conversion is five lines in `record.go`.

### What the gateway still decides

- **The doors.** The path prefixes, the route table, the class per
  route, and which dialect each door speaks are [[004-request-path]]'s
  and stay here. The bridge is told a wire; it never reads a path.
- **The dialect pairing.** Whether an attempt is passthrough,
  translation, or the estimate, and whether a door can reach a target
  at all (`dialect_unsupported`), stays in `modeFor` and `bridgeable`.
- **The codec options.** `DefaultMaxTokens` from the Model,
  `DropSampling` false, `UseMaxCompletionTokens` from the reasoning
  family predicate: the values are computed here and passed in.
- **`Lux-Loss`** is set from `Request`'s second return, joined with
  commas, and omitted when it is nil. **`Lux-Estimated: true`** is set
  when `CountTokensFor` returns `estimated`. The names are this
  repository's; the values are the bridge's.
- **Every code, status, and sentence.** The bridge's seven failure
  codes are mapped here, in `bridgeFailure`, and nowhere else:
  `decode_request` is `invalid_request` with the bridge's detail,
  whatever the scope, as stage 8 says, and so is `encode_request`,
  because the same encode fails on every target of the dialect;
  `decode_response` and `encode_response` are `upstream_error`;
  `write_failed` is `client_closed`; `stream_failed` is classified as
  any other cut after the first byte, `client_closed` when the caller's
  context is done, `upstream_timeout` when the Provider's timeout
  passed, `upstream_error` otherwise; `unsupported` cannot reach a
  caller, because `bridgeable` refuses first, and is answered
  `dialect_unsupported` so that a bug in the route rule is still a
  fixed code rather than a panic.
- **`stream_options.include_usage`.** The rule stays a gateway rule;
  only the splice moves. The edit is made on the `/openai` door's
  `POST /v1/chat/completions` with `stream` true toward an `openai`
  target, and for one reason: a request whose tokens cannot be read is
  a request whose spend cannot be counted
  ([[009-usage-and-metering]]). That reason is metering, which the
  bridge knows nothing about, and the condition is a route, a door, and
  a target dialect, which are this repository's vocabulary. A bridge
  that injected it would be a bridge with an opinion about billing. So
  `forward.go` keeps the `if`, and calls `bridge.SetIncludeUsage` for
  the bytes.
- **The count route's member removal.** Same split: the rule that a
  `/lux` count re-encoded toward an `anthropic` target carries neither
  `max_tokens` nor `stream` stays here, in one line that calls
  `bridge.RemoveMember` twice.
- **The model name written back.** Which name the caller sees, the
  Model's, and which the provider sees, the target's upstream name, is
  [[008-routing-and-models]]'s; the gateway passes both in and the
  bridge writes them where each dialect keeps them.

### Decodes per request

Unchanged in count. Before the swap a translated request was decoded
twice, once in `decode` for the reservation's estimate and once per
attempt in `outbound`, so the loss report is that target's alone.
After it, `bridge.CountTokensFor` decodes for the estimate, once per
request and reused by a record whose upstream reported no usage, and
`b.Request` decodes per attempt: the same two, and `call.irReq` and
`call.decodeErr` are gone. The `ir` import stays: `bridge.Open` and
`bridge.CountTokensFor` take `ir.Dialect` names, and `doorDialect` and
`targetDialect` are the gateway's choice of codec per route and per
target. The estimate is read in the route's dialect for that reason:
the bridge's `CountTokens` reads a body through a wire's one frontend,
Chat Completions for `WireOpenAI`, which would have sent a
`/openai/v1/responses` body to the byte heuristic; `CountTokensFor`
was added to the bridge for this, and
`TestResponsesEstimateUsesItsOwnCodec` holds the route to it.

### The swap, commit by commit

1. Bump `latere.ai/x/pkg` to the commit that carries the bridge,
   `go get latere.ai/x/pkg@2e4ba0359ec37bdf30a753b029c914e35a5bb3fe &&
   go mod tidy`, which pins a pseudo-version until the tag exists.
2. `models.go` and its shapes deleted; `listModels` and `readModel`
   call `bridge.ModelList` and `bridge.ModelEntry`.
3. `usage.go` deleted; the sniffers become `bridge.UsageScanner` and
   `bodyUsage` becomes `bridge.UsageOf`; `fromIR`, which the two
   translate arms read until step 6, moves beside them for three
   commits and goes with them.
4. `probe.go` deleted; the probe and the four edits become the bridge's.
5. `errors.go` loses the shapes and the frame; `envelope` and
   `streamErrorFrame` become one call each.
6. `translate.go` and `stream.go` lose the codec pair, the event loop,
   and the re-emission; `outbound`, `respondWhole`, and
   `streamTranslated` call the bridge.
7. The unit tests that tested the moved internals directly
   (`usage_test.go`, `probe_test.go`, and the shape half of
   `errors_test.go` and `translate_test.go`) are deleted in the commit
   that deletes the code they test; their goldens are the bridge's
   `testdata`. The door-level tests are not touched: the four decode
   shapes `handler_test.go` reads a body back through move to
   `shapes_test.go`, a test helper, and `TestCodecOptionsFollowTheModel`
   holds the Model's `maxOutputTokens` to the codec's `max_tokens`
   through the handler in place of the unit test over the pair.

Each commit leaves the tree green. Nothing is kept behind a flag and no
forwarding function is left at an old name: the deleted code is deleted
in the commit that replaces it.

## Not in this spec

The bridge's own design, tests, and coverage floor, which are the
`latere.ai/x/pkg` module's. A Gemini codec: `WireGoogle` still has an
error shape, a list shape, a usage reading, and no translation, and the
`/gemini` door is still passthrough only. Any change to the route
table, the pipeline order, the error table, the record, the headers, or
the `/v1` API ([[004-request-path]], [[009-usage-and-metering]],
[[011-api]]). Any change to what a caller receives: this spec fails if
one byte moves.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The six behaviours the move is most likely to break are byte-identical after it: same-dialect passthrough, the loss report, the stream's last-value usage, the stream error frame per door, the model list shapes, and the count emulation, with the test sources and goldens unedited | `TestSameDialectSameBytes`, `TestTranslationReportsLoss`, `TestStreamUsageIsTheLastValue`, `TestStreamErrorFramePerDoor`, `TestModelsListShapes`, `TestCountTokensEmulation`, each passing unchanged | passing, unedited |
| Every other acceptance row of [[004-request-path]] still passes, the error envelope, the streaming, the model rewrite, and the refusal order among them | `gateway`'s suite, unedited but for the deletions of step 7 | passing; `TestCodecOptionsFollowTheModel` and `TestDialectsPerRouteAndTarget` added beside them |
| The conformance suite's `doors` group is green against a `luxd` built from the commit before the swap and from the commit after it, against the same stubs and the same fixtures | [[018-conformance-suite]]'s `doors` group, run twice in the migration's CI job | passing: `TestE2EConformance` green before the swap, at `cc7ef7e`, and after it, with an identical list of passing and skipped cases; the `conformance-twice` job of `verify.yml` runs it against the base commit and the head |
| `gateway`'s non-test line count across the six files falls by at least 700, and `models.go`, `usage.go`, and `probe.go` are gone | `TestGatewayCarriesNoCodecGlue`, which fails if any of the three files exists or the other three exceed 738 lines, in `internal/arch` | passing: the three are gone and the six fell from 1438 lines to 618 |
| `gateway`'s import list loses `llmdialect`, the four codec packages, `llmdialect/tokencount`, and `httpjson`, gains `llmdialect/bridge`, and keeps `llmdialect/ir`, whose `Dialect` names `Open` takes | `TestGatewayImports`, an explicit list compared against `go list -f '{{.Imports}}'` for the package itself, in `internal/arch` | passing |
| The allow list for `gateway` in [[001-architecture]]'s dependency test gains nothing: the bridge dials nothing and reaches no package that does | `TestRootPackagesDialNothing`, unchanged in its rule; the `gateway` row's comment names the bridge and loses the `llmjson` entry nothing ever reached | passing |
| `gateway`'s statement coverage stays at or above the 90% floor after the deletions | the coverage gate | passing |
| The bridge's seven failure codes each map to the code [[004-request-path]]'s table names, and a decode refusal is `invalid_request` whatever its `RefusalScope` | `TestBridgeFailuresMapToCodes`, table-driven over the seven, in `gateway` | passing |
