#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks the cache ownership rule of the workflows (old issue 147, docs/dev/ci.md#cache-ownership):
# main's `warm-caches` job saves the Go and envtest caches, every other job in ci.yml only
# restores them, there is one Go cache scheme (`go-ci`), `go-vet` is gone, and the e2e Go key has no run id.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wf=${1:-$repo/.github/workflows/ci.yml}
e2e=${2:-$repo/.github/workflows/e2e-kind.yml}

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }

# job_body FILE JOB: the lines of one top-level job.
job_body() { awk -v j="  $2:" '$0 == j { f = 1; next } f && /^  [a-zA-Z0-9_-]+:/ { exit } f' "$1"; }
# shellcheck disable=SC2329 # called through check
has() { grep -Eq -- "$2" <<<"$1"; }
# shellcheck disable=SC2329
hasnt() { ! grep -Eq -- "$2" <<<"$1"; }

warm=$(job_body "$wf" warm-caches)
check "warm-caches exists" test -n "$warm"
check "warm-caches runs on pushes to main only" has "$warm" "if: github.event_name == 'push' && github.ref == 'refs/heads/main'"
check "warm-caches does not wait for changes (never skipped by ci-skip-heavy)" hasnt "$warm" 'needs:'
check "warm-caches looks up with lookup-only" has "$warm" 'lookup-only: true'
warmweb=$(job_body "$wf" warm-caches-web)
for k in 'go-ci-' 'envtest-'; do
  check "warm-caches saves $k" has "$warm" "key: ${k}"
  check "warm-caches saves $k only on a miss" has "$warm" "cache-hit != 'true'"
done
check "warm-caches uses actions/cache/save" has "$warm" 'actions/cache/save@'
check "warm-caches-web saves no Playwright cache (web-e2e runs in the Playwright image, #569)" hasnt "$warmweb" 'playwright'
check "warm-caches-web runs on pushes to main only" has "$warmweb" "if: github.event_name == 'push' && github.ref == 'refs/heads/main'"
check "warm-caches-web does not wait for changes" hasnt "$warmweb" 'needs:'
check "warm-caches-web is advisory" grep -qx "advisory warm-caches-web" "$repo/hack/ci-required.txt"

# setup-node keys its pnpm cache on the hash of every cache-dependency-path entry, so each lockfile
# list a consumer uses must be warmed by one job with exactly that list (a single path per warm job).
check "warm-caches-web warms web/pnpm-lock.yaml alone" has "$warmweb" '^          cache-dependency-path: web/pnpm-lock.yaml$'
warmpaths=$(printf '%s\n' "$warmweb" | grep -E '^          cache-dependency-path: ' | sort -u)
while read -r job path; do
  [ "$job" = warm-caches-web ] && continue
  if [ "$path" = '|' ] || [ -z "$path" ]; then continue; fi
  check "consumer $job cache-dependency-path '$path' is warmed" has "$warmpaths" "cache-dependency-path: ${path}\$"
done < <(awk '/^  [a-zA-Z0-9_-]+:/ { job = $1 } /cache-dependency-path:/ { sub(/.*cache-dependency-path: */, ""); print job, $0 }' "$wf")

# No job but warm-caches and flavor-publish/flavors (their own layer cache) may use the saving form.
saving=$(awk '/^  [a-zA-Z0-9_-]+:/ { job = $1 } /uses: actions\/cache@/ { print job }' "$wf")
check "no ci.yml job restores-and-saves with actions/cache@" test -z "$saving"
saves=$(awk '/^  [a-zA-Z0-9_-]+:/ { job = $1 } /uses: actions\/cache\/save@/ { print job }' "$wf" | sort -u | tr '\n' ' ')
check "only the warm-caches jobs and the flavor jobs save a cache (got: $saves)" test "$saves" = "flavor-publish: flavors: warm-caches: "

