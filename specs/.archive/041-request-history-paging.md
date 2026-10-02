---
title: "Request history paging: a cursor that keeps its range, and archive pages of bounded reading"
status: complete
track: core
depends_on:
  - specs/009-usage-and-metering.md
  - specs/011-api.md
  - specs/012-request-log-and-events.md
affects: [internal/store/, internal/reqlog/, internal/api/, api/openapi.yaml, docs/api.md, specs/009-usage-and-metering.md, specs/011-api.md, specs/012-request-log-and-events.md]
effort: small
created: 2026-10-02
updated: 2026-10-02
author: changkun
---

# Request history paging

## Overview

`GET /v1/requests` pages its records by `cursor` and `next_cursor`. Two
things stop a client from paging a long range to its end. A cursor is
bound to the resolved range, and an end the caller left open is filled
from the clock on every request, so the second page of a query without
`to` is refused. And the archive reader walks every hour of the range
and reads every object in it until a page is full, so under a filter
that matches few records one page can read most of the archive. This
spec makes a cursor carry the range it was issued for, and bounds the
reading one archive page does.

## Current state

On 2026-10-02:

- `store.EncodeRecordCursor` (the ring) and `reqlog.encodeCursor` (the
  archive) each bind a cursor to an 8 hex digest of the record query
  with `from` and `to` resolved. `usageQuery` in `internal/api` fills an
  open `to` with now and an open `from` with `DefaultRange` before `to`
  on every request, so a second page without `to` resolves another
  range, its digest differs, and the page is `invalid_field` at
  `cursor`. `lux requests -since 24h` sends `from` alone and meets the
  same refusal on its second page.
- `reqlog.Reader.List` visits each UTC hour from the end of the range
  down to its start, one `ListObjects` per hour under each root (the
  prefix, and with partitions every partition), whether or not the
  hour holds an object: a 90 day range is 2 160 listings per root.
- Each object of a visited hour is read with `GetObject` and decoded
  whole before the filters apply. The authorizer's filter narrows the
  query to an owner or a label pair, and only a label pair on the
  partition label narrows what is read; any other filter is applied
  record by record after the read.
- A page ends when it holds `limit` records or the range ends, so the
  page that crosses a stretch where the filter matches nothing, or the
  last page of the range, reads everything between. Against a bucket
  with 20 milliseconds per call, an archive of 20 days at two objects
  every ten minutes, and an owner who appears once per object in the
  newest two days, the first two pages of 200 take 4.8 seconds each
  and the third reads 2 532 objects and 212 listings before a 60 second
  deadline ends it.

## Design

### The cursor keeps its range

Both record cursors share one framing in `internal/store`:

```
base64url("<kind>|<digest>|<from>|<to>|<position>")
```

`<from>` and `<to>` are the resolved range as Unix nanoseconds, and
`<digest>` stays the 8 hex digest of the record query with that range.
`<position>` is the kind's: `<at>|<id>` for the ring (`records`), and
`<hour>|<line>|<key>` for the archive (`archive`), the key last so a
`|` in a configured prefix cannot split it.
`store.EncodeQueryCursor`, `store.DecodeQueryCursor`, and
`store.QueryCursorWindow` are the framing; the archive's own digest
copy is removed.

`GET /v1/requests` with a `cursor` fills an end of the range the
caller left open from the cursor's range before the defaults of
[[009-usage-and-metering]] apply. A follow-up page that repeats the
first page's parameters, or sends the cursor alone, therefore resolves
the same query and is answered; a `from`, `to`, or filter that differs
from the first page's resolves another query and stays
`invalid_field` at `cursor`. Cursors minted before this change do not
decode and are `invalid_field` at `cursor`; a client starts again from
the first page.

### Archive pages read a bounded amount

The reader walks only the hours that hold objects: for each month of
the range, newest first, one delimited `ListObjects` per root names the
days present; for each present day one names the hours present; for
each present hour the objects are listed and read, keys in ascending
order across the roots, as before. An empty stretch costs a listing
per month and root rather than one per hour.

A page with a positive `limit` makes at most `reqlog.PageReads` bucket
calls, listings and reads together, counted from its first call. When
the count is spent, the page ends at the next point where the walk
has moved past the position it resumed from, and answers the records it
holds, which may be fewer than `limit` and may be none, with a
`next_cursor` at the position reached. A position names an hour and,
within it, an object key and the lines of it already read, an empty
key being the hour's start, so a page can end between objects, between
hours, or inside a stretch with no objects. Every page costs about the
same number of calls whatever its depth, and a filter that matches
few records costs pages rather than time.

