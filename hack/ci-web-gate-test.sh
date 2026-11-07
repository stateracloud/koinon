#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The required check `web` aggregates the parallel web-* jobs of ci.yml (old issue 218). Fails when a web-*
# job is missing from the gate's needs, or the gate names a job that does not exist.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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
grep -qF '    if: ${{ !cancelled() && !(github.event_name == '"'push'"' && needs.changes.outputs.skip == '"'true'"') }}' <(sed -n '/^  web:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: web gate must run if: !cancelled() (runs when a part fails, not on a superseded run)"; exit 1; }
# A draft is not tested: the gate must fail on it (old issue 593), or a vacuous green would be the newest run.
# shellcheck disable=SC2016 # literal text of the workflow
sed -n '/^  web:$/,/^  [a-z-]*:$/p' "$wf" | grep -qF 'SKIP == true && $EVENT == pull_request' || { echo "FAIL: web gate must fail on a draft (skip=true on a pull_request)"; exit 1; }
echo "ok: web gate covers $(echo "$jobs" | tr '\n' ' ')"
