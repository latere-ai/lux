#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Runs the fenced blocks of a Markdown document in order, so a command
# that stopped working fails a build rather than an operator (spec 017):
#
#	tools/docs/run-blocks.sh docs/install.md          # write the files, run the sh blocks
#	tools/docs/run-blocks.sh --write docs/install.md  # write the files and run nothing
#
# Two kinds of block take part. A block fenced ```sh is a step and runs
# under bash -euo pipefail in the current directory with the caller's
# environment, every block one script so a variable one block exports the
# next one reads. A block whose fence names a file, ```yaml file=NAME, is
# written to that file before any step runs, so a document shows an
# operator the file and hands the runner the same bytes. Every other
# block is prose and is skipped. The runner prints each step's number
# and line before it runs, so a failure names its block.
set -euo pipefail

write_only=false
if [ "${1:-}" = "--write" ]; then
  write_only=true
  shift
fi
doc=${1:?usage: run-blocks.sh [--write] DOCUMENT}
[ -r "$doc" ] || { echo "run-blocks: $doc is not readable" >&2; exit 1; }

script=$(mktemp)
trap 'rm -f "$script"' EXIT

# Pass one: the named files.
awk -v doc="$doc" '
  /^```[a-z]+ file=[^ ]+$/ { file=$0; sub(/^```[a-z]+ file=/, "", file); infile=1; content=""; next }
  /^```$/ && infile { printf "%s", content > file; close(file); printf "run-blocks: wrote %s (%s:%d)\n", file, doc, NR; infile=0; next }
  infile { content = content $0 "\n" }
' "$doc"

# Pass two: the steps, as one script whose markers name each block.
awk -v doc="$doc" '
  /^```sh$/ { inblock=1; n++; printf "echo \"run-blocks: step %d (%s:%d)\"\n", n, doc, NR; next }
  /^```$/ && inblock { inblock=0; next }
  inblock { print }
  END { if (n == 0) { print "echo \"run-blocks: " doc " has no sh block\" >&2; exit 1" } }
' "$doc" > "$script"

steps=$(grep -c '^echo "run-blocks: step ' "$script" || true)
echo "run-blocks: $steps step(s) in $doc"
if $write_only; then
  exit 0
fi
bash -euo pipefail "$script"
