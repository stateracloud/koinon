#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Show why CI failed, in a few lines. For a PR it finds the failing checks; for a run id it takes the
# run's failed jobs. Per failing GitHub Actions job it prints at most 30 lines: the job name, the
# link, then the useful lines of `gh run view --log-failed` (ANSI codes and timestamps stripped;
# `--- FAIL`, `[FAIL]`/`[FAILED]` with their Expected/to equal lines, `panic:`, `##[error]`,
# `FAIL<TAB>pkg` and compiler errors `file.go:12:3:` kept; `go: downloading` dropped).
# After the link it prints one greppable line `class=<category> retry=<yes|no|unknown>`: the first row
# of hack/ci-fail-classes.tsv (ordered; CI_FAIL_CLASSES overrides the path) that matches the log, else
# `class=unknown retry=unknown` so a new failure shows up and the table can grow.
#
# Usage: hack/ci-fail.sh <PR|run-id> [--job NAME]
#   A number of 9 digits or more is a run id, a shorter one a PR number. --job keeps the jobs whose
#   name contains NAME.
# Exit: 0 nothing failed, 1 something failed, 64 usage.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

usage() {
  echo "usage: hack/ci-fail.sh <PR|run-id> [--job NAME]" >&2
  exit 64
}

target="" jobname=""
while [ $# -gt 0 ]; do
  case $1 in
  --job)
    [ $# -ge 2 ] || usage
    jobname=$2
    shift
    ;;
  -*) usage ;;
  *)
    [ -z "$target" ] || usage
    target=$1
    ;;
  esac
  shift
done
case $target in '' | *[!0-9]*) usage ;; esac

esc=$(printf '\033')
tab=$(printf '\t')

# filter: stdin is a --log-failed log, stdout the useful lines (deduplicated).
filter() {
  sed -E \
    -e "s/${esc}\\[[0-9;]*[A-Za-z]//g" \
    -e "s/^([^${tab}]*${tab}[^${tab}]*${tab})?[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z ?//" \
    -e 's/\r$//' |
    grep -Ev '^go: (downloading|finding|extracting)' |
    awk '!seen[$0]++'
}

# classify: stdin is the filtered log; prints "<category> <retry>" of the first table row (in table
# order, not log order) with a matching line, or "unknown unknown".
classify() {
  local table=${CI_FAIL_CLASSES:-$(dirname "${BASH_SOURCE[0]}")/ci-fail-classes.tsv} log klass retry re
  log=$(cat)
  while IFS=$'\t' read -r klass retry re; do
    case $klass in '' | '#'*) continue ;; esac
    if grep -Eq -e "$re" <<<"$log"; then
      echo "$klass $retry"
      return 0
    fi
  done <"$table"
  echo "unknown unknown"
}

useful() { # stdin: filtered log
  grep -E -e '--- FAIL' -e '\[FAIL(ED)?\]' -e 'panic:' -e '##\[error\]' \
    -e '^FAIL([[:space:]]|$)' -e '^[[:space:]]*(Expected|to equal|to be |to match|to contain|to have|to succeed|<[^>]+>:)' \
    -e '[A-Za-z0-9_./-]+\.go:[0-9]+(:[0-9]+)?:' || true
}

# Failing jobs, one per line: name, run id, job id, url (tab separated, "-" for empty).
if [ "${#target}" -ge 9 ]; then
  rows=$(gh run view "$target" --json jobs |
    jq -r --arg run "$target" '.jobs[] | select((.conclusion // "") | IN("failure", "timed_out", "cancelled", "startup_failure"))
      | [.name, $run, ((.databaseId // "") | tostring), (.url // "")] | map(if . == "" then "-" else . end) | @tsv')
else
  rows=$(gh pr view "$target" --json statusCheckRollup |
    jq -r '(.statusCheckRollup // [])[]
      | select(((.conclusion // .state // "") | IN("FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR")))
      | (.detailsUrl // .targetUrl // "") as $u
      | [(.name // .context // "?"), (($u | capture("/runs/(?<r>[0-9]+)")? | .r) // ""), (($u | capture("/job/(?<j>[0-9]+)")? | .j) // ""), $u]
      | map(if . == "" then "-" else . end) | @tsv')
fi

rc=0
while IFS=$'\t' read -r name run job url; do
  [ -n "$name" ] || continue
  if [ -n "$jobname" ]; then
    case $name in *"$jobname"*) ;; *) continue ;; esac
  fi
  rc=1
  echo "job $name"
  if [ "$run" = "-" ]; then
    echo "no GitHub Actions run: $url"
    echo "class=unknown retry=unknown"
    continue
  fi
  echo "run $url"
  args=(run view "$run" --log-failed)
  [ "$job" = "-" ] || args+=(--job "$job")
  ghrc=0
  raw=$(gh "${args[@]}" 2>&1) || ghrc=$?
  log=$(filter <<<"$raw") || true
  if [ "$ghrc" -ne 0 ]; then
    cls=unknown retry=unknown # gh's own error text is not a CI log
  else
    read -r cls retry <<<"$(classify <<<"$log")"
  fi
  echo "class=$cls retry=$retry"
  out=$(useful <<<"$log" | head -n 27) || true
  if [ -z "$out" ]; then
    out=$(printf '%s\n' "$log" | tail -n 10)
  fi
  printf '%s\n' "$out"
done <<<"$rows"
exit "$rc"
