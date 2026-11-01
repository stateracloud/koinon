#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The required check `web` aggregates the parallel web-* jobs of ci.yml (#218). Fails when a web-*
# job is missing from the gate's needs, or the gate names a job that does not exist.
set -euo pipefail
wf="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/ci.yml"

jobs=$(sed -n 's/^  \(web-[a-z0-9-]*\):$/\1/p' "$wf" | sort)
needs=$(sed -n '/^  web:$/,/^  [a-z-]*:$/p' "$wf" | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' | tr ',' '\n' | tr -d ' ' | grep '^web-' | sort || true)

[[ -n $jobs ]] || { echo "FAIL: no web-* jobs in ci.yml"; exit 1; }
if [[ $jobs != "$needs" ]]; then
  echo "FAIL: web-* jobs and the needs of the web gate differ"
  echo "jobs:"; echo "$jobs"; echo "needs:"; echo "$needs"
  exit 1
fi
# shellcheck disable=SC2016 # literal GitHub expression, not a shell expansion
grep -qF '    if: ${{ !cancelled() }}' <(sed -n '/^  web:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: web gate must run if: !cancelled() (runs when a part fails, not on a superseded run)"; exit 1; }
echo "ok: web gate covers $(echo "$jobs" | tr '\n' ' ')"
