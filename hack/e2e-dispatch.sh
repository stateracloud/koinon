#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): a dispatch needs write access to the repository it acts on.
#
# Dispatches one run of .github/workflows/e2e-kind.yml, finds that run and prints `run=<id> url=<url>`
# (issue 1488). Use it instead of `gh workflow run ... && sleep && gh run list`: the sleep hook refuses
# that chain, and `gh run list` can return another person's run. Wait for the run in a background
# task, never with a foreground sleep. The logic is Go:
# hack/cmd/e2edispatch (ADR 0118), a main-module tool that `go tool` runs, so its exit code passes
# through (0 found; 1 no single run in time, or two new runs that it cannot tell apart, listed on
# stderr; 2 usage error or a gh call failed). GH overrides the gh command.
#
# Usage: hack/e2e-dispatch.sh --ref REF [--suite go|web|drill] [--backup-restore] [--stories]
#                             [--commit SHA --bisect-id ID] [--timeout 2m] [--dry-run]

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
# `go tool` finds the tool through the module of the working directory. No argument is a path.
cd "$repo"
exec go tool e2edispatch "$@"
