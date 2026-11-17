#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Generates docs/adr/README.md's index table, docs/adr/adrs.json and docs/adr/graph.md from the ADR front
# matter. Nobody edits them by hand, so two pull requests that each add an ADR no longer conflict on them
# (they still conflict on a number: reserve it with `hack/claim.sh adr`).
# The logic is Go: hack/cmd/adr (ADR 0118), a main-module tool that `go tool` runs, so its exit codes pass
# through (0 clean, 1 findings, 2 usage or read error). It reads files only and works offline.
#
# Usage: hack/adr-index.sh [--check] [adr-dir]    (default: write; --check exits 1 when a file is stale)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
check=()
dir=
for arg in "$@"; do
  case $arg in
    --check) check=(--check) ;;
    -*) echo "adr-index: unknown option $arg" >&2; exit 2 ;;
    *) [ -z "$dir" ] || { echo "adr-index: only one adr-dir is allowed" >&2; exit 2; }; dir=$arg ;;
  esac
done
if [ -n "$dir" ]; then
  [ -d "$dir" ] || { echo "adr-index: '$dir' is not a directory" >&2; exit 2; }
  dir=$(cd "$dir" && pwd)
fi
# `go tool` finds the tool through the module of the working directory.
cd "$repo"
export ADR_ROOT=$repo
# KOINON_TEST_ADR_BIN names a built hack/cmd/adr binary (hack/ship-test.sh runs the real tool in a repository without this module).
if [ -n "${KOINON_TEST_ADR_BIN:-}" ]; then exec "$KOINON_TEST_ADR_BIN" index ${check[@]+"${check[@]}"} ${dir:+"$dir"}; fi
exec go tool adr index ${check[@]+"${check[@]}"} ${dir:+"$dir"}
