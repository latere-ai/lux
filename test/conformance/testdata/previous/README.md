# The previous-release fixtures

One directory per tagged release, named after the tag, holding what
that release's conformance run produced: one resolved manifest per kind
as `provider.json`, `budget.json`, `model.json`, and `key.json`, read
back through `GET /v1/{kind}s/{name}`, and the run's request records as
`records.ndjson`, one `metering.Record` per line.

The `fixture` group of the conformance suite reads every directory
here on every push: it applies each manifest under the run's prefix and
holds the read-back's `spec` to the fixture's, and decodes each record
with the current `metering.Record` and holds every member to the same
value after a round trip. That is the schema evolution promise of the
manifest contract and the record additivity promise of the usage record
executed rather than asserted: a field that changed type, a default
that changed, or a member that was removed fails here and nowhere else.

The release pipeline writes the next directory: after a tag's release
exists, its `fixture` job pushes the branch `conformance/fixture-<tag>`
adding `<tag>/` here, and attaches the same bytes to the release as
`fixture-<tag>.tar.gz`. The job opens no pull request, because the
GitHub organisation forbids an Action from creating one; the maintainer
merges the branch. Nothing is written by hand, old directories are
kept, and until the first tag the group skips saying this directory
holds no version.
