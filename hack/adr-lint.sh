#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# ADR lint (ADR 0046, issue 283): numbers unique, the YAML front matter of every ADR valid (status, date,
# area, tags, decides, amends, supersedes, relates), the prose header lines in step with it, links both
# ways ("amended by" is derived), no editor-only syntax (`[[`, callouts, Dataview), and README.md's index,
# adrs.json and graph.md current. An ADR still Proposed is a warning.
# The logic is Go: hack/cmd/adr (ADR 0118), a main-module tool that `go tool` runs, so its exit codes pass
# through (0 clean, 1 findings, 2 usage or read error). It reads files only and works offline.
#
# Usage: hack/adr-lint.sh [adr-dir]    (default docs/adr)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
dir=
for arg in "$@"; do
  case $arg in
    -*) echo "adr-lint: unknown option $arg" >&2; exit 2 ;;
    *) [ -z "$dir" ] || { echo "adr-lint: only one adr-dir is allowed" >&2; exit 2; }; dir=$arg ;;
  esac
done
if [ -n "$dir" ]; then
  [ -d "$dir" ] || { echo "adr-lint: '$dir' is not a directory" >&2; exit 2; }
  dir=$(cd "$dir" && pwd)
fi
# `go tool` finds the tool through the module of the working directory.
cd "$repo"
export ADR_ROOT=$repo
if [ -n "${ADR_BIN:-}" ]; then exec "$ADR_BIN" lint ${dir:+"$dir"}; fi
exec go tool adr lint ${dir:+"$dir"}
