#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Keeps the required status checks and the jobs of ci.yml in step (#561). hack/ci-required.txt is the
# source of truth for the `main` ruleset's required checks. A job of ci.yml, by its display name (the
# `name:` field, else the job key), must be
#   - listed as required,
#   - listed as `advisory` (an explicit, short list), or
#   - a part of a required job: named in the `needs` of a required job, transitively (the parts of the
#     `go` and `web` gates, and `changes`).
# Every required name (not `external`) must have a job. So a new job cannot be silently advisory, and
# a required name cannot outlive its job. Matrix jobs are not supported (a matrix fans one job out
# into several check names) and fail the test.
#
# Usage:
#   hack/ci-required-test.sh              the checks above, plus the script's own negative cases
#   hack/ci-required-test.sh --ruleset    diff the list against the live `main` ruleset (gh api); prints
#                                         SKIPPED and exits 0 when gh is missing or not authenticated
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

# check YML LIST: print problems, return 1 when there are any.
check() {
  local yml=$1 list=$2 problems=0 line key name needs matrix n
  local -A required=() advisory=() jobname=() jobneeds=() isjob=()
  while IFS= read -r line; do
    line=${line%%#*}
    line=$(sed -e 's/[[:space:]]*$//' -e 's/^[[:space:]]*//' <<<"$line")
    [[ -n $line ]] || continue
    case $line in
      advisory\ *) advisory[${line#advisory }]=1 ;;
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

  # Parts: jobs a required job depends on, transitively.
  local -A part=()
  local -a queue=()
  for key in "${!jobname[@]}"; do
    if [[ -n ${required[${jobname[$key]}]:-} ]]; then queue+=("$key"); fi
  done
  while [[ ${#queue[@]} -gt 0 ]]; do
    key=${queue[0]}; queue=("${queue[@]:1}")
    for n in ${jobneeds[$key]}; do
      if [[ -z ${part[$n]:-} && -n ${jobname[$n]:-} ]]; then
        part[$n]=1; queue+=("$n")
      fi
    done
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

# contexts LIST: the names the ruleset must require, sorted, one per line.
contexts() {
  local line
  while IFS= read -r line; do
    line=${line%%#*}
    line=$(sed -e 's/[[:space:]]*$//' -e 's/^[[:space:]]*//' <<<"$line")
    [[ -n $line && $line != advisory\ * ]] || continue
    echo "${line#external }"
  done <"$1" | sort -u
}

ruleset() {
  command -v gh >/dev/null 2>&1 || { echo "SKIPPED: gh is not installed; the list was not compared with the ruleset"; return 0; }
  gh auth status >/dev/null 2>&1 || { echo "SKIPPED: gh is not authenticated; the list was not compared with the ruleset"; return 0; }
  local repo ids id live want rule found=0
  repo=${CI_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)} ||
    { echo "SKIPPED: gh could not name the repository (offline?)"; return 0; }
  ids=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.target == "branch") | .id' 2>/dev/null) ||
    { echo "SKIPPED: gh api repos/$repo/rulesets failed (offline or no access)"; return 0; }
  for id in $ids; do
    rule=$(gh api "repos/$repo/rulesets/$id" 2>/dev/null) ||
      { echo "SKIPPED: gh api repos/$repo/rulesets/$id failed"; return 0; }
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
echo "ok: every job of $(basename "$yml") is required, advisory or a part of a required job; every required name has a job"

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
    name: the gate
    needs: [changes, part]
    runs-on: x
  plain:
    runs-on: x
  adv:
    name: "advisory one"
    runs-on: x
EOF
cat >"$tmp/ok.txt" <<'EOF'
the gate
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
expect_fail "job neither required nor advisory" $'the gate\nadvisory advisory one' "'plain' (plain) is neither"
expect_fail "required name without a job" $'the gate\nplain\nghost\nadvisory advisory one' "required check 'ghost' has no job"
expect_fail "advisory name without a job" $'the gate\nplain\nadvisory advisory one\nadvisory ghost' "advisory job 'ghost' does not exist"
expect_fail "advisory job not listed" $'the gate\nplain' "'advisory one' (adv) is neither"
printf 'jobs:\n  m:\n    name: m\n    strategy:\n      matrix:\n        a: [1]\n' >"$tmp/m.yml"
printf 'm\n' >"$tmp/m.txt"
if check "$tmp/m.yml" "$tmp/m.txt" >/dev/null 2>&1; then echo "FAIL: a matrix job passed"; exit 1; fi
echo "ok: the negative cases fail as they should"
