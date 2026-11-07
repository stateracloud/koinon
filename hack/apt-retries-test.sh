#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Every apt step in a Dockerfile must retry and time out (#149): snapshot.ubuntu.com answers 5xx
# now and then and a build with no retries fails on it. Each `apt-get update` or `apt-get install`
# carries `-o Acquire::Retries=5 -o Acquire::http::Timeout=30`, and every `update` also
# `-o APT::Update::Error-Mode=any` (#141: without it a failed fetch only warns and the build fails
# later with "Unable to locate package"); a snapshot.ubuntu.com step must print an outage line when
# its update fails. A step that runs apt through another tool (`playwright install --with-deps`)
# writes an apt.conf.d snippet with all three settings instead. `hack/apt-retries-test.sh --self-test` checks the checker against bad input.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)

# check FILE: prints one line per violation. A RUN is the instruction joined across backslash
# continuations; comment lines inside it are dropped.
check() {
  awk '
    function flush(   rest, tool, cmd) {
      if (instr == "") return
      if (instr ~ /^RUN[ \t]/) {
        rest = instr
        # `apt` and `apt-get` as whole words (not apt-cache, aptitude, /etc/apt/ or apt.conf.d)
        while (match(rest, /(^|[^A-Za-z0-9_\/.-])apt(-get)?[ \t]+/)) {
          tool = substr(rest, RSTART, RLENGTH)
          sub(/^[^a]*/, "", tool)
          sub(/[ \t]+$/, "", tool)
          # cmd is the options and subcommand of this one call
          cmd = substr(rest, RSTART + RLENGTH)
          rest = cmd
          sub(/[;&|].*/, "", cmd)
          if (cmd ~ /(^|[ \t])update([ \t]|$)/ && cmd !~ /APT::Update::Error-Mode=any|--error-on=any/)
            printf "%s:%d: %s update without -o APT::Update::Error-Mode=any (a fetch error would only warn): %s %s\n", FILENAME, start, tool, tool, cmd
          if (cmd !~ /-o Acquire::Retries=5/ || cmd !~ /-o Acquire::http::Timeout=30/)
            printf "%s:%d: %s without -o Acquire::Retries=5 -o Acquire::http::Timeout=30: %s %s\n", FILENAME, start, tool, tool, cmd
        }
        if (instr ~ /--with-deps/ && (instr !~ /Acquire::Retries "5"/ || instr !~ /Acquire::http::Timeout "30"/ || instr !~ /APT::Update::Error-Mode "any"/))
          printf "%s:%d: --with-deps runs apt without an apt.conf.d snippet setting Acquire::Retries, Acquire::http::Timeout and APT::Update::Error-Mode\n", FILENAME, start
        # a snapshot.ubuntu.com step must name the outage when its update fails (#141)
        if (instr ~ /snapshot\.ubuntu\.com/ && instr ~ /apt-get/ && instr !~ /unreachable/)
          printf "%s:%d: snapshot.ubuntu.com apt step without an outage message (|| echo \"snapshot.ubuntu.com unreachable ...\" >&2; exit 1)\n", FILENAME, start
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

self_test() {
  local tmp
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/apt-retries-test.XXXXXX")
  trap 'rm -rf "$tmp"' RETURN
  printf 'FROM x\nRUN apt-get update \\\n && apt-get install -y curl\n' >"$tmp/bad"
  printf 'FROM x\nRUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update \\\n && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y curl\n' >"$tmp/good"
  printf 'FROM x\nRUN npx playwright install --with-deps chromium\n' >"$tmp/deps"
  printf 'FROM x\nRUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update; apt-get install -y curl\n' >"$tmp/second"
  printf 'FROM x\nRUN apt install -y curl\n' >"$tmp/plain"
  printf 'FROM x\nRUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 install -y curl\n' >"$tmp/goodget"
  printf 'FROM x\nRUN echo x > /etc/apt/apt.conf.d/80retries && apt-cache policy curl\n' >"$tmp/conf"
  [ "$(check "$tmp/plain" | wc -l)" -eq 1 ] || { echo "self-test: plain apt not flagged" >&2; exit 1; }
  [ -z "$(check "$tmp/goodget")" ] || { echo "self-test: good apt-get flagged" >&2; exit 1; }
  [ -z "$(check "$tmp/conf")" ] || { echo "self-test: apt.conf.d path or apt-cache flagged" >&2; exit 1; }
  [ "$(check "$tmp/bad" | wc -l)" -eq 3 ] || { echo "self-test: bad file should give 3 violations" >&2; exit 1; }
  [ -z "$(check "$tmp/good")" ] || { echo "self-test: good file flagged" >&2; exit 1; }
  [ "$(check "$tmp/deps" | wc -l)" -eq 1 ] || { echo "self-test: --with-deps without snippet not flagged" >&2; exit 1; }
  printf 'FROM x\nRUN apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 update\n' >"$tmp/nomode"
  printf 'FROM x\nRUN echo https://snapshot.ubuntu.com/ubuntu/x/ && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update\n' >"$tmp/nomsg"
  printf 'FROM x\nRUN echo https://snapshot.ubuntu.com/ubuntu/x/ && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update || { echo "snapshot.ubuntu.com unreachable" >&2; exit 1; }\n' >"$tmp/msg"
  [ "$(check "$tmp/nomode" | wc -l)" -eq 1 ] || { echo "self-test: update without Error-Mode not flagged" >&2; exit 1; }
  [ "$(check "$tmp/nomsg" | wc -l)" -eq 1 ] || { echo "self-test: snapshot step without outage message not flagged" >&2; exit 1; }
  [ -z "$(check "$tmp/msg")" ] || { echo "self-test: snapshot step with message flagged" >&2; exit 1; }
  [ "$(check "$tmp/second" | wc -l)" -eq 1 ] || { echo "self-test: second apt-get on a line not flagged" >&2; exit 1; }
  printf 'FROM x\nRUN apt-get -qq -o Acquire::Retries=5 -o Acquire::http::Timeout=30 update\n' >"$tmp/quiet"
  printf 'FROM x\nRUN apt-get -qq -o Acquire::Retries=5 -o Acquire::http::Timeout=30 -o APT::Update::Error-Mode=any update\n' >"$tmp/quietmode"
  [ "$(check "$tmp/quiet" | wc -l)" -eq 1 ] || { echo "self-test: update with -qq and no Error-Mode not flagged" >&2; exit 1; }
  [ -z "$(check "$tmp/quietmode")" ] || { echo "self-test: update with -qq and Error-Mode flagged" >&2; exit 1; }
  echo "apt-retries-test self-test: ok"
}

# The checker is proven on bad input before it judges the tree, so a green run never rests on a
# broken checker (and the self-test needs mktemp, which the run then stops without).
self_test
[ "${1:-}" != --self-test ] || exit 0

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
