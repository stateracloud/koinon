#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Mutation testing driver (spike for issue 401; see docs/dev/mutation-spike-2026-11.md).
#
# Generates mutants with avito-tech/go-mutesting (pinned in tools/mutation/go.mod) and runs each one
# through `go test -overlay`, so the source tree is never modified and mutants run in parallel. The
# tool's own executor copies every mutant over the real file and runs `go test` serially; we only use
# it to generate (--no-exec).
#
# Usage:
#   hack/mutation.sh --packages "./internal/declarative ./internal/leakguard"
#   hack/mutation.sh --diff origin/main            # only mutants on lines changed since the merge base
#   hack/mutation.sh --packages ./internal/declarative --lines internal/declarative/declarative.go:170-200
#
# Flags:
#   --packages "P..."   packages to mutate (go list patterns, space separated). With --diff and no
#                       --packages: the packages that hold changed non-test .go files.
#   --diff BASE         keep only mutants whose changed line was added or changed since
#                       `git merge-base BASE HEAD` (working-tree changes included).
#   --lines FILE:A-B    keep only mutants in FILE between lines A and B (repeatable).
#   --max-procs N       mutants tested at once (default 2). Each runs a full `go test` of its package.
#                       Postgres specs share one server (hack/pgserver, KOINON_PGTEST_DSN) as in
#                       hack/go-test.sh; every test process gets its own template-copied databases.
#   --dry-run           print the packages and the changed line ranges, then stop.
#   --timeout S         per-mutant test timeout in seconds (default: 3x the clean run, at least 60).
#   --disable M         go-mutesting mutator to disable (repeatable, e.g. 'numbers/*').
#   --out DIR           result directory (default $TMPDIR/koinon-mutation/<timestamp>).
#   --replace F=SRC     also overlay repo file F with SRC in the clean run and every mutant run
#                       (repeatable), e.g. an older test file to show a mutant it would let through.
#
# Output: DIR/results.tsv (pkg file line mutator status seconds mutant), DIR/survivors/*.diff, and
# one summary line per package. Status is killed, survived, timeout or error (the mutant did not
# build, or the test binary failed to set up). Exit 0 when no mutant survived, 1 when any survived,
# 2 on a usage or setup error. Not part of the required CI.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
packages=""
diff_base=""
lines=()
max_procs=2
timeout=""
disables=()
replaces=()
out=""

dry_run=0

usage() { awk 'NR >= 4 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
  case $1 in
    --packages) packages=$2; shift 2 ;;
    --diff) diff_base=$2; shift 2 ;;
    --lines) lines+=("$2"); shift 2 ;;
    --max-procs) max_procs=$2; shift 2 ;;
    --timeout) timeout=$2; shift 2 ;;
    --disable) disables+=("$2"); shift 2 ;;
    --out) out=$2; shift 2 ;;
    --replace) replaces+=("$2"); shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "mutation.sh: unknown argument $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ $max_procs =~ ^[1-9][0-9]*$ ]] || { echo "mutation.sh: --max-procs must be a positive integer" >&2; exit 2; }
timeout_flag=$timeout
cd "$root"
out=${out:-${TMPDIR:-/tmp}/koinon-mutation/$(date +%Y%m%dT%H%M%S)}
mkdir -p "$out/bin" "$out/overlay" "$out/logs" "$out/survivors"

