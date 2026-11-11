#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/ci-rules-check.sh offline with small fixture trees in a temp dir: one passing tree, and one
# negative case per rule. Also runs the script on the real tree, which must not fail.
# shellcheck disable=SC2015  # ok and bad never fail, so A && ok || bad is an if-else here
# shellcheck disable=SC2016  # fixtures are written with single quotes on purpose

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script="$repo/hack/ci-rules-check.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-rules-check-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

sha=0123456789abcdef0123456789abcdef01234567
n=0

# A passing tree: one workflow with a required job, permissions, a pinned action and a timeout.
good_tree() { # DIR
	mkdir -p "$1/.github/workflows" "$1/hack" "$1/web"
	printf '%s\n' '# SPDX-License-Identifier: Apache-2.0' 'aggregator go' 'quick checks' 'advisory flavors' >"$1/hack/ci-required.txt"
	cat >"$1/.github/workflows/ci.yml" <<EOF
name: ci
on:
  pull_request:
    types: [opened]
  merge_group:
    types: [checks_requested]
permissions:
  contents: read
concurrency:
  group: ci-\${{ github.sha }}
  cancel-in-progress: false
jobs:
  go:
    name: go
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - uses: actions/checkout@$sha # v7.0.1
      - uses: ./.github/actions/local
  quick-checks:
    name: quick checks
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - run: true
EOF
	printf 'export default {\n  retries: 0,\n};\n' >"$1/web/playwright.config.ts"
}

# case_ NAME WANT_RC WANT_LINE MUTATION: build a good tree, apply MUTATION (a shell snippet using $r), run.
case_() {
	n=$((n + 1))
	local r=$tmp/r$n out rc
	good_tree "$r"
	eval "$4"
	out=$("$script" "$r" 2>&1)
	rc=$?
	if [[ $rc -ne $2 ]]; then bad "$1: exit $rc, want $2 ($out)"; return; fi
	if grep -q -F "$3" <<<"$out"; then ok "$1"; else bad "$1: no line '$3' in: $out"; fi
}

case_ "a good tree passes every rule" 0 'rule=Q1 verdict=pass' ':'
case_ "a good tree: T1 passes" 0 'rule=T1 verdict=pass' ':'
case_ "a good tree: T7 passes" 0 'rule=T7 verdict=pass' ':'

# W1
case_ "W1: a workflow without top-level permissions fails" 1 'rule=W1 verdict=fail' \
	'printf "name: x\non: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n" >"$r/.github/workflows/other.yml"'
case_ "W1: a job-level permissions key is not top-level" 1 'rule=W1 verdict=fail' \
	'printf "name: x\non: push\njobs:\n  a:\n    permissions:\n      contents: read\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n" >"$r/.github/workflows/other.yml"'
case_ "W1: permissions: {} counts" 0 'rule=W1 verdict=pass' \
	'printf "name: x\non: push\npermissions: {}\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n" >"$r/.github/workflows/other.yml"'

# W2
case_ "W2: an action pinned by a tag fails" 1 'rule=W2 verdict=fail' \
	'sed -i.bak "s|actions/checkout@$sha|actions/checkout@v4|" "$r/.github/workflows/ci.yml"'
case_ "W2: an action pinned by a short sha fails" 1 'rule=W2 verdict=fail' \
	'sed -i.bak "s|actions/checkout@$sha|actions/checkout@0123456|" "$r/.github/workflows/ci.yml"'
case_ "W2: a commented unpinned action is ignored" 0 'rule=W2 verdict=pass' \
	'printf "      # - uses: actions/checkout@v4\n" >>"$r/.github/workflows/ci.yml"'

# W5
case_ "W5: a secret besides GITHUB_TOKEN fails" 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ secrets.OPENROUTER_API_KEY }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: inherit fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: inherit\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: double-quoted inherit fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: \"inherit\"\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: 'inherit' fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: '"'"'inherit'"'"'\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: > block scalar fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: >\n      inherit\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: >- block scalar fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: >-\n      inherit\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: | block scalar fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: |\n      inherit\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: a flow map with a secret fails" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: { K: \${{ secrets.OPENROUTER_API_KEY }} }\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: a flow map with only GITHUB_TOKEN fails too (use a block map)" 1 'rule=W5 verdict=fail' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets: { T: x }\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: a block map with only GITHUB_TOKEN passes" 0 'rule=W5 verdict=pass' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets:\n      T: \${{ secrets.GITHUB_TOKEN }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets: with a comment then a map passes" 0 'rule=W5 verdict=pass' \
	'printf "  call:\n    uses: ./.github/workflows/x.yml\n    secrets:  # explicit\n      T: \${{ secrets.GITHUB_TOKEN }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: GITHUB_TOKEN passes" 0 'rule=W5 verdict=pass' \
	'printf "      - run: echo\n        env:\n          T: \${{ secrets.GITHUB_TOKEN }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets['NAME'] fails" 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ secrets['"'"'OPENROUTER_API_KEY'"'"'] }}\n" >>"$r/.github/workflows/ci.yml"'
case_ 'W5: secrets["NAME"] fails' 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ secrets[\"OPENROUTER_API_KEY\"] }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets[expression] fails" 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ secrets[env.NAME] }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: toJSON(secrets) fails" 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ toJSON(secrets) }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: toJson(secrets) fails" 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ toJson(secrets) }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: a bare secrets object passed to a function fails" 1 'rule=W5 verdict=fail' \
	'printf "      - run: echo\n        env:\n          K: \${{ format('"'"'{0}'"'"', secrets) }}\n" >>"$r/.github/workflows/ci.yml"'
