---
title: "Translation through llmdialect: the codec glue leaves the gateway for an importable bridge"
status: dispatched
track: core
depends_on:
  - specs/004-request-path.md
  - specs/018-conformance-suite.md
affects: [gateway/, test/conformance/]
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

`gateway` is [[004-request-path]]'s handler, complete and at 96%
statement coverage. The six files that hold the layer:

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

The bridge is drafted but not released. This spec waits for a tagged
`latere.ai/x/pkg/llmdialect/bridge` whose surface is the one below; it
is not dispatchable before that tag exists, and that dependency is not
in `depends_on`, which names specs of this repository only.

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
bridge.CountTokens(wire, body) (n int64, estimated bool, err error)
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
| `streamTranslated`'s event loop | `b.Stream(c.w, resp.Body, ...)` with `FirstByte` writing the `text/event-stream` header and the status, `Flush` the response controller's flush, and `Fail` the gateway's failure mapped to a `bridge.Failure` |
| `responseEvents` | `b.StreamResponse(c.w, body, ...)` for a target that answers a stream request with one JSON body |
| `envelope`, `openaiError`, `anthropicError`, `geminiError`, `googleStatus` | `bridge.Envelope(wire, Failure{Code, Message, Detail, RequestID, Status, Domain: "lux"})` |
| `streamErrorFrame` | `bridge.ErrorFrame(wire, Failure{...})`, still written by `endStream`, which decides whether there is anyone left to write to |
| `modelList`, `modelEntry`, the four entry structs, `marshal` | `bridge.ModelList` and `bridge.ModelEntry` over `[]bridge.Model{{Name: n, OwnedBy: "lux"}}` |
| `usageParts`, `wireUsage`, `wireEnvelope`, `merge`, `fromIR`, `bodyUsage` | `bridge.UsageOf` and the `usage` the bridge's `Response` and `Stream` return |
| `sniffer`, `sseSniffer`, `jsonSniffer`, `frameEnd`, `frameData` | `bridge.NewUsageScanner(wire, FramingSSE\|FramingJSON)`, the same `io.Writer` beside the passthrough relay |
| `probe`, `probeBody` | `bridge.Probe`, returning `bridge.Call` |
| `rewriteModel`, `rewriteFrame`, `setIncludeUsage`, `removeMember`, and the JSON scanner under them (`member`, `skipValue`, `splice`, `insertMember`) | `bridge.SetModel`, `bridge.SetModelInFrame`, `bridge.SetIncludeUsage`, `bridge.RemoveMember` |
| `tokencount.Estimate` over the decoded request, in `estimate` and `estimatedTokens` | `bridge.CountTokens(wire, body)` and `bridge.CountBody(wire, n)` |

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
  when `CountTokens` returns `estimated`. The names are this
  repository's; the values are the bridge's.
- **Every code, status, and sentence.** The bridge's seven failure
  codes are mapped here, and nowhere else: `decode_request` is
  `invalid_request` with the bridge's detail, whatever the scope, as
  stage 8 says; `decode_response` and `encode_response` are
  `upstream_error`; `write_failed` is `client_closed`; `stream_failed`
  is `upstream_error` unless the caller's context is already done;
  `unsupported` cannot reach a caller, because `bridgeable` refuses
  first, and is a bug if it does.
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

Unchanged. Today a translated request is decoded twice, once in
`decode` for the reservation's estimate and once per attempt in
`outbound`, so the loss report is that target's alone. After the swap,
`bridge.CountTokens` decodes for the estimate and `b.Request` decodes
per attempt: the same two, and `call.irReq` and `call.decodeErr`
disappear with the `ir` import.

### The swap, commit by commit

1. Bump `latere.ai/x/pkg` to the tag that carries the bridge.
2. `models.go` and its shapes deleted; `listModels` and `readModel`
   call `bridge.ModelList` and `bridge.ModelEntry`.
3. `usage.go` deleted; the sniffers become `bridge.UsageScanner` and
   `bodyUsage` becomes `bridge.UsageOf`.
4. `probe.go` deleted; the probe and the four edits become the bridge's.
5. `errors.go` loses the shapes and the frame; `envelope` and
   `streamErrorFrame` become one call each.
6. `translate.go` and `stream.go` lose the codec pair, the event loop,
   and the re-emission; `outbound`, `respondWhole`, and
   `streamTranslated` call the bridge.
7. The unit tests that tested the moved internals directly
   (`usage_test.go`, `probe_test.go`, and the shape half of
   `errors_test.go` and `translate_test.go`) are deleted in the commit
   that deletes the code they test; their goldens go to the bridge's
   `testdata` in the same batch. The door-level tests are not touched.

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
| The six behaviours the move is most likely to break are byte-identical after it: same-dialect passthrough, the loss report, the stream's last-value usage, the stream error frame per door, the model list shapes, and the count emulation, with the test sources and goldens unedited | `TestSameDialectSameBytes`, `TestTranslationReportsLoss`, `TestStreamUsageIsTheLastValue`, `TestStreamErrorFramePerDoor`, `TestModelsListShapes`, `TestCountTokensEmulation`, each passing unchanged | not built |
| Every other acceptance row of [[004-request-path]] still passes, the error envelope, the streaming, the model rewrite, and the refusal order among them | `gateway`'s suite, unedited but for the deletions of step 7 | not built |
| The conformance suite's `doors` group is green against a `luxd` built from the commit before the swap and from the commit after it, against the same stubs and the same fixtures | [[018-conformance-suite]]'s `doors` group, run twice in the migration's CI job | not built |
| `gateway`'s non-test line count across the six files falls by at least 700, and `models.go`, `usage.go`, and `probe.go` are gone | `TestGatewayCarriesNoCodecGlue`, which fails if any of the three files exists | not built |
| `gateway`'s import list loses `llmdialect`, `llmdialect/ir`, the four codec packages, `llmdialect/tokencount`, and `httpjson`, and gains `llmdialect/bridge` alone | `TestGatewayImports`, an explicit list compared against `go list -deps` for the package itself | not built |
| The allow list for `gateway` in [[001-architecture]]'s dependency test gains nothing: the bridge dials nothing and reaches no package that does | `TestRootPackagesDialNothing`, unchanged | not built |
| `gateway`'s statement coverage stays at or above the 90% floor after the deletions | the coverage gate | not built |
| The bridge's seven failure codes each map to the code [[004-request-path]]'s table names, and a decode refusal is `invalid_request` whatever its `RefusalScope` | `TestBridgeFailuresMapToCodes`, table-driven over the seven | not built |
