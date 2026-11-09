#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/review-gate.sh (the `review` check, ADR 0103) and hack/review-verdict.sh with a stub gh.
# The stub answers from JSON files in $T (files, statuses, pr, runs, jobs) through the caller's --jq
# filter, so the filters are exercised; calls are recorded in $T/calls. The sensitive paths come from
# the real hack/claim.sh (its `sensitive-paths` output is the contract), except where a case stubs it.
# Also pins the workflow's shape (.github/workflows/review.yml).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/review-gate-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
# Answers from JSON files in $T, applying the caller's --jq filter like gh does.
echo "$*" >>"$T/calls"
jqf=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do [[ ${args[i]} == --jq ]] && jqf=${args[i + 1]}; done
ans() { [[ -f $1 ]] || exit 0; if [[ -n $jqf ]]; then jq -r "$jqf" "$1"; else cat "$1"; fi; }
err() { [[ ! -f $1 ]] || { cat "$1" >&2; exit 1; }; }
case "$*" in
"api --paginate repos/o/r/pulls/"*"/files"*) err "$T/files.err"; ans "$T/files.json" ;;
"api repos/o/r/commits/"*"/statuses"*) err "$T/statuses.err"; ans "$T/statuses.json" ;;
"pr view"*) err "$T/pr.err"; ans "$T/pr.json" ;;
"api -X POST repos/o/r/statuses/"*) [[ -z ${T_POST_FAILS:-} ]] || { echo "HTTP 422" >&2; exit 1; } ;;
"run list"*) ans "$T/runs.json" ;;
"api repos/o/r/actions/runs/"*"/jobs"*) ans "$T/jobs.json" ;;
"run rerun"*) [[ -z ${T_RERUN_FAILS:-} ]] ;;
*) exit 9 ;;
esac
SH
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" T="$tmp" GH="$tmp/bin/gh" GH_REPO=o/r
unset GITHUB_STEP_SUMMARY CLAIM
SHA=0123456789abcdef0123456789abcdef01234567
OTHER=89abcdef0123456789abcdef0123456789abcdef