# One Go cache scheme: every setup-go in ci.yml has cache: false (the go-ci restore is explicit).
total=$(grep -c 'uses: actions/setup-go@' "$wf")
off=$(awk '/uses: actions\/setup-go@/ { f = 1; next } f && /cache: false/ { n++; f = 0 } f && /^      - / { f = 0 } END { print n + 0 }' "$wf")
check "every setup-go in ci.yml has cache: false ($off of $total)" test "$total" = "$off"
for j in generate chart chart-smoke escape-admission scripts flavors; do
  check "$j restores go-ci" has "$(job_body "$wf" "$j")" 'key: go-ci-'
done

check "go-vet is not a job" test -z "$(job_body "$wf" go-vet)"
check "go-vet is not a need of go" hasnt "$(job_body "$wf" go)" 'go-vet'
check "go-vet is not in ci-required.txt" hasnt "$(cat "$repo/hack/ci-required.txt")" 'go-vet'
check "warm-caches is advisory" grep -qx 'advisory warm-caches' "$repo/hack/ci-required.txt"
check "golangci-lint default set still runs govet (standard)" grep -Eq '^  default: standard' "$repo/.golangci.yml"
check "go-lint does not save the lint cache" has "$(job_body "$wf" go-lint)" 'skip-save-cache: true'
# The lint action's cache has a time-interval key: warm-caches must run it on every push to main,
# so the step carries no go-ci miss condition.
check "warm-caches runs golangci-lint on every push (no if:)" test "$(awk '/^      - /{blk=$0} /golangci-lint-action@/{print prev} {prev=$0}' <<<"$warm" | grep -c 'if:')" = 0
check "warm-caches runs golangci-lint" has "$warm" 'golangci-lint-action@'

# The envtest key follows the Makefile's version.
ver=$(sed -n 's/^ENVTEST_K8S_VERSION ?= //p' "$repo/Makefile")
check "envtest keys are envtest-$ver" test "$(grep -c "key: envtest-$ver\$" "$wf")" = 3
check "no other envtest key" test "$(grep -c 'key: envtest-' "$wf")" = 3

# The e2e Go cache: one entry per go.sum, no run id, saved only when the exact key was missing.
check "e2e-go key has no run id" hasnt "$(grep 'key: e2e-go-' "$e2e")" 'run_id'
check "e2e-go is saved only on an exact miss" grep -q "steps.e2ego.outputs.cache-hit != 'true'" "$e2e"

# Test results (issue 43): only pull requests reuse them, and main's warm-caches job fills them.
gotest=$(job_body "$wf" go-test)
check "go-test sets KOINON_GO_TEST_CACHE for pull_request runs only" has "$gotest" "KOINON_GO_TEST_CACHE: \\$\\{\\{ github.event_name == 'pull_request' && '1' \\|\\| '' \\}\\}"
check "warm-caches runs the cacheable packages through go-test.sh with the cache switch" has "$warm" 'KOINON_GO_TEST_CACHE: .1.'
check "warm-caches takes the cacheable list from go-test-cacheable.sh" has "$warm" 'pkgs=\$\(hack/go-test-cacheable.sh\)'
check "warm-caches guards an empty cacheable list" has "$warm" '\[ -n "\$pkgs" \]'
check "warm-caches' test run cannot fail the job (continue-on-error)" has "$warm" 'continue-on-error: true'
check "warm-caches saves test results per commit" has "$warm" 'key: go-testres-.*github\.sha'
check "go-test restores test results by prefix on pull requests" has "$gotest" 'go-testres-.*-$'
check "go-ci save does not depend on the test run (comes first, no testres condition)" test "$(awk '/actions\/cache\/save@/{s++} /id: testres/{t=NR} /key: go-ci-/{if(s&&!k){k=NR}} END{print (k&&t&&k<t)?1:0}' <<<"$warm")" = 1
check "warm-caches does not run the cacheable packages with -count=1" hasnt "$warm" 'go-test.sh.*-count=1'
check "no workflow sets KOINON_GO_TEST_CACHE to 1 outside a pull_request condition" test "$(grep -h 'KOINON_GO_TEST_CACHE' "$wf" | grep -vc "pull_request\|warm-caches\|KOINON_GO_TEST_CACHE: '1'\|^ *#")" = 0

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