case_ "W5: secrets['GITHUB_TOKEN'] passes" 0 'rule=W5 verdict=pass' \
	'printf "      - run: echo\n        env:\n          T: \${{ secrets['"'"'GITHUB_TOKEN'"'"'] }}\n" >>"$r/.github/workflows/ci.yml"'

# Q1
case_ "Q1: a workflow-level paths filter on a required workflow fails" 1 'rule=Q1 verdict=fail' \
	'sed -i.bak "s|^    types: \[opened\]|    types: [opened]\n    paths:\n      - \"**.go\"|" "$r/.github/workflows/ci.yml"'
case_ "Q1: paths-ignore fails too" 1 'rule=Q1 verdict=fail' \
	'sed -i.bak "s|^    types: \[opened\]|    types: [opened]\n    paths-ignore:\n      - \"docs/**\"|" "$r/.github/workflows/ci.yml"'
case_ "Q1: a paths filter on a workflow with no required check is fine" 0 'rule=Q1 verdict=pass' \
	'printf "name: o\non:\n  pull_request:\n    paths:\n      - x\npermissions:\n  contents: read\njobs:\n  o:\n    name: optional thing\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/o.yml"'
case_ "Q1: a required name with parentheses is matched as plain text" 1 'rule=Q1 verdict=fail' \
	'printf "escape (admission)\n" >>"$r/hack/ci-required.txt"; printf "name: e\non:\n  pull_request:\n    paths:\n      - x\npermissions:\n  contents: read\njobs:\n  e:\n    name: escape (admission)\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/e.yml"'
case_ "Q1: a double-quoted required name is matched" 1 'rule=Q1 verdict=fail' \
	'printf "escape (admission)\n" >>"$r/hack/ci-required.txt"; printf "name: e\non:\n  pull_request:\n    paths:\n      - x\npermissions:\n  contents: read\njobs:\n  e:\n    name: \"escape (admission)\"  # note\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/e.yml"'
case_ "Q1: a single-quoted required name is matched" 1 'rule=Q1 verdict=fail' \
	'printf "escape (admission)\n" >>"$r/hack/ci-required.txt"; printf "name: e\non:\n  pull_request:\n    paths:\n      - x\npermissions:\n  contents: read\njobs:\n  e:\n    name: '"'"'escape (admission)'"'"'\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/e.yml"'
case_ "Q1: a job with no name is matched by its id" 1 'rule=Q1 verdict=fail' \
	'printf "escape-admission\n" >>"$r/hack/ci-required.txt"; printf "name: e\non:\n  pull_request:\n    paths:\n      - x\npermissions:\n  contents: read\njobs:\n  escape-admission:\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/e.yml"'
case_ "Q1: a job-level if is fine" 0 'rule=Q1 verdict=pass' \
	'sed -i.bak "s|^    name: go|    name: go\n    if: \${{ github.event_name == '"'"'push'"'"' }}|" "$r/.github/workflows/ci.yml"'

# Q6
case_ "Q6: cancel-in-progress true on a required workflow fails" 1 'rule=Q6 verdict=fail' \
	'sed -i.bak "s|cancel-in-progress: false|cancel-in-progress: true|" "$r/.github/workflows/ci.yml"'
case_ "Q6: an expression without pull_request fails" 1 'rule=Q6 verdict=fail' \
	'sed -i.bak "s|cancel-in-progress: false|cancel-in-progress: \${{ github.ref != '"'"'refs/heads/main'"'"' }}|" "$r/.github/workflows/ci.yml"'
case_ "Q6: an expression naming pull_request passes" 0 'rule=Q6 verdict=pass' \
	'sed -i.bak "s|cancel-in-progress: false|cancel-in-progress: \${{ github.event_name == '"'"'pull_request'"'"' }}|" "$r/.github/workflows/ci.yml"'
case_ "Q6: a pull_request not-equal expression fails" 1 'rule=Q6 verdict=fail' \
	'sed -i.bak "s|cancel-in-progress: false|cancel-in-progress: \${{ github.event_name != '"'"'pull_request'"'"' }}|" "$r/.github/workflows/ci.yml"'
case_ "Q6: pull_request inside a longer expression passes" 0 'rule=Q6 verdict=pass' \
	'sed -i.bak "s|cancel-in-progress: false|cancel-in-progress: \${{ github.event_name == '"'"'pull_request'"'"' \&\& github.ref != '"'"'refs/heads/main'"'"' }}|" "$r/.github/workflows/ci.yml"'
