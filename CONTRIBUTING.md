# Contributing

Thanks for looking. This file is for people and agents changing Lux.
Users read the [README](README.md) and [`docs/`](docs/README.md); the
design and the reasoning behind it live in [`specs/`](specs/README.md).

## Getting set up

You need Go 1.27 or newer and `git`. Then:

```sh
make run   # luxd on loopback, every state in memory
make       # the quality gate
```

`make` needs only the Go toolchain and git. Everything it pins comes from
public modules, so it runs the same on your machine as in CI.

Install the hooks once with `make hooks`. They run formatting and license
checks before a commit and the linter before a push, so you see a finding
before CI does.

## Sending a change

Push to `main`. Keep one logical change per commit, stage the files
explicitly, and write the subject in the imperative, saying what changed
for whoever reads the log. Run the gate before you push (`make`, which is
`go tool lateregate`); the pre-push hook then runs the linter over the
packages the push changes, so a finding reaches you before CI does. Batch
a series of commits and push once, because one push is one CI run.

There are no pull requests here. The organization does not let an Action
open one, and a change lands on `main` rather than waiting on a review
branch. A release is `go tool lateregate release vX.Y.Z`, cut from a
green CI run of the commit being tagged: the command reads CI before it
runs the quality bar and refuses while the repository is red.

If you are planning something large, open an issue first. A design that
lands without a spec is harder to review than one that arrives with the
reasoning attached.

## The bar

`make` runs the whole gate (`go tool lateregate`): formatting, the
linter, modernization, known vulnerabilities, the suite with and without
the race detector, per-package coverage at 90% or more, the suite with
only the toolchain on `PATH`, the suite against an empty temporary
directory, the license notice, the dependency allow list, and the spec
tree. `go tool lateregate list` names the gates and
`go tool lateregate <name>` runs one.

A bug fix carries a test that fails without it. A change that lowers a
threshold or adds a waiver records the reason in `.lateregate.yaml`, so
the exception is reviewable rather than invisible.

## Specs first

A feature starts as a spec with acceptance criteria that are testable
sentences. The implementation follows the spec, and a divergence is
recorded in the spec's Outcome section rather than left in the code.
Names in a spec (manifest fields, error codes, environment variables,
metrics) are the names the code uses.

Small fixes do not need a spec. Anything that changes the manifest
schema, the `/v1` API, a webhook or event payload, or a configuration
variable does.

## Where a package belongs

Three places, by who imports it:

- The module root (`manifest/`, `gateway/`, `metering/`, `client/`, `authorizer/`) holds the
  packages a platform built on Lux imports. A change there keeps existing
  call sites compiling or names the break in the CHANGELOG.
- `internal/` holds what only `luxd` needs: the HTTP API, identity,
  configuration, the stores.
- A generic package with a plausible second consumer outside Lux
  belongs in [`latere.ai/x/pkg`](https://github.com/latere-ai/pkg), the
  shared library Lux already depends on. If you are unsure, put it in
  `internal/` and say so in the commit message.

## Three registers

Every sentence is written for one reader, and the register follows the
reader: the user in API `message` fields and the `lux` command's output,
the contributor in specs, this file, package documentation, and commit
messages, the developer in logs, `/readyz`, and error details. The rule
and the review checklist are in
[`docs/writing/registers.md`](https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md).

## Reporting a vulnerability

Do not open an issue. [`SECURITY.md`](SECURITY.md) says where to send it.
