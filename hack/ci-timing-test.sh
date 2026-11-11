#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# hack/ci-timing.sh (#572), offline. Two parts:
#  1. The analysis, on the recorded raw records in hack/testdata/ci-timing/records.json: the code-PR filter
#     (PR 5 is docs-only, PR 8 lists only docs but has 300 changed files), the statistics of the queue merges
#     (1, 2, 3, 7: even count, so p75 is the nearest rank), the admin split (4 never queued; 6 and 8 admin;
#     PR 2 was dropped once and still merged by the queue), the push-time fallbacks and runner minutes.
#  2. The fetch path, with a stub `gh` serving hack/testdata/ci-timing/gh/: run matching by head_sha, the
#     merge-queue window, a failed or empty GitHub call fails the run loudly and names the PR or run.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fix=$root/hack/testdata/ci-timing/records.json
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-timing-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail() { echo "ci-timing-test: $*" >&2; exit 1; }

chk() { # chk <jq expr> <expected>: on $out
  local got
  got=$(jq -c "$1" <<<"$out")
  [ "$got" = "$2" ] || fail "$1: got $got, want $2"
}

# ---- 1. analysis ------------------------------------------------------------------------------------------------
out=$("$root/hack/ci-timing.sh" --from "$fix" --json)
chk '.code_prs' 7
chk '.queue_prs' 4
chk '.admin_prs' 3
chk '.push_to_merged_min | [.n, .median, .p75, .worst]' '[4,25,30,40]'
chk '.queue_min | [.n, .median, .p75, .worst]' '[4,11,12,14]'
chk '.admin_push_to_merged_min | [.n, .median, .p75, .worst]' '[3,40,50,50]'
chk '[.prs[] | select(.admin) | .number]' '[4,6,8]'
chk '.push_source' '{"force_push":1,"suite":6}'
chk '[.prs[] | {key: (.number | tostring), value: .runner_min}] | from_entries' '{"1":15,"2":10,"3":0,"4":1,"6":5,"7":0,"8":0}'
chk '.runner_min_per_pr | [.n, .median, .p75, .worst]' '[7,1,10,15]'
chk '.ci_runs_per_pr_median' 1
chk '.job_median_pr_ci[0] | [.job, .n, .median_min]' '["ci/go-test",4,5]'
chk '.job_median_merge_group | map(.job)' '["ci/go-test"]'

txt=$("$root/hack/ci-timing.sh" --from "$fix")
grep -qx 'push_to_merged_min_median=25' <<<"$txt" || fail "text output lacks push_to_merged_min_median=25"
grep -qx 'admin_prs=3' <<<"$txt" || fail "text output lacks admin_prs=3"

rc=0
"$root/hack/ci-timing.sh" --n 0 >/dev/null 2>&1 || rc=$?
[ "$rc" = 64 ] || fail "--n 0 exits $rc, want 64"
"$root/hack/ci-timing.sh" help 2>&1 | grep -qi 'last push' || fail "help lacks the last-push definition"

# ---- 2. fetch path with a stub gh -------------------------------------------------------------------------------
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
# Serves the recorded files of $STUB; applies the --jq expression to them like gh does.
set -euo pipefail
echo "$*" >>"$STUB_LOG"
jqx="" ev="" br="" num="" ep=""
args=("$@")
i=0
while [ $i -lt ${#args[@]} ]; do
  a=${args[$i]}
  case $a in
  --jq) i=$((i + 1)); jqx=${args[$i]} ;;
  -f | -F)
    i=$((i + 1))
    kv=${args[$i]}
    case $kv in event=*) ev=${kv#event=} ;; branch=*) br=${kv#branch=} ;; n=*) num=${kv#n=} ;; esac
    ;;
  repos/*) ep=$a ;;
  esac
  i=$((i + 1))
done
[ "${1:-}" != repo ] || { echo example/repo; exit 0; }
[ "${1:-}" != pr ] || { cat "$STUB/prlist.json"; exit 0; }
case $ep in
'') key=gql-$num ;;
*/jobs) key=jobs-$(basename "$(dirname "$ep")") ;;
*) if [ "$ev" = merge_group ]; then key=runs-mg; else key=runs-pr-${br//\//_}; fi ;;
esac
[ "${STUB_FAIL:-}" != "$key" ] || exit 1
[ "${STUB_EMPTY:-}" != "$key" ] || exit 0
jq -c "$jqx" "$STUB/$key.json"
STUB
chmod +x "$tmp/bin/gh"
export STUB=$root/hack/testdata/ci-timing/gh STUB_LOG=$tmp/calls.log
run() { PATH="$tmp/bin:$PATH" "$root/hack/ci-timing.sh" --n 5 --json; }

out=$(run 2>/dev/null)
chk '.code_prs' 2
chk '.queue_prs' 1
chk '[.prs[] | select(.admin) | .number]' '[12]'
# PR 11: queue 10:15 to 10:30; last push = suite 10:00. Runs 101 (head_sha of its commit), 103 and queue run 201
# count; 102 (a sha that is not the PR's) and 202 (another PR's queue run) do not.
chk '.queue_min.median' 15
chk '.push_to_merged_min.median' 30
chk '[.prs[] | {key: (.number | tostring), value: [.runner_min, .runs]}] | from_entries' '{"11":[21,3],"12":[2,1]}'
grep -q 'created=2026-11-01T09:00:00Z\.\.2026-11-02T10:30:00Z' "$tmp/calls.log" || fail "merge-queue window created=A..B missing"
grep -q 'runs/102/jobs' "$tmp/calls.log" && fail "jobs of a run of another commit were fetched"
grep -q 'runs/101/jobs' "$tmp/calls.log" || fail "jobs of run 101 were not fetched"

# A failed call, or an empty pull request result, ends the run with exit 1 and names the PR or the run.
for case in jobs-101:jobs-101.json gql-12:gql-12.json runs-mg:mgruns.json; do
  rc=0
  err=$(STUB_FAIL=${case%%:*} run 2>&1 >/dev/null) || rc=$?
  [ "$rc" = 1 ] || fail "failed call ${case%%:*} exits $rc, want 1"
  grep -q "${case##*:}" <<<"$err" || fail "failed call ${case%%:*}: error does not name ${case##*:}: $err"
  [ "${case%%:*}" = gql-12 ] || continue # an empty run or job listing is a valid answer (`[]`); an empty pull request is not
  rc=0
  err=$(STUB_EMPTY=${case%%:*} run 2>&1 >/dev/null) || rc=$?
  [ "$rc" = 1 ] || fail "empty result ${case%%:*} exits $rc, want 1"
  grep -q "${case##*:}" <<<"$err" || fail "empty result ${case%%:*}: error does not name ${case##*:}: $err"
done
echo "ci-timing-test: ok"
