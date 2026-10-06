---
title: "A provider's refusal reason: the upstream status on the request log line, and every credential redacted from the developer detail"
status: complete
track: core
depends_on:
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/019-observability.md
affects: [gateway/, internal/arch/, cmd/luxd/, docs/, specs/004-request-path.md, specs/019-observability.md]
effort: small
created: 2026-10-06
updated: 2026-10-06
author: changkun
---

# A provider's refusal reason

## Overview

A provider that refuses a request with a `4xx` gives its reason in the
body of its answer: a model that does not support a parameter, an
endpoint no routing preference admits, a malformed field. Lux answers
the caller `upstream_rejected` with one fixed sentence, and the reason
travels apart as the developer detail, the upstream status and the
first KiB of the body in `Lux-Error-Detail`, exactly as a `429` or a
`5xx` carries it. Two gaps made such a refusal hard to read. The
operator's one line per request, `lux.request`, has no upstream status,
so a log query for the provider's status reads nothing and a refusal
by the provider looks the same as one by Lux. And the excerpt is the
upstream's bytes as they arrived: a provider that echoes the credential
it was sent, or any other credential-shaped string, puts it into a
header the caller receives.

## Current state

On 2026-10-06:

- `gateway/forward.go`'s `attempt` answers every upstream error status
  through one helper, `upstreamDetail`: a retryable status
  (`upstream_error`), a redirect, `401`, or `403` (`upstream_error`),
  and any other `4xx` (`upstream_rejected`). The helper reads the first
  1024 bytes of the body and prefixes `upstream status N: `; it redacts
  nothing. `writeFailure` sends the detail in `Lux-Error-Detail`, cut to
  one line of `maxDetailBytes`, 1024, and the lux door also in
  `details.detail`.
- `TestUpstreamStatusMapping` already holds `upstream status 400` in the
  header of a `400`, so a `4xx` has carried its reason since the doors
  were built.
- `Record.UpstreamStatus` is the last attempt's status and reaches the
  usage record, but `logRequest` in `gateway/trace.go` writes no field
  for it, so the line has none.
- The discovery and health jobs of [[005-providers]] already redact the
  Provider's credential from the excerpt they keep in `status`, with
  `[redacted]`.

## Design

### The log line

The data plane's line of [[019-observability]] gains `upstream_status`,
an integer: `Record.UpstreamStatus`, the last attempt's HTTP status, and
`0` when no provider answered. It sits after `code`, and with it an
operator tells a provider's refusal (`upstream_rejected` and a `4xx`)
from a provider's failure (`upstream_error` and a `5xx` or `429`) and
both from a refusal Lux made before any attempt (`0`). The line carries
no body, so the reason itself is not on it; the caller has it, and the
usage record of [[009-usage-and-metering]] has the status already.

### The detail

`upstreamDetail` takes the credential value `inject` wrote on the
attempt, nil for none, and builds the detail in four steps:

1. It reads `maxDetailBytes` plus the larger of the credential's length
   and `maxDetailBytes`, so a credential that starts inside the first
   KiB is read whole.
2. It replaces every occurrence of the credential with `[redacted]`, the
   jobs' marker.
3. It passes the result through `latere.ai/x/pkg/audit.Redact`, which
   replaces credential-shaped strings, a bearer token in an
   `Authorization` line, a `*_KEY=`-style assignment, basic auth in a
   URL, a JWT, an AWS or GitHub key, an `sk-` key, with `***`.
4. It cuts the excerpt to `maxDetailBytes`, after the redaction, so no
   prefix of a credential is left at the cut.

A body that fails mid-read keeps what arrived, and the read error is
named before it: `upstream status 400 (reading the body: <error>):
<excerpt>`. The one constant `maxDetailBytes` bounds both the excerpt
and the header; the separate `maxUpstreamDetail` of the same value is
removed.

Every status the attempt answers goes through the one helper, so the
`4xx` path and the `429` path redact alike. The caller's body is
unchanged: the fixed sentence of [[011-api]]'s table, the detail only in
`Lux-Error-Detail` and the lux door's `details.detail`.

`latere.ai/x/pkg/audit` imports the standard library alone; the import
rules of `internal/arch` admit it for `gateway` with that reason.

## Not in this spec

- The reason on the log line. [[019-observability]] keeps every body,
  an upstream's included, out of the log.
- An opaque route's answer, which is relayed to the caller whole and
  unread, because the caller speaks the provider's API directly
  ([[004-request-path]]).
- How a consumer shows the detail. A consumer that reads
  `Lux-Error-Detail` on a `429` or a `5xx` reads it on a `4xx` the same
  way.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | A provider's `400` with a JSON body, on a whole answer and on a stream, is `upstream_rejected` with `Lux-Error-Detail` equal to `upstream status 400: ` and the body, cut to 1024 bytes, and the body is not in the caller's body | `TestProviderRefusalReasonReachesTheCaller` |
| 2 | The lux door's `details.detail` is the status and the body's first 1024 bytes | `TestProviderReasonOnTheLuxDoor` |
| 3 | The request's log line carries `upstream_status` `400`, and every line carries the field, `0` when no provider answered | `TestProviderRefusalReasonReachesTheCaller`, `TestRequestLineFields`, `cmd/luxd`'s `TestLogFieldsAreTheTable` |
| 4 | An upstream that echoes the credential it was sent, on a `400` and on a `429`, has it replaced by `[redacted]`, and an `sk-` key that is not the Provider's by `***`; the reason survives | `TestProviderEchoIsRedacted` |
| 5 | A credential that crosses the 1024-byte cut leaves no prefix of itself | `TestCredentialAcrossTheCutIsRedacted` |
| 6 | A body that fails mid-read names the error before what arrived | `TestUnreadableBodyIsNamed` |
| 7 | `gateway` imports `latere.ai/x/pkg/audit` and nothing else new | `TestGatewayImports`, `TestRootPackagesDialNothing` |

## Outcome

Built and verified on 2026-10-06 as designed.

| # | Test |
|---|---|
| 1 | `TestProviderRefusalReasonReachesTheCaller`, stream and not |
| 2 | `TestProviderReasonOnTheLuxDoor` |
| 3 | `TestProviderRefusalReasonReachesTheCaller`; `TestRequestLineFields`, whose field row gains `upstream_status`, `200` on a served request and `0` on a refusal; `TestLogFieldsAreTheTable` |
| 4 | `TestProviderEchoIsRedacted` |
| 5 | `TestCredentialAcrossTheCutIsRedacted`, at three offsets, the last a credential that ends exactly at the cut |
| 6 | `TestUnreadableBodyIsNamed` |
| 7 | `TestGatewayImports` and the `gateway` row of `TestRootPackagesDialNothing` |

The header half needed no change. The `4xx` branch has called the same
helper as the `429` branch since the doors were built, and
`TestProviderRefusalReasonReachesTheCaller`'s header assertion passes
against the code before this spec; what fails there without the change
is the log line's `upstream_status`. A consumer that saw only the fixed
sentence for a provider's `400` had the reason in `Lux-Error-Detail`
and did not show it.

Without the change, criteria 3 to 6 fail: the line has no
`upstream_status`, an echoed credential reaches the header in full, a
credential across the cut leaves its prefix, and a read error is
dropped.
