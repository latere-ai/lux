---
title: "Request history paging: a cursor that keeps its range, and archive pages of bounded reading"
status: validated
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
| 4 | Over an archive of thousands of objects in a 90 day range where the filter matches only the newest objects, every page makes at most `PageReads` calls beyond those that pass its starting position, the pages together answer every matching record once in the unpaged order, and a stretch of empty months and days costs no listing per hour | `internal/reqlog` test with a counting bucket |
| 5 | The `listRequests` operation says a page may hold fewer records than `limit`, or none, while `next_cursor` is present | `TestOpenAPIIsCurrent` |
