#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Keeps the required status checks and the jobs of ci.yml in step (old issue 561). hack/ci-required.txt is the
# source of truth for the `main` ruleset's required checks. A job of ci.yml, by its display name (the
# `name:` field, else the job key), must be
#   - listed as required,
#   - listed as `advisory` (an explicit, short list), or
#   - a part of an aggregator: named in the `needs` of a job listed as `aggregator` (the `go` and `web`
#     gates and their parts). An aggregator reads `needs.*.result`, so a failed or skipped part fails
#     it. Needing a job from any other required job is no exemption: `if:` without a status function
#     implies success(), so a failing needed job would make that required job skip, which is green.
# Every required name (not `external`) must have a job. So a new job cannot be silently advisory, and
# a required name cannot outlive its job. Matrix jobs are not supported (a matrix fans one job out
# into several check names) and fail the test.
#
# Usage:
#   hack/ci-required-test.sh              the checks above, plus the script's own negative cases
#   hack/ci-required-test.sh --ruleset    diff the list against the live `main` ruleset (gh api); prints
#                                         SKIPPED and exits 0 when gh is missing, not logged in
#                                         (auth) or cannot reach GitHub (network)
# Environment: CI_YML and CI_REQUIRED override the two files (used by the negative cases).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
yml=${CI_YML:-$root/.github/workflows/ci.yml}
list=${CI_REQUIRED:-$root/hack/ci-required.txt}