fail=0
show() { printf '%s\n' "$1" | sed 's/^/    /'; }
reset() { rm -f "$tmp"/*.json "$tmp"/*.err "$tmp"/calls; }
# files <name>...: the pull request's files (a name `old=>new` is a rename).
files() {
  local j="[" sep="" f
  for f in "$@"; do
    if [[ $f == *'=>'* ]]; then j+="$sep{\"filename\":\"${f#*=>}\",\"previous_filename\":\"${f%%=>*}\"}"; else j+="$sep{\"filename\":\"$f\"}"; fi
    sep=","
  done
  echo "$j]" >"$tmp/files.json"
}
# statuses <context>:<state>...: newest first, as the API lists them.
statuses() {
  local j="[" sep="" s
  for s in "$@"; do j+="$sep{\"context\":\"${s%%:*}\",\"state\":\"${s#*:}\",\"creator\":{\"login\":\"procoduck\"},\"description\":\"ready: reviewed by opus-review\"}"; sep=","; done
  echo "$j]" >"$tmp/statuses.json"
}
# gate <want rc> <want text> <description> [VAR=value...]
gate() {
  local want=$1 text=$2 desc=$3 rc=0 out
  shift 3
  out=$(env EVENT=pull_request_target PR=7 SHA=$SHA "$@" bash hack/review-gate.sh 2>&1) || rc=$?
  if [[ $rc != "$want" ]]; then echo "FAIL: gate: $desc: exit $rc, want $want"; show "$out"; fail=1; return; fi
  if [[ -n $text ]] && ! grep -qF -- "$text" <<<"$out"; then echo "FAIL: gate: $desc: no '$text' in:"; show "$out"; fail=1; return; fi
  echo "ok: gate: $desc"
}

# The contract with claim.sh: its sensitive-paths output, as ruleset patterns.
pats=$(KOINON_CLAIM_REPO=o/r bash hack/claim.sh sensitive-paths)
for p in '.github/**' 'AGENTS.md' 'docs/adr/**' 'hack/claim.sh'; do
  grep -qxF -- "$p" <<<"$pats" || { echo "FAIL: hack/claim.sh sensitive-paths has no '$p'"; fail=1; }
done
echo "ok: hack/claim.sh sensitive-paths prints ruleset patterns ($(wc -l <<<"$pats" | tr -d ' '))"

reset; files internal/api/x.go web/src/a.ts
gate 0 "touches no sensitive path" "no sensitive path: pass without a verdict"
grep -q statuses "$tmp/calls" && { echo "FAIL: gate: the statuses were read for a non-sensitive PR"; fail=1; }

reset; files internal/api/x.go .github/workflows/ci.yml
gate 1 "hack/review-verdict.sh 7 ready <reviewer> $SHA" "sensitive, no verdict: fail and name the script"
reset; files AGENTS.md; statuses e2e-lab:success
gate 1 "has no review-verdict status" "sensitive, other statuses only: fail"
reset; files docs/adr/0103-x.md; statuses review-verdict:success
gate 0 "review-verdict is success" "sensitive, success verdict: pass"
reset; files charts/koinon/values.yaml; statuses review-verdict:failure review-verdict:success
gate 1 "review-verdict on ${SHA:0:12} is failure" "the newest verdict counts (failure after success)"
reset; files hack/claim.sh; statuses review-verdict:success review-verdict:failure
gate 0 "review-verdict is success" "the newest verdict counts (success after failure)"
reset; files hack/claim.sh.bak hack/claim_test.sh docs/adr.md
gate 0 "touches no sensitive path" "an exact pattern matches the exact path only"
reset; files 'internal/auth/x.go=>internal/other/x.go'
gate 1 "internal/auth/x.go (internal/auth/**)" "a rename out of a sensitive path counts by its old name"
reset; echo '[]' >"$tmp/files.json"
gate 1 "the file list is empty" "an empty file list fails closed"
reset; echo "HTTP 502" >"$tmp/files.err"
gate 1 "cannot read the files" "a file list error fails closed"
reset; files AGENTS.md; echo "HTTP 403: Resource not accessible by integration" >"$tmp/statuses.err"
gate 1 "statuses: read" "a statuses error fails closed and names the permission"
reset; files AGENTS.md; echo 'not json' >"$tmp/statuses.json"
gate 1 "not valid JSON" "a statuses body that is not JSON fails closed"
reset; printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/noclaim"; chmod +x "$tmp/noclaim"; files AGENTS.md
gate 1 "printed no patterns" "no sensitive patterns fails closed" CLAIM="$tmp/noclaim"
reset; printf '#!/usr/bin/env bash\necho boom; exit 1\n' >"$tmp/badclaim"; chmod +x "$tmp/badclaim"; files AGENTS.md
gate 1 "sensitive-paths failed: boom" "a claim.sh failure fails closed" CLAIM="$tmp/badclaim"
reset
gate 1 "cannot read the head commit" "no head sha fails closed" SHA=
gate 1 "no pull request number" "no PR number fails closed" PR=
gate 0 "nothing to review" "push: pass" EVENT=push PR= SHA=

# merge_group: the queued PR from the ref, its head from the API.
reset; files AGENTS.md; echo "{\"headRefOid\":\"$SHA\"}" >"$tmp/pr.json"; statuses review-verdict:success
gate 0 "review-verdict is success" "merge_group: the queued PR's head verdict" EVENT=merge_group PR= SHA= GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-7-$OTHER
grep -q "pulls/7/files" "$tmp/calls" || { echo "FAIL: merge_group: the files of #7 were not read"; fail=1; }
grep -q "commits/$SHA/statuses" "$tmp/calls" || { echo "FAIL: merge_group: the statuses of the PR head were not read"; fail=1; }
reset; files AGENTS.md; echo "{\"headRefOid\":\"$SHA\"}" >"$tmp/pr.json"
gate 1 "has no review-verdict status" "merge_group: no verdict on the PR head fails" EVENT=merge_group PR= SHA= GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-7-$OTHER
reset; files internal/x.go; echo "{\"headRefOid\":\"$SHA\"}" >"$tmp/pr.json"
gate 0 "touches no sensitive path" "merge_group: not sensitive passes" EVENT=merge_group PR= SHA= GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-7-$OTHER
reset
gate 1 "cannot read the pull request number" "merge_group: a bad ref fails closed" EVENT=merge_group PR= SHA= GITHUB_REF=refs/heads/main
reset; echo "HTTP 404" >"$tmp/pr.err"
gate 1 "cannot read the head commit" "merge_group: no head fails closed" EVENT=merge_group PR= SHA= GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-7-$OTHER

# The step summary.
reset; files AGENTS.md
gate 1 "" "summary written" GITHUB_STEP_SUMMARY="$tmp/summary"
if ! grep -q "^## review" "$tmp/summary" 2>/dev/null || ! grep -qF "hack/review-verdict.sh 7 ready" "$tmp/summary"; then
  echo "FAIL: the step summary lacks the failure and the hint"; fail=1
fi

# hack/review-verdict.sh
# verdict <want rc> <want text> <description> ARGS...
verdict() {
  local want=$1 text=$2 desc=$3 rc=0 out
  shift 3
  out=$(bash hack/review-verdict.sh "$@" 2>&1) || rc=$?
  if [[ $rc != "$want" ]]; then echo "FAIL: verdict: $desc: exit $rc, want $want"; show "$out"; fail=1; return; fi
  if [[ -n $text ]] && ! grep -qF -- "$text" <<<"$out"; then echo "FAIL: verdict: $desc: no '$text' in:"; show "$out"; fail=1; return; fi
  echo "ok: verdict: $desc"
}
pr() { echo "{\"state\":\"$1\",\"headRefOid\":\"$2\"}" >"$tmp/pr.json"; }
run() { echo "[{\"databaseId\":55,\"status\":\"$1\",\"conclusion\":\"$2\"}]" >"$tmp/runs.json"; echo '{"jobs":[{"id":9,"name":"review"}]}' >"$tmp/jobs.json"; }

reset
verdict 64 "usage" "too few arguments" 7 ready opus
verdict 64 "ready or not-ready" "a bad verdict" 7 approve opus "$SHA"
verdict 64 "not a full commit sha" "a short sha" 7 ready opus "${SHA:0:12}"
verdict 64 "the reviewer is" "a reviewer with spaces" 7 ready "opus review" "$SHA"
verdict 64 "not a pull request number" "a bad PR" x7 ready opus "$SHA"
[[ ! -f $tmp/calls ]] || { echo "FAIL: verdict: usage errors called gh"; fail=1; }

reset; pr OPEN "$OTHER"
verdict 4 "STALE" "the head moved: refuse" 7 ready opus "$SHA"
grep -q "POST" "$tmp/calls" && { echo "FAIL: verdict: a stale head was posted"; fail=1; }
reset; pr MERGED "$SHA"
verdict 3 "not open" "a merged PR: refuse" 7 ready opus "$SHA"
reset; echo "HTTP 404" >"$tmp/pr.err"
verdict 1 "cannot read pull request #7" "an unreadable PR: exit 1" 7 ready opus "$SHA"

reset; pr OPEN "$SHA"; run completed failure
verdict 0 "re-running review (job 9) of run 55" "ready: post success and re-run the red job" 7 ready opus-review "$SHA" "lane review, 0 findings"
grep -qF -- "-X POST repos/o/r/statuses/$SHA -f state=success -f context=review-verdict -f description=ready: reviewed by opus-review: lane review, 0 findings" "$tmp/calls" || { echo "FAIL: verdict: the posted status is wrong:"; cat "$tmp/calls"; fail=1; }
grep -qF -- "run list --workflow review.yml --commit $SHA --event pull_request_target" "$tmp/calls" || { echo "FAIL: verdict: the run lookup is wrong"; fail=1; }
reset; pr OPEN "$SHA"; run completed success
verdict 0 "already success" "ready on a green job: no re-run" 7 ready opus "$SHA"
grep -q "run rerun" "$tmp/calls" && { echo "FAIL: verdict: re-ran a job that agrees"; fail=1; }
reset; pr OPEN "$SHA"; run completed success
verdict 0 "re-running review" "not-ready on a green job: post failure and re-run it" 7 not-ready opus "$SHA"
grep -qF -- "-f state=failure -f context=review-verdict -f description=not-ready: reviewed by opus" "$tmp/calls" || { echo "FAIL: verdict: not-ready did not post failure"; fail=1; }
reset; pr OPEN "$SHA"; run in_progress ""
verdict 0 "Re-run all jobs" "a run in progress: say so, no re-run" 7 ready opus "$SHA"
reset; pr OPEN "$SHA"
verdict 0 "no review run" "no run yet: posted, nothing to re-run" 7 ready opus "$SHA"
reset; pr OPEN "$SHA"; run completed failure
T_POST_FAILS=1 verdict 1 "could not post" "the post fails: exit 1, no re-run" 7 ready opus "$SHA"
reset; pr OPEN "$SHA"; run completed failure
T_RERUN_FAILS=1 verdict 0 "could not re-run" "a failed re-run is a warning" 7 ready opus "$SHA"
reset; pr OPEN "$SHA"
long=$(printf 'n%.0s' {1..200})
verdict 0 "posted" "a long note is cut to 140 characters" 7 ready opus "$SHA" "$long"
d=$(sed -n 's/.*-f description=//p' "$tmp/calls" | head -1)
d=${d% --silent}
((${#d} == 140)) || { echo "FAIL: verdict: description is ${#d} characters, want 140"; fail=1; }

# The workflow: the check name, the triggers, the base's YAML for pull requests, drafts skipped.
wf=.github/workflows/review.yml
w() { grep -Eq -- "$1" "$wf" || { echo "FAIL: review.yml: no /$1/"; fail=1; }; }
w '^    name: review$'
w '^  pull_request_target:$'
w '^  merge_group:$'
w '^    types: \[checks_requested\]$'
w '^  push:$'
w "^    if: github.event_name != 'pull_request_target' \|\| !github.event.pull_request.draft$"
w 'statuses: read'
# shellcheck disable=SC2016 # a regex, not an expansion
w 'git cat-file -e "\$MG_BASE:hack/review-gate.sh"'
if grep -Eq '^  pull_request:|^    paths:|ref: \$\{\{ github.event.pull_request.head' "$wf"; then echo "FAIL: review.yml runs the PR's YAML or code, or has a paths filter"; fail=1; fi
echo "ok: review.yml shape"

if ((fail)); then echo "review-gate-test: FAILED"; exit 1; fi
echo "review-gate-test: all passed"
