#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Agent instruction files lint (issue 1045, docs/agents/agents-md.md): no CLAUDE.md in the tree, the root
# AGENTS.md indexes exactly the nested ones, line and byte budgets, a first line that names the root,
# every make target and repo path an AGENTS.md names exists, no rule in two files. The logic is Go:
# hack/cmd/agentsmdlint (ADR 0118), a main-module tool that `go tool` runs, so its exit codes pass
# through (0 clean, 1 findings, 2 usage or read error).
#
# Usage: hack/agentsmd-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
root=${1:-$repo}
[ -d "$root" ] || { echo "agentsmd-lint: ROOT '$root' does not exist or is not a directory" >&2; exit 2; }
root=$(cd "$root" && pwd)
# `go tool` finds the tool through the module of the working directory.
cd "$repo"
exec go tool agentsmdlint "$root"
