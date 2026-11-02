#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The required check `go` aggregates the parallel go-* jobs of ci.yml (#217). Fails when a go-*
# job is missing from the gate's needs, or the gate names a job that does not exist.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
wf="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/ci.yml"

jobs=$(sed -n 's/^  \(go-[a-z0-9-]*\):$/\1/p' "$wf" | sort)
needs=$(sed -n '/^  go:$/,/^  [a-z-]*:$/p' "$wf" | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' | tr ',' '\n' | tr -d ' ' | grep '^go-' | sort || true)

[[ -n $jobs ]] || { echo "FAIL: no go-* jobs in ci.yml"; exit 1; }
if [[ $jobs != "$needs" ]]; then
  echo "FAIL: go-* jobs and the needs of the go gate differ"
  echo "jobs:"; echo "$jobs"; echo "needs:"; echo "$needs"
  exit 1
fi
# shellcheck disable=SC2016 # literal GitHub expression, not a shell expansion
grep -qF '    if: ${{ !cancelled() }}' <(sed -n '/^  go:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go gate must run if: !cancelled() (runs when a part fails, not on a superseded run)"; exit 1; }
echo "ok: go gate covers $(echo "$jobs" | tr '\n' ' ')"
