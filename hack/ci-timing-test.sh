#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# hack/ci-timing.sh (#572), offline, on the recorded raw records in hack/testdata/ci-timing/records.json:
# the code-PR filter (PR 5 is docs-only), the push-to-merged and queue statistics of the queue merges
# (PRs 1 to 3), the admin split (PR 4), the push-time fallbacks, runner minutes and the job medians.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fix=$root/hack/testdata/ci-timing/records.json
fail() { echo "ci-timing-test: $*" >&2; exit 1; }

out=$("$root/hack/ci-timing.sh" --from "$fix" --json)
chk() { # chk <jq expr> <expected>
  local got
  got=$(jq -c "$1" <<<"$out")
  [ "$got" = "$2" ] || fail "$1: got $got, want $2"
}
chk '.code_prs' 4
chk '.queue_prs' 3
chk '.admin_prs' 1
chk '.push_to_merged_min | [.n, .median, .p75, .worst]' '[3,30,40,40]'
chk '.queue_min | [.n, .median, .p75, .worst]' '[3,12,14,14]'
chk '.admin_push_to_merged_min | [.n, .median, .worst]' '[1,50,50]'
chk '[.prs[] | select(.admin) | .number]' '[4]'
chk '.push_source' '{"force_push":1,"suite":3}'
# PR 1: (5+2+8) min of jobs; PR 2: 10; PR 3: none; PR 4: 1.
chk '[.prs[] | {key: (.number | tostring), value: .runner_min}] | from_entries' '{"1":15,"2":10,"3":0,"4":1}'
chk '.runner_min_per_pr | [.median, .worst]' '[5.5,15]'
chk '.ci_runs_per_pr_median' 1
chk '.job_median_pr_ci[0] | [.job, .n, .median_min]' '["ci/go-test",3,5]'
chk '.job_median_merge_group | map(.job)' '["ci/go-test"]'

# The text form has the same numbers.
txt=$("$root/hack/ci-timing.sh" --from "$fix")
grep -qx 'push_to_merged_min_median=30' <<<"$txt" || fail "text output lacks push_to_merged_min_median=30"
grep -qx 'admin_prs=1' <<<"$txt" || fail "text output lacks admin_prs=1"

# Usage errors exit 64; help prints the text.
rc=0
"$root/hack/ci-timing.sh" --n 0 >/dev/null 2>&1 || rc=$?
[ "$rc" = 64 ] || fail "--n 0 exits $rc, want 64"
"$root/hack/ci-timing.sh" help 2>&1 | grep -qi 'last push' || fail "help lacks the last-push definition"
echo "ci-timing-test: ok"
