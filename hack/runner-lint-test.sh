#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/runner-lint.sh offline with fixture trees in a temp dir.
# shellcheck disable=SC2015  # ok and bad never fail, so A && ok || bad is an if-else here
# shellcheck disable=SC2016  # fixtures are written with single quotes on purpose

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/runner-lint.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/runner-lint-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

n=0
# case_ <name> <want rc> <file under root> <content (printf format)>
case_() {
	n=$((n + 1))
	local r=$tmp/r$n out rc
	mkdir -p "$r/$(dirname "$3")"
	printf '%b\n' "$4" >"$r/$3"
	out=$("$script" "$r" 2>&1)
	rc=$?
	if [[ $rc -eq $2 ]]; then ok "$1"; else bad "$1: exit $rc, want $2 ($out)"; fi
}

W=.github/workflows/x.yml
case_ "ubuntu-latest"                 0 $W '    runs-on: ubuntu-latest'
case_ "ubuntu-24.04"                  0 $W '    runs-on: ubuntu-24.04'
case_ "matrix expression"             0 $W '    runs-on: ${{ matrix.os }}'
case_ "self-hosted scalar"            1 $W '    runs-on: self-hosted'
case_ "self-hosted flow list"         1 $W '    runs-on: [self-hosted, linux]'
case_ "self-hosted block list"        1 $W '    runs-on:\n      - linux\n      - self-hosted'
case_ "self-hosted capitalised"       1 $W '    runs-on: Self-Hosted'
case_ "blacksmith label"              1 $W '    runs-on: blacksmith-4vcpu-ubuntu-2404'
case_ "blacksmith in a matrix"        1 $W '        os: [blacksmith-2vcpu-ubuntu-2204]'
case_ "runs-on from vars"             1 $W '    runs-on: ${{ vars.RUNNER }}'
case_ "runs-on from inputs"           1 $W '    runs-on: ${{ inputs.runner }}'
case_ "runs-on from the event"        1 $W '    runs-on: ${{ github.event.inputs.r }}'
case_ "runs-on from needs"            1 $W '    runs-on: ${{ needs.a.outputs.r }}'
case_ "runs-on from head_ref"         1 $W '    runs-on: ${{ github.head_ref }}'
case_ "runs-on from ref_name"         1 $W '    runs-on: ${{ github.ref_name }}'
case_ "runs-on from actor"            1 $W '    runs-on: ${{ github.actor }}'
case_ "runs-on from triggering_actor" 1 $W '    runs-on: ${{ github.triggering_actor }}'
case_ "github.event_name is fine"     0 $W '    runs-on: ${{ github.event_name == '"'"'push'"'"' && '"'"'ubuntu-latest'"'"' || '"'"'ubuntu-24.04'"'"' }}'
case_ "block form labels from vars"   1 $W '    runs-on:\n      labels: ${{ vars.X }}'
case_ "block form group from inputs"  1 $W '    runs-on:\n      group: ${{ inputs.g }}\n      labels: ubuntu-latest'
case_ "block form plain labels"       0 $W '    runs-on:\n      labels: ubuntu-latest\n    env:\n      A: ${{ vars.X }}'
case_ "comment mentions self-hosted"  0 $W '    # never self-hosted here\n    runs-on: ubuntu-latest'
case_ "trailing comment"              0 $W '    runs-on: ubuntu-latest # not self-hosted'
case_ "a word that only contains it"  0 $W '    name: notself-hosted-ish'
case_ "yaml extension"                1 .github/workflows/y.yaml '    runs-on: self-hosted'
case_ "actionlint config label"       1 .github/actionlint.yaml 'self-hosted-runner:\n  labels: [my-box]'
case_ "no workflows at all"           0 README.md 'x'

# The real tree is clean.
out=$("$script" 2>&1) && ok "the repository is clean" || bad "the repository: $out"

exit "$fail"
