#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Every apt step in a Dockerfile must retry and time out (#149): snapshot.ubuntu.com answers 5xx
# now and then and a build with no retries fails on it. Each `apt-get update` or `apt-get install`
# carries `-o Acquire::Retries=5 -o Acquire::http::Timeout=30`; a step that runs apt through
# another tool (`playwright install --with-deps`) writes an apt.conf.d snippet with both settings
# instead. `hack/apt-retries-test.sh --self-test` checks the checker against bad input.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

# check FILE: prints one line per violation. A RUN is the instruction joined across backslash
# continuations; comment lines inside it are dropped.
check() {
  awk '
    function flush(   n, rest, i, parts, cmd) {
      if (instr == "") return
      if (instr ~ /^RUN[ \t]/) {
        rest = instr
        n = split(rest, parts, /apt-get[ \t]+/)
        for (i = 2; i <= n; i++) {
          # parts[i] starts with the options and subcommand of one apt-get call
          cmd = parts[i]
          sub(/[;&|].*/, "", cmd)
          if (cmd !~ /-o Acquire::Retries=5/ || cmd !~ /-o Acquire::http::Timeout=30/)
            printf "%s:%d: apt-get without -o Acquire::Retries=5 -o Acquire::http::Timeout=30: apt-get %s\n", FILENAME, start, cmd
        }
        if (instr ~ /--with-deps/ && (instr !~ /Acquire::Retries "5"/ || instr !~ /Acquire::http::Timeout "30"/))
          printf "%s:%d: --with-deps runs apt without an apt.conf.d snippet setting Acquire::Retries and Acquire::http::Timeout\n", FILENAME, start
      }
      instr = ""
    }
    /^[ \t]*#/ { next }
    {
      if (instr == "") start = FNR
      line = $0
      cont = (line ~ /\\[ \t]*$/)
      sub(/\\[ \t]*$/, "", line)
      instr = (instr == "" ? "" : instr " ") line
      if (!cont) flush()
    }
    END { flush() }
  ' "$1"
}

if [ "${1:-}" = --self-test ]; then
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/apt-retries-test.XXXXXX")
  trap 'rm -rf "$tmp"' EXIT
  printf 'FROM x\nRUN apt-get update \\\n && apt-get install -y curl\n' >"$tmp/bad"
  printf 'FROM x\nRUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 update \\\n && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y curl\n' >"$tmp/good"
  printf 'FROM x\nRUN npx playwright install --with-deps chromium\n' >"$tmp/deps"
  printf 'FROM x\nRUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 update; apt-get install -y curl\n' >"$tmp/second"
  [ "$(check "$tmp/bad" | wc -l)" -eq 2 ] || { echo "self-test: bad file should give 2 violations" >&2; exit 1; }
  [ -z "$(check "$tmp/good")" ] || { echo "self-test: good file flagged" >&2; exit 1; }
  [ "$(check "$tmp/deps" | wc -l)" -eq 1 ] || { echo "self-test: --with-deps without snippet not flagged" >&2; exit 1; }
  [ "$(check "$tmp/second" | wc -l)" -eq 1 ] || { echo "self-test: second apt-get on a line not flagged" >&2; exit 1; }
  echo "apt-retries-test self-test: ok"
  exit 0
fi

cd "$root"
n=0
bad=0
while IFS= read -r f; do
  n=$((n + 1))
  out=$(check "$f")
  if [ -n "$out" ]; then echo "$out" >&2; bad=1; fi
done < <(git ls-files -z -- '*Dockerfile' '*Dockerfile.*' '*.Dockerfile' | tr '\0' '\n')
[ "$n" -gt 0 ] || { echo "apt-retries-test: found no Dockerfiles" >&2; exit 1; }
[ "$bad" -eq 0 ] || exit 1
echo "apt-retries-test: ok ($n Dockerfiles)"