case_ "Q6: cancel-in-progress true on a workflow without a required check passes" 0 'rule=Q6 verdict=pass' \
	'printf "name: o\non: push\npermissions:\n  contents: read\nconcurrency:\n  group: g\n  cancel-in-progress: true\njobs:\n  o:\n    name: optional thing\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n" >"$r/.github/workflows/o.yml"'

case_ "Q6: cancel-in-progress true on a double-quoted required name fails" 1 'rule=Q6 verdict=fail' \
	'printf "escape (admission)\n" >>"$r/hack/ci-required.txt"; printf "name: e\non: push\npermissions:\n  contents: read\nconcurrency:\n  group: g\n  cancel-in-progress: true\njobs:\n  e:\n    name: \"escape (admission)\"\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/e.yml"'
case_ "Q6: cancel-in-progress true on a single-quoted required name fails" 1 'rule=Q6 verdict=fail' \
	'printf "escape (admission)\n" >>"$r/hack/ci-required.txt"; printf "name: e\non: push\npermissions:\n  contents: read\nconcurrency:\n  group: g\n  cancel-in-progress: true\njobs:\n  e:\n    name: '"'"'escape (admission)'"'"'\n    runs-on: ubuntu-latest\n    timeout-minutes: 5\n    steps:\n      - run: true\n" >"$r/.github/workflows/e.yml"'

# T1
case_ "T1: ginkgo flake attempts in a script fail" 1 'rule=T1 verdict=fail' \
	'printf "go tool ginkgo --flake-attempts=3 ./...\n" >"$r/hack/go-test.sh"'
case_ "T1: FlakeAttempts in Go code fails" 1 'rule=T1 verdict=fail' \
	'mkdir -p "$r/internal/x"; printf "var _ = It(\"x\", FlakeAttempts(2), func() {})\n" >"$r/internal/x/x_test.go"'
case_ "T1: a retry action in a workflow fails" 1 'rule=T1 verdict=fail' \
	'printf "      - uses: nick-fields/retry@$sha\n" >>"$r/.github/workflows/ci.yml"'
case_ "T1: Playwright retries above 0 fails" 1 'rule=T1 verdict=fail' \
	'printf "export default {\n  retries: process.env.CI ? 1 : 0,\n};\n" >"$r/web/playwright.config.ts"'
case_ "T1: Playwright retries 0 passes" 0 'rule=T1 verdict=pass' \
	'printf "export default {\n  retries: 0,\n};\n" >"$r/web/playwright.config.ts"'
case_ "T1: the word in docs is ignored" 0 'rule=T1 verdict=pass' \
	'mkdir -p "$r/docs"; printf "no flake-attempts please\n" >"$r/docs/x.md"'

# T2
case_ "T2: a waiver in its date passes" 0 'rule=T2 verdict=pass' \
	'mkdir -p "$r/internal/x"; printf "// flaky-waiver: #123 until 2999-01-01\n" >"$r/internal/x/x_test.go"'
case_ "T2: a waiver past its date fails" 1 'rule=T2 verdict=fail' \
	'mkdir -p "$r/internal/x"; printf "// flaky-waiver: #123 until 2000-01-01\n" >"$r/internal/x/x_test.go"'
case_ "T2: a waiver without a date fails" 1 'rule=T2 verdict=fail' \
	'mkdir -p "$r/internal/x"; printf "// flaky-waiver: #123\n" >"$r/internal/x/x_test.go"'
case_ "T2: a waiver without an issue fails" 1 'rule=T2 verdict=fail' \
	'mkdir -p "$r/internal/x"; printf "// flaky-waiver: until 2999-01-01\n" >"$r/internal/x/x_test.go"'

# T7
case_ "T7: a job without timeout-minutes is reported, not failed" 0 'rule=T7 verdict=report' \
	'sed -i.bak "/timeout-minutes: 5/{x;s/^/x/;/^x\{1\}$/{x;d;};x;}" "$r/.github/workflows/ci.yml"'
case_ "T7: defers to runner-lint.sh when it has the rule" 0 'rule=T7 verdict=pass' \
	'printf "grep -q timeout-minutes \"\$f\"\n" >"$r/hack/runner-lint.sh"; sed -i.bak "/timeout-minutes: 5/d" "$r/.github/workflows/ci.yml"'
case_ "T7: the word only in a comment of runner-lint.sh does not defer" 0 'rule=T7 verdict=report' \
	'printf "# timeout-minutes rule\n" >"$r/hack/runner-lint.sh"; sed -i.bak "/timeout-minutes: 5/d" "$r/.github/workflows/ci.yml"'

# The real tree must not fail.
out=$("$script" "$repo" 2>&1)
rc=$?
if [[ $rc -eq 0 ]]; then ok "the repository itself passes ($(grep -c '^rule=' <<<"$out") rules)"; else bad "the repository fails: $out"; fi

exit "$fail"