# jobs_of FILE: one line per job, "key<TAB>display name<TAB>space-separated needs (- for none)<TAB>matrix(0|1)".
jobs_of() {
  awk '
    function flush() {
      if (key != "") print key "\t" (name != "" ? name : key) "\t" (needs != "" ? needs : "-") "\t" matrix
      key = ""; name = ""; needs = ""; matrix = 0
    }
    /^jobs:/ { injobs = 1; next }
    injobs && /^[^ #]/ { flush(); injobs = 0 }
    !injobs { next }
    /^  [A-Za-z0-9_-]+:[ ]*$/ { flush(); key = $1; sub(/:$/, "", key); next }
    key == "" { next }
    /^    name:/ {
      v = $0; sub(/^    name:[ ]*/, "", v); sub(/[ ]+#.*$/, "", v)
      gsub(/^["\047]|["\047]$/, "", v); name = v; next
    }
    /^    needs:/ {
      v = $0; sub(/^    needs:[ ]*/, "", v); gsub(/[\[\],]/, " ", v); gsub(/[ ]+/, " ", v)
      sub(/^ /, "", v); sub(/ $/, "", v); needs = v; next
    }
    /^      matrix:/ || /^    strategy:/ { if ($0 ~ /matrix/) matrix = 1 }
    END { flush() }
  ' "$1"
}

# The aggregators are a fixed set, whose run scripts hack/ci-go-gate-test.sh and hack/ci-web-gate-test.sh
# test. A name in ci-required.txt outside this set is rejected.
aggregators=(go web)

# aggregator_problems YML KEY: what is wrong with the aggregator job, from the job's own lines (comment
# lines dropped): it must run when a part fails (job-level `if:` with !cancelled() or always(), else a
# skipped gate is green) and its step must read every part's result and require success.
aggregator_problems() {
  local body ifline
  body=$(awk -v k="  $2:" '$0 == k { on = 1; next } on && /^  [A-Za-z0-9_-]+:[ ]*$/ { on = 0 } on && $0 !~ /^[ ]*#/' "$1")
  ifline=$(grep -E '^    if:' <<<"$body" || true)
  grep -qE '!cancelled\(\)|always\(\)' <<<"$ifline" || echo "its job-level if: lacks !cancelled() or always(), so a failed part skips it (skipped is green);"
  grep -qE 'join\(needs\.\*\.result' <<<"$body" || echo "it does not collect needs.*.result;"
  grep -qE '== success' <<<"$body" || echo "it does not require every result to be success;"
}

# check YML LIST: print problems, return 1 when there are any.
check() {
  local yml=$1 list=$2 problems=0 line key name needs matrix n
  local -A required=() advisory=() aggregator=() jobname=() jobneeds=() isjob=()
  while IFS= read -r line; do
    line=${line%%#*}
    line=$(sed -e 's/[[:space:]]*$//' -e 's/^[[:space:]]*//' <<<"$line")
    [[ -n $line ]] || continue
    case $line in
      advisory\ *) advisory[${line#advisory }]=1 ;;
      aggregator\ *)
        name=${line#aggregator }
        if [[ " ${aggregators[*]} " != *" $name "* ]]; then
          echo "FAIL: '$name' is not a known aggregator (${aggregators[*]}); only gates tested by hack/ci-*-gate-test.sh qualify"
          problems=1
        fi
        aggregator[$name]=1; required[$name]=1 ;;
      external\ *) : ;; # produced by another workflow
      *) required[$line]=1 ;;
    esac
  done <"$list"
  [[ ${#required[@]} -gt 0 ]] || { echo "FAIL: no required checks in $list"; return 1; }

  while IFS=$'\t' read -r key name needs matrix; do
    if [[ $matrix == 1 ]]; then
      echo "FAIL: job '$key' uses a matrix; this test cannot name its checks (extend it before adding one)"
      problems=1
    fi
    jobname[$key]=$name
    jobneeds[$key]=$needs
    isjob[$name]=1
  done < <(jobs_of "$yml")
  [[ ${#jobname[@]} -gt 0 ]] || { echo "FAIL: no jobs found in $yml"; return 1; }

  # Parts: the jobs an aggregator needs. The aggregator must check every result.
  local -A part=()
  for key in "${!jobname[@]}"; do
    [[ -n ${aggregator[${jobname[$key]}]:-} ]] || continue
    gate_problems=$(aggregator_problems "$yml" "$key") || true
    if [[ -n $gate_problems ]]; then
      echo "FAIL: aggregator '${jobname[$key]}': $gate_problems"
      problems=1
    fi
    for n in ${jobneeds[$key]}; do
      if [[ -n ${jobname[$n]:-} ]]; then part[$n]=1; fi
    done
  done
  for name in "${!aggregator[@]}"; do
    if [[ -z ${isjob[$name]:-} ]]; then echo "FAIL: aggregator '$name' has no job"; problems=1; fi
  done

  for key in "${!jobname[@]}"; do
    name=${jobname[$key]}
    if [[ -n ${required[$name]:-} || -n ${advisory[$name]:-} || -n ${part[$key]:-} ]]; then continue; fi
    echo "FAIL: job '$name' ($key) is neither required nor advisory: add it to $(basename "$list") (required, or 'advisory ' with a reason), then to the ruleset"
    problems=1
  done
  for name in "${!required[@]}"; do
    if [[ -z ${isjob[$name]:-} ]]; then
      echo "FAIL: required check '$name' has no job in $(basename "$yml") (list it as 'external ' if another workflow produces it)"
      problems=1
    fi
  done
  for name in "${!advisory[@]}"; do
    if [[ -z ${isjob[$name]:-} ]]; then
      echo "FAIL: advisory job '$name' does not exist in $(basename "$yml"): remove it from $(basename "$list")"
      problems=1
    fi
  done
  for name in "${!advisory[@]}"; do
    if [[ -n ${required[$name]:-} ]]; then echo "FAIL: '$name' is both required and advisory"; problems=1; fi
  done
  return "$problems"
}

# workflows_of LIST: the workflow files that produce a required check, one basename per line: ci.yml and the
# external ones the header of the list names (`#   <check>   .github/workflows/<file>.yml`).
workflows_of() {
  { echo ci.yml; sed -n -E 's@^#[[:space:]]+[^[:space:]].*[[:space:]]\.github/workflows/([A-Za-z0-9_.-]+\.yml)[[:space:]]*$@\1@p' "$1"; } | sort -u
}

# merge_group_problems LIST WFDIR: the merge queue (ADR 0094 decision 4) waits for every required check on its
# own build, so a workflow that produces one must listen to `merge_group`; else the check stays absent and the
# queue entry times out.
merge_group_problems() {
  local wf n=0 problems=0
  while IFS= read -r wf; do
    n=$((n + 1))
    if [[ ! -f $2/$wf ]]; then echo "FAIL: required workflow $wf is not in $2"; problems=1; continue; fi
    grep -Eq '^  merge_group:' "$2/$wf" || { echo "FAIL: workflow $wf produces a required check but has no merge_group trigger (ADR 0094)"; problems=1; }
  done < <(workflows_of "$1")
  [[ $n -gt 1 ]] || { echo "FAIL: no external workflows found in the header of $(basename "$1")"; problems=1; }
  return "$problems"
}

# contexts LIST: the names the ruleset must require, sorted, one per line.
contexts() {
  local line
  while IFS= read -r line; do
    line=${line%%#*}
    line=$(sed -e 's/[[:space:]]*$//' -e 's/^[[:space:]]*//' <<<"$line")
    [[ -n $line && $line != advisory\ * ]] || continue
    line=${line#external }
    echo "${line#aggregator }"
  done <"$1" | sort -u
}

ruleset() {
  command -v gh >/dev/null 2>&1 || { echo "SKIPPED (auth): gh is not installed; the list was not compared with the ruleset"; return 0; }
  local probe
  if ! probe=$(gh api rate_limit 2>&1); then # reachability first: `gh auth status` also fails offline
    if grep -qiE '401|403|auth|login|token|credential' <<<"$probe"; then
      echo "SKIPPED (auth): gh is not logged in; the list was not compared with the ruleset"
    else
      echo "SKIPPED (network): GitHub is not reachable; the list was not compared with the ruleset"
    fi
    return 0
  fi
  local repo ids id live want rule found=0
  repo=${CI_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)} ||
    { echo "SKIPPED (network): gh could not name the repository, offline or not a GitHub checkout"; return 0; }
  ids=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.target == "branch") | .id' 2>/dev/null) ||
    { echo "SKIPPED (network): gh api repos/$repo/rulesets failed, offline or no access"; return 0; }
  for id in $ids; do
    rule=$(gh api "repos/$repo/rulesets/$id" 2>/dev/null) ||
      { echo "SKIPPED (network): gh api repos/$repo/rulesets/$id failed"; return 0; }
    [[ $(jq -r .name <<<"$rule") == main ]] || continue
    found=1
    live=$(jq -r '[.rules[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context] | .[]' <<<"$rule" | sort -u)
    want=$(contexts "$list")
    if [[ $live == "$want" ]]; then
      echo "ok: the ruleset 'main' of $repo requires exactly the $(wc -l <<<"$want" | tr -d ' ') checks of $(basename "$list")"
      return 0
    fi
    echo "FAIL: the ruleset 'main' of $repo and $(basename "$list") differ"
    comm -23 <(echo "$live") <(echo "$want") | sed 's/^/  only in the ruleset: /' || true
    comm -13 <(echo "$live") <(echo "$want") | sed 's/^/  only in the list: /' || true
    return 1
  done
  [[ $found == 1 ]] || { echo "FAIL: no branch ruleset named 'main' in $repo"; return 1; }
}

if [[ ${1:-} == --ruleset ]]; then
  ruleset
  exit
fi

check "$yml" "$list"
echo "ok: every job of $(basename "$yml") is required, advisory or a part of an aggregator; every required name has a job"

merge_group_problems "$list" "${CI_WORKFLOWS:-$root/.github/workflows}"
echo "ok: every workflow that produces a required check listens to merge_group"

# Negative cases, on small fixtures: the check has to fail where it should.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-required-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/ci.yml" <<'EOF'
name: ci
on: push
jobs:
  changes:
    name: changes
    runs-on: x
  part:
    needs: changes
    name: part
    runs-on: x
  gate:
    name: go
    needs: [changes, part]
    if: ${{ !cancelled() }}
    runs-on: x
    steps:
      - env:
          RESULTS: ${{ join(needs.*.result, ' ') }}
        run: for r in $RESULTS; do [[ $r == success ]] || exit 1; done
  docs:
    name: docs
    needs: [changes]
    runs-on: x
  plain:
    runs-on: x
  adv:
    name: "advisory one"
    runs-on: x
EOF
cat >"$tmp/ok.txt" <<'EOF'
aggregator go
docs
plain # comment
external elsewhere
advisory advisory one
EOF
check "$tmp/ci.yml" "$tmp/ok.txt" >/dev/null || { echo "FAIL: the fixture that should pass failed"; check "$tmp/ci.yml" "$tmp/ok.txt"; exit 1; }
expect_fail() { # description, list content, expected text
  printf '%s\n' "$2" >"$tmp/bad.txt"
  local out
  if out=$(check "$tmp/ci.yml" "$tmp/bad.txt" 2>&1); then echo "FAIL: $1: the check passed"; exit 1; fi
  grep -qF "$3" <<<"$out" || { echo "FAIL: $1: expected '$3' in:"; echo "$out"; exit 1; }
}
expect_fail "job neither required nor advisory" $'aggregator go\ndocs\nadvisory advisory one' "'plain' (plain) is neither"
expect_fail "required name without a job" $'aggregator go\ndocs\nplain\nghost\nadvisory advisory one' "required check 'ghost' has no job"
expect_fail "advisory name without a job" $'aggregator go\ndocs\nplain\nadvisory advisory one\nadvisory ghost' "advisory job 'ghost' does not exist"
expect_fail "advisory job not listed" $'aggregator go\ndocs\nplain' "'advisory one' (adv) is neither"
# The reviewer's repro: a required job needs a hidden job that nothing aggregates; if hidden fails,
# docs is skipped, which is green.
sed -e 's/^    needs: \[changes\]$/    needs: [changes, hidden]/' -e 's/^  plain:$/  hidden:\n    runs-on: x\n    steps:\n      - run: "false"\n  plain:/' "$tmp/ci.yml" >"$tmp/hidden.yml"
cp "$tmp/ok.txt" "$tmp/hidden.txt"
if out=$(check "$tmp/hidden.yml" "$tmp/hidden.txt" 2>&1); then echo "FAIL: a hidden need of a required job passed"; exit 1; fi
grep -qF "job 'hidden' (hidden) is neither" <<<"$out" || { echo "FAIL: hidden need: wrong message: $out"; exit 1; }
# An aggregator that does not read the results is not one, and a comment does not count.
sed -e 's/join(needs.\*.result/join(x.y/' "$tmp/ci.yml" >"$tmp/noagg.yml"
if out=$(check "$tmp/noagg.yml" "$tmp/ok.txt" 2>&1); then echo "FAIL: an aggregator without needs.*.result passed"; exit 1; fi
grep -qF "does not collect needs" <<<"$out" || { echo "FAIL: noagg: wrong message: $out"; exit 1; }
sed -e 's/^    name: docs$/    name: docs\n    # needs.*.result join(needs.*.result) == success !cancelled()/' "$tmp/ci.yml" >"$tmp/comment.yml"
printf 'aggregator go\naggregator docs\nplain\nadvisory advisory one\n' >"$tmp/comment.txt"
if out=$(check "$tmp/comment.yml" "$tmp/comment.txt" 2>&1); then echo "FAIL: a comment made docs an aggregator"; exit 1; fi
grep -qF "'docs' is not a known aggregator" <<<"$out" || { echo "FAIL: comment: wrong message: $out"; exit 1; }
# An aggregator without !cancelled() is skipped when a part fails, which is green.
sed -e '/^    if: \${{ !cancelled() }}$/d' "$tmp/ci.yml" >"$tmp/nocancel.yml"
if out=$(check "$tmp/nocancel.yml" "$tmp/ok.txt" 2>&1); then echo "FAIL: an aggregator without !cancelled() passed"; exit 1; fi
grep -qF "lacks !cancelled()" <<<"$out" || { echo "FAIL: nocancel: wrong message: $out"; exit 1; }
printf 'jobs:\n  m:\n    name: m\n    strategy:\n      matrix:\n        a: [1]\n' >"$tmp/m.yml"
printf 'm\n' >"$tmp/m.txt"
if check "$tmp/m.yml" "$tmp/m.txt" >/dev/null 2>&1; then echo "FAIL: a matrix job passed"; exit 1; fi
# A required external workflow without merge_group fails.
mkdir "$tmp/wf"
printf 'on:\n  merge_group:\n    types: [checks_requested]\n' >"$tmp/wf/ci.yml"
printf 'on:\n  merge_group:\n    types: [checks_requested]\n' >"$tmp/wf/ext.yml"
printf '# Required checks that other workflows produce:\n#   dco              .github/workflows/ext.yml\n\nexternal dco\n' >"$tmp/mg.txt"
merge_group_problems "$tmp/mg.txt" "$tmp/wf" >/dev/null || { echo "FAIL: the merge_group fixture that should pass failed"; exit 1; }
printf 'on:\n  pull_request:\n' >"$tmp/wf/ext.yml"
if out=$(merge_group_problems "$tmp/mg.txt" "$tmp/wf" 2>&1); then echo "FAIL: a required workflow without merge_group passed"; exit 1; fi
grep -qF "workflow ext.yml produces a required check but has no merge_group" <<<"$out" || { echo "FAIL: merge_group: wrong message: $out"; exit 1; }
echo "ok: the negative cases fail as they should"