The contract of [[011-api]] already lets a page be shorter than
`limit` while `next_cursor` is present; `GET /v1/requests` documents
that it may be empty, and a client follows `next_cursor` until it is
absent, which `client.List` and `lux requests` do.

## Not in this spec

- An index of the archive by owner or label, which would let a page
  skip objects without reading them; partitions by a Key label
  ([[012-request-log-and-events]]) remain the way to narrow the read.
- Reading several objects of a page at once, which trades the wait for
  memory held per request.
- The ring source's paging cost, which is bounded by
  `metering.RecordsPerKey` per Key.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | Over the ring, a query with `from` and no `to` pages to its end across a clock tick between pages, with every record once and in order; the same with `to` on every page; the cursor alone resumes; and a cursor with another `model`, `from`, or `to` is `invalid_field` at `cursor` | `internal/api` test over the memory store |
| 2 | The same four over the archive, with a `reqlog.Reader` over `s3test` as the route's source | `internal/api` test |
| 3 | The ring and the archive cursor carry the range, decode only under their own kind and query, and refuse a malformed cursor, a key outside the prefix, an hour outside the range, and a key outside its hour | `internal/store` and `internal/reqlog` tests |
| 4 | Over an archive of hundreds of objects in a 90 day range where the filter matches only the newest objects and one two months older, every page makes at most `PageReads` calls and one more for each further root a level is listed under, the pages together answer every matching record once in the unpaged order, a page may hold none while a cursor remains, and no listing names a day or an hour that holds no object | `internal/reqlog` test with a counting bucket |
| 5 | The `listRequests` operation says a page may hold fewer records than `limit`, or none, while `next_cursor` is present | `TestOpenAPIIsCurrent` |

## Outcome

Built and verified on 2026-10-02 as designed, with `PageReads` at 64.

| # | Test |
|---|---|
| 1 | `TestRequestsCursorKeepsItsRange/memory`, `internal/api`: `from` alone with the clock a minute later on every page, `to` on every page, the cursor alone, and a cursor sent with `status` dropped or changed, another `from`, or another `to`. Without the fill from the cursor, the second page is refused with the detail `cursor query f5d32f65, the request's 67ae9f16` |
| 2 | `TestRequestsCursorKeepsItsRange/archive`, the same over a `reqlog.Reader` on `s3test` holding two objects |
| 3 | `TestRecordCursorBindsTheQuery` and `TestQueryCursorWindow`, `internal/store`; `TestArchiveReaderPages`, `internal/reqlog`, with a short position, a bad or negative line, an hour that is not one, a line of no object, a key outside the prefix, without an hour, or in another hour, and an hour outside the range |
| 4 | `TestArchivePagesReadABoundedAmount`, `internal/reqlog`, 577 objects with one root and with three partitions beside the unpartitioned prefix. Against the reader before this spec its first page made 2 713 calls with one root and 11 258 with partitions |
| 5 | `TestOpenAPIIsCurrent` and `TestOpenAPINavigationLabels`, `internal/api` |

Measured with a bucket that waits 20 milliseconds per call, a 90 day
range, `limit` 200, and an archive of two replicas writing an object
every ten minutes each:

| Archive and filter | Before | After |
|---|---|---|
| 20 days of objects, an owner once per object in the newest two days | pages 1 and 2: 4.8 s; page 3: stopped at a 60 s deadline after 2 532 reads and 212 listings | every page 1.40 to 1.43 s, 64 calls; 56 records a page while the owner appears, then pages of none; 103 pages to the end |
| 20 days of objects, no filter | 0.26 to 0.29 s a page of 200, 2 listings | 0.31 to 0.33 s a page of 200, 4 listings |
| 2 days of objects, no filter | pages of 200 in 0.28 s; the last page 46.1 s, 2 089 listings | pages of 200 in 0.33 s; the last page 0.27 s, 5 listings |

Divergences and findings:

- A level of the walk is listed under every root that holds it before
  the count is checked again, and the calls that find the position a
  page resumes from come before it can end, so a page with several
  roots makes at most `PageReads` calls and one more for each further
  root once past its start.
- A truncated delimited listing continued from the name of its last
  common prefix, which lists the keys under that prefix again and rolls
  them up into the same prefix. The partition listing had this flaw
  before this spec, and the new month and day listings shared it; the
  test over listings of two entries a page found it. A listing now
  continues after the prefix with its trailing delimiter raised by one
  byte.
- A page resumes with a listing of its month and of its day before the
  listing of its hour, two calls more than before, which is the 40
  milliseconds the unfiltered pages above gained.
- A page that fills `limit` at the last line of an object resumes in
  that object and reads it once more to find no line left, as before.
- A `List` with no `limit`, which the route never sends, reads to the
  end of the range without the bound.