# Changed line ranges since the merge base: "file start end" per hunk (new side).
changed=""
if [ -n "$diff_base" ]; then
  mb=$(git merge-base "$diff_base" HEAD)
  changed=$(git --no-pager diff -U0 --no-color "$mb" -- '*.go' ':!*_test.go' | awk '
    /^\+\+\+ / { f = substr($2, 3); next }
    /^@@ / {
      split($3, a, ","); s = substr(a[1], 2) + 0; n = (a[2] == "") ? 1 : a[2] + 0
      if (n > 0 && f != "dev/null") print f, s, s + n - 1
    }')
  if [ -z "$packages" ]; then
    packages=$(awk '{print "./" $1}' <<<"$changed" | xargs -r -n1 dirname | sort -u | tr '\n' ' ')
  fi
  if [ -z "$changed" ]; then echo "mutation.sh: no changed Go lines since $mb"; exit 0; fi
fi
[ -n "$packages" ] || { echo "mutation.sh: give --packages or --diff" >&2; exit 2; }
if [ $dry_run = 1 ]; then
  echo "packages: $packages"
  [ -z "$changed" ] || { echo "changed (file first last):"; echo "$changed"; }
  exit 0
fi

# The tool lives in its own module so it never enters the root go.mod.
(cd tools/mutation && go build -o "$out/bin/go-mutesting" "github.com/avito-tech/go-mutesting/cmd/go-mutesting")

# One testcontainers session and one shared Postgres for the whole run, as hack/go-test.sh does:
# without them every mutant's test binary starts its own container.
export TESTCONTAINERS_SESSION_ID=${TESTCONTAINERS_SESSION_ID:-koinon-mutation-$$-$RANDOM$RANDOM}
pg_pid=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() { [ -z "$pg_pid" ] || { kill "$pg_pid" 2>/dev/null; wait "$pg_pid" 2>/dev/null; } || true; }
trap cleanup EXIT
if [ -z "${KOINON_PGTEST_DSN:-}" ]; then
  go build -o "$out/bin/pgserver" ./hack/pgserver
  rm -f "$out/pg.dsn"
  "$out/bin/pgserver" -dsn-file "$out/pg.dsn" >"$out/logs/pgserver.log" 2>&1 &
  pg_pid=$!
  while [ ! -s "$out/pg.dsn" ] && kill -0 "$pg_pid" 2>/dev/null; do sleep 0.2; done
  if [ -s "$out/pg.dsn" ]; then
    KOINON_PGTEST_DSN=$(<"$out/pg.dsn"); export KOINON_PGTEST_DSN
  else
    pg_pid=""; echo "mutation.sh: no shared Postgres (see $out/logs/pgserver.log); each test process starts its own or skips" >&2
  fi
fi

in_scope() { # file line
  local f=$1 l=$2 spec lf range
  if [ ${#lines[@]} -gt 0 ]; then
    local hit=0
    for spec in "${lines[@]}"; do
      lf=${spec%%:*}; range=${spec#*:}
      if [ "$lf" = "$f" ] && [ "$l" -ge "${range%-*}" ] && [ "$l" -le "${range#*-}" ]; then hit=1; fi
    done
    [ $hit = 1 ] || return 1
  fi
  if [ -n "$diff_base" ]; then
    awk -v f="$f" -v l="$l" '$1 == f && l >= $2 && l <= $3 { found = 1 } END { exit !found }' <<<"$changed" || return 1
  fi
  return 0
}

# run_one <pkg> <file> <line> <mutator> <mutant>: test one mutant, append a results row.
# shellcheck disable=SC2329 # invoked through xargs (export -f)
run_one() {
  local pkg=$1 file=$2 line=$3 mutator=$4 mutant=$5 id ov log start rc status
  id=$(basename "$mutant")
  id=${file//\//_}.${id##*.}
  ov=$out/overlay/$id.json
  log=$out/logs/$id.log
  printf '{"Replace":{%s"%s":"%s"}}\n' "$extra_overlay" "$root/$file" "$mutant" >"$ov"
  start=$SECONDS
  rc=0
  # -vet=off: a mutant that only vet rejects is reported as surviving the tests.
  KUBECONFIG=/dev/null go test -vet=off -overlay="$ov" -count=1 -timeout "${timeout}s" "$pkg" >"$log" 2>&1 || rc=$?
  if [ "$rc" = 0 ]; then status=survived
  elif grep -q 'panic: test timed out' "$log"; then status=timeout
  elif grep -qE '\[build failed\]|\[setup failed\]|^# ' "$log"; then status=error
  else status=killed
  fi
  if [ "$status" = survived ]; then
    diff -u --label "$file" --label "$file (mutant)" "$root/$file" "$mutant" >"$out/survivors/$id.diff" || true
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$pkg" "$file" "$line" "$mutator" "$status" "$((SECONDS - start))" "$mutant" >>"$out/results.tsv"
}
extra_overlay=""
for r in "${replaces[@]}"; do
  src=$(cd "$(dirname "${r#*=}")" && pwd)/$(basename "${r#*=}")
  # shellcheck disable=SC2089,SC2090 # a JSON fragment, printed by printf, never word-split
  extra_overlay+="\"$root/${r%%=*}\":\"$src\","
done
printf '{"Replace":{%s}}\n' "${extra_overlay%,}" >"$out/overlay/clean.json"

export -f run_one
# shellcheck disable=SC2090 # extra_overlay is JSON text, see above
export out root timeout extra_overlay

printf 'pkg\tfile\tline\tmutator\tstatus\tseconds\tmutant\n' >"$out/results.tsv"
echo "mutation.sh: results in $out (max-procs $max_procs)"
any_survived=0
summary=()
for pkg in $packages; do
  pkg_start=$SECONDS
  # Non-test files of the package, generated code excluded.
  mapfile -t files < <(go list -f '{{range .GoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' "$pkg" | sed "s|^$root/||" | while read -r f; do
    [ -n "$f" ] && ! grep -qE '^// Code generated .* DO NOT EDIT\.$' "$f" && echo "$f"
  done)
  # Generating costs a full type-check per file: with --lines or --diff, only the files they name.
  if [ ${#lines[@]} -gt 0 ] || [ -n "$diff_base" ]; then
    wanted=" ${lines[*]%%:*} $(awk '{print $1}' <<<"$changed" | tr '\n' ' ') "
    keep=()
    for f in "${files[@]}"; do [[ $wanted == *" $f "* ]] && keep+=("$f"); done
    files=("${keep[@]}")
  fi
  [ ${#files[@]} -gt 0 ] || continue

  # Clean run first: the package must be green, and it sets the per-mutant timeout.
  t0=$SECONDS
  if ! KUBECONFIG=/dev/null go test -vet=off -overlay="$out/overlay/clean.json" -count=1 "$pkg" >"$out/logs/clean.${pkg//\//_}.log" 2>&1; then
    echo "mutation.sh: $pkg fails without mutants; see $out/logs/clean.${pkg//\//_}.log" >&2
    summary+=("$pkg	clean run failed"); continue
  fi
  clean=$((SECONDS - t0))
  if [ -z "$timeout" ]; then timeout=$((clean * 3)); [ "$timeout" -ge 60 ] || timeout=60; fi

  # Generate mutants. --debug prints the mutator before each saved mutation file.
  gen_args=(--no-exec --do-not-remove-tmp-folder --debug)
  for m in "${disables[@]}"; do gen_args+=(--disable "$m"); done
  gen_log=$out/logs/generate.${pkg//\//_}.log
  # It type-checks from the module root and writes report.json into the working directory.
  had_report=0; [ -e report.json ] && had_report=1
  gen_rc=0
  TMPDIR=$out "$out/bin/go-mutesting" "${gen_args[@]}" "${files[@]}" >"$gen_log" 2>&1 || gen_rc=$?
  [ $had_report = 1 ] || rm -f report.json
  if [ $gen_rc != 0 ]; then
    echo "mutation.sh: go-mutesting failed for $pkg; see $gen_log" >&2; summary+=("$pkg	generate failed"); continue
  fi

  # "Mutator <name>" then "Save mutation into "<tmp>/<file>.<n>" with checksum ...".
  queue=$out/queue.${pkg//\//_}
  : >"$queue"
  generated=0
  while IFS=$'\t' read -r mutator mutant; do
    generated=$((generated + 1))
    orig=${mutant%.*}; orig=${orig#*/go-mutesting-*/}; file=${orig#/}; file=${file#"${root#/}"/}
    line=$({ diff "$root/$file" "$mutant" || true; } | sed -n '1s/^\([0-9]*\).*/\1/p')
    in_scope "$file" "${line:-0}" || continue
    printf '%s\t%s\t%s\t%s\t%s\n' "$pkg" "$file" "${line:-0}" "$mutator" "$mutant" >>"$queue"
  done < <(awk '/^Mutator /{m=$2} /Save mutation into/{ if (match($0, /"[^"]+"/)) print m "\t" substr($0, RSTART+1, RLENGTH-2) }' "$gen_log")
  tested=$(wc -l <"$queue" | tr -d ' ')

  tr '\t\n' '\0\0' <"$queue" | xargs -0 -r -n5 -P "$max_procs" bash -c 'run_one "$@"' _

  read -r killed survived timedout errors < <(awk -F'\t' -v p="$pkg" '$1 == p { c[$5]++ } END { print c["killed"]+0, c["survived"]+0, c["timeout"]+0, c["error"]+0 }' "$out/results.tsv")
  [ "$survived" = 0 ] || any_survived=1
  summary+=("$pkg	generated=$generated tested=$tested killed=$killed survived=$survived timeout=$timedout error=$errors clean=${clean}s wall=$((SECONDS - pkg_start))s")
  timeout=$timeout_flag
done

printf '%s\n' "${summary[@]}"
awk -F'\t' 'NR > 1 && $5 == "survived" { print "SURVIVED " $2 ":" $3 " " $4 }' "$out/results.tsv"
exit $any_survived
