#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# bash 5 guard lint (ADR 0090, old issue 553): every `#!/usr/bin/env bash` script carries the guard
# line as its first command. The logic is Go: hack/cmd/bash5lint (ADR 0118), a main-module tool that
# `go tool` runs, so its exit codes pass through (0 clean, 1 findings, 2 usage or read error). This
# file keeps the old path and command line, so Make, preflight and CI do not change.
#
# Usage: hack/bash5-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
root=${1:-$repo}
[ -d "$root" ] || { echo "bash5-lint: ROOT '$root' does not exist or is not a directory" >&2; exit 2; }
root=$(cd "$root" && pwd)
# `go tool` finds the tool through the module of the working directory.
cd "$repo"
exec go tool bash5lint "$root"
