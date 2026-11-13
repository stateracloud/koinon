#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs the docs guides' example commands against a build of this checkout (issue #124, ADR 0108).
#
# The docs site is stateracloud/koinon-docs. Its guide pages show shell blocks whose info string is
# `sh e1=<name>`; this repository owns the commands, in command files (hack/docs-e1.sh). The quickstart
# and the backup guide need a cluster and run in test/e2e. A guide whose examples need nothing but the
# Koinon binaries puts its file under test/e2e/commands/guides/, and this script runs every block of
# every such file, so a command that stops working on a pull request fails the pull request.
#
#   hack/docs-examples.sh               build the binaries, run every block of every guide file
#   hack/docs-examples.sh --no-build    use the binaries already in $KOINON_BIN_DIR
#   hack/docs-examples.sh --list        print "<file> <block>" for each block, run nothing
#
# Each block runs through `docs-e1.sh run` (bash -euo pipefail) in its own empty directory, with the
# built binaries first on PATH and KUBECONFIG=/dev/null, so an example that reaches for a cluster fails.
# It fails (exit 1) when any block fails or when there is no block at all (a check that runs nothing
# must not pass). Exit 2 on a usage or build error.
#
# Environment:
#   DOCS_EXAMPLES_DIR   directory of guide command files (default test/e2e/commands/guides)
#   KOINON_BIN_DIR      where the binaries are built, or are found with --no-build (default: a temp dir)
#   DOCS_EXAMPLES_TIMEOUT  seconds one block may run (default 120)
#
# What a guide file must hold: only blocks that run offline with the binaries. A block that needs a
# cluster, a model or the network belongs to the e2e command files, not here.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
dir=${DOCS_EXAMPLES_DIR:-$root/test/e2e/commands/guides}
timeout_s=${DOCS_EXAMPLES_TIMEOUT:-120}
# The binaries a guide example may call, built from cmd/<name>.
BINARIES=(koinon-api)

build=1
list=0
for a in "$@"; do
  case $a in
  --no-build) build=0 ;;
  --list) list=1 ;;
  *) echo "usage: docs-examples.sh [--no-build] [--list]" >&2; exit 2 ;;
  esac
done

shopt -s nullglob
files=("$dir"/*.e1)
[[ ${#files[@]} -gt 0 ]] || { echo "docs-examples: no *.e1 file in $dir" >&2; exit 1; }

if [[ $list -eq 1 ]]; then
  for f in "${files[@]}"; do
    for n in $(E1_FILE=$f "$root/hack/docs-e1.sh" names); do echo "${f##*/} $n"; done
  done
  exit 0
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/docs-examples.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
bin=${KOINON_BIN_DIR:-$tmp/bin}
if [[ $build -eq 1 ]]; then
  command -v go >/dev/null || { echo "docs-examples: go is not on PATH (use --no-build with KOINON_BIN_DIR)" >&2; exit 2; }
  mkdir -p "$bin"
  for b in "${BINARIES[@]}"; do
    echo "docs-examples: building $b"
    (cd "$root" && go build -o "$bin/$b" "./cmd/$b") || { echo "docs-examples: building $b failed" >&2; exit 2; }
  done
fi
for b in "${BINARIES[@]}"; do
  [[ -x $bin/$b ]] || { echo "docs-examples: $bin/$b is not an executable (build it, or drop --no-build)" >&2; exit 2; }
done

pass=0
failed=()
for f in "${files[@]}"; do
  names=$(E1_FILE=$f "$root/hack/docs-e1.sh" names) || { echo "FAIL: reading the e1 blocks of $f" >&2; failed+=("${f##*/}"); continue; }
  for n in $names; do
    w=$(mktemp -d "$tmp/work.XXXXXX")
    label="${f##*/} $n"
    if env "PATH=$bin:$PATH" KUBECONFIG=/dev/null E1_FILE="$f" E1_WORKDIR="$w" \
      timeout "$timeout_s" "$root/hack/docs-e1.sh" run "$n" >"$tmp/out" 2>&1; then
      echo "ok: $label"
      pass=$((pass + 1))
    else
      echo "FAIL: $label" >&2
      sed 's/^/    /' "$tmp/out" >&2
      failed+=("$label")
    fi
  done
done

if [[ $((pass + ${#failed[@]})) -eq 0 ]]; then
  echo "docs-examples: no e1 block in ${#files[@]} file(s) under $dir" >&2
  exit 1
fi
if [[ ${#failed[@]} -gt 0 ]]; then
  echo "docs-examples: ${#failed[@]} example(s) failed, $pass passed: ${failed[*]}" >&2
  exit 1
fi
echo "docs-examples: all $pass example(s) passed"
