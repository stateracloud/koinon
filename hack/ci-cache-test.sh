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
action=$repo/.github/actions/setup-go-cache/action.yml
check "warm-caches saves envtest-" has "$warm" "key: envtest-"
check "warm-caches saves envtest- only on a miss" has "$warm" "cache-hit != 'true'"
check "warm-caches saves go-ci with the key of setup-go-cache, only on a miss" has "$warm" 'key: \$\{\{ steps.go.outputs.key \}\}$'
check "warm-caches looks up go-ci through setup-go-cache" has "$warm" 'uses: ./.github/actions/setup-go-cache'
# The go-ci key (weekly, issue 572): one definition for every consumer, with the week as the last part.
check "setup-go-cache has the go-ci key with a week suffix" grep -Eq 'key=\$\{prefix\}\$\{GO_SUM\}-\$\(\( \$\(date -u \+%s\) / 604800 \)\)' "$action"
check "setup-go-cache only restores (never saves)" test -z "$(grep -E 'uses: actions/cache/save|uses: actions/cache@' "$action")"
check "setup-go-cache restore keys fall back to an older week" grep -q 'steps.key.outputs.prefix }}$' "$action"
check "warm-caches uses actions/cache/save" has "$warm" 'actions/cache/save@'
check "warm-caches-web saves no Playwright cache (web-e2e runs in the Playwright image, #569)" hasnt "$warmweb" 'playwright'
check "warm-caches-web runs on pushes to main only" has "$warmweb" "if: github.event_name == 'push' && github.ref == 'refs/heads/main'"
check "warm-caches-web does not wait for changes" hasnt "$warmweb" 'needs:'
check "warm-caches-web is advisory" grep -qx "advisory warm-caches-web" "$repo/hack/ci-required.txt"
warmplug=$(job_body "$wf" warm-caches-opencode-plugins)
check "warm-caches-opencode-plugins runs on pushes to main only" has "$warmplug" "if: github.event_name == 'push' && github.ref == 'refs/heads/main'"
check "warm-caches-opencode-plugins does not wait for changes" hasnt "$warmplug" 'needs:'
check "warm-caches-opencode-plugins is advisory" grep -qx "advisory warm-caches-opencode-plugins" "$repo/hack/ci-required.txt"

# setup-node keys its pnpm cache on the hash of every cache-dependency-path entry, so each lockfile
# list a consumer uses must be warmed by one job with exactly that list (a single path per warm job).
check "warm-caches-web warms web/pnpm-lock.yaml alone" has "$warmweb" '^          cache-dependency-path: web/pnpm-lock.yaml$'
check "warm-caches-opencode-plugins warms harnesses/opencode/plugins/pnpm-lock.yaml alone" test "$(grep -Ec '^          cache-dependency-path: ' <<<"$warmplug")" = 1
warmpaths=$(printf '%s\n%s\n' "$warmweb" "$warmplug" | grep -E '^          cache-dependency-path: ' | sort -u)
while read -r job path; do
  case $job in warm-caches-web* | warm-caches-opencode-plugins*) continue ;; esac
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
  check "$j restores go-ci" has "$(job_body "$wf" "$j")" 'uses: \./\.github/actions/setup-go-cache'
done
for j in go-tidy go-lint go-test go-race-serial go-race-parallel; do
  check "$j restores go-ci" has "$(job_body "$wf" "$j")" 'uses: \./\.github/actions/setup-go-cache'
done
# The one exception is `changes`: it restores the module cache alone, under the key without a week.
check "no job but changes pastes a go-ci key (setup-go-cache defines it)" test "$(grep -c 'key: go-ci-' "$wf")" = 1

check "go-vet is not a job" test -z "$(job_body "$wf" go-vet)"
check "go-vet is not a need of go" hasnt "$(job_body "$wf" go)" 'go-vet'
check "go-vet is not in ci-required.txt" hasnt "$(cat "$repo/hack/ci-required.txt")" 'go-vet'
check "warm-caches is advisory" grep -qx 'advisory warm-caches' "$repo/hack/ci-required.txt"
check "golangci-lint default set still runs govet (standard)" grep -Eq '^  default: standard' "$repo/.golangci.yml"
check "go-lint does not save the lint cache" has "$(job_body "$wf" go-lint)" 'skip-save-cache: true'
# The lint action's cache has a 7-day interval key, and so has the go-ci key (setup-go-cache): the full
# lint runs only on a go-ci miss, which is the first push of a new interval (issue 572).
check "warm-caches runs golangci-lint only on a go-ci miss" test "$(awk '/golangci-lint-action@/{print prev} {prev=$0}' <<<"$warm" | grep -c "cache-hit != 'true'")" = 1
check "warm-caches runs golangci-lint" has "$warm" 'golangci-lint-action@'

# The envtest key follows the Makefile's version.
ver=$(sed -n 's/^ENVTEST_K8S_VERSION ?= //p' "$repo/Makefile")
check "envtest keys are envtest-$ver" test "$(grep -c "key: envtest-$ver\$" "$wf")" = 4
check "no other envtest key" test "$(grep -c 'key: envtest-' "$wf")" = 4
check "escape (admission) restores the envtest cache" has "$(job_body "$wf" escape-admission)" "key: envtest-$ver"

# The e2e Go cache: one entry per go.sum, no run id, saved only when the exact key was missing.
check "e2e-go key has no run id" hasnt "$(grep 'key: e2e-go-' "$e2e")" 'run_id'
check "e2e-go is saved only on an exact miss" grep -q "steps.e2ego.outputs.cache-hit != 'true'" "$e2e"
check "e2e-go is also saved from the nightly schedule" grep -q "github.event_name == 'schedule') && github.ref == 'refs/heads/main' && steps.e2ego.outputs.cache-hit" "$e2e"

# Test results (issue 43): only pull requests reuse them, and main's warm-caches job fills them.
gotest=$(job_body "$wf" go-test)
check "go-test sets KOINON_GO_TEST_CACHE for pull_request runs only" has "$gotest" "KOINON_GO_TEST_CACHE: \\$\\{\\{ github.event_name == 'pull_request' && '1' \\|\\| '' \\}\\}"
check "warm-caches runs the cacheable packages through go-test.sh with the cache switch" has "$warm" 'KOINON_GO_TEST_CACHE: .1.'
check "warm-caches takes the cacheable list from go-test-cacheable.sh" has "$warm" 'pkgs=\$\(hack/go-test-cacheable.sh\)'
# shellcheck disable=SC2016 # a regex for a literal $pkgs, not an expansion
check "warm-caches guards an empty cacheable list" has "$warm" '\[ -n "\$pkgs" \]'
check "warm-caches' test run cannot fail the job (continue-on-error)" has "$warm" 'continue-on-error: true'
check "warm-caches saves test results per commit" has "$warm" 'key: go-testres-.*github\.sha'
check "go-test restores test results by prefix on pull requests" has "$gotest" 'go-testres-.*-$'
check "go-ci save does not depend on the test run (comes first, no testres condition)" test "$(awk '/actions\/cache\/save@/{s++} /id: testres/{t=NR} /key: .*steps\.go\.outputs\.key/{if(s&&!k){k=NR}} END{print (k&&t&&k<t)?1:0}' <<<"$warm")" = 1
check "warm-caches does not run the cacheable packages with -count=1" hasnt "$warm" 'go-test.sh.*-count=1'
# cache_switches FILE: every non-comment line of FILE that names KOINON_GO_TEST_CACHE is either go-test's
# pull_request expression or the unconditional `KOINON_GO_TEST_CACHE: '1'`, and that one is there exactly
# once, in warm-caches (pushes to main only).
# shellcheck disable=SC2329 # called through check
cache_switches() {
  local allowed="KOINON_GO_TEST_CACHE: (\\$\\{\\{ github.event_name == 'pull_request' && '1' \\|\\| '' \\}\\}|'1') *$"
  [ "$(grep -cE "^ *KOINON_GO_TEST_CACHE: '1' *$" "$1")" = 1 ] || return 1
  grep -Eq "^ *KOINON_GO_TEST_CACHE: '1' *$" <<<"$(job_body "$1" warm-caches)" || return 1
  ! grep -E 'KOINON_GO_TEST_CACHE' "$1" | grep -Ev '^ *#' | grep -Eqv "$allowed"
}
check "KOINON_GO_TEST_CACHE is 1 only on pull requests and in warm-caches" cache_switches "$wf"
# The same check must catch an unconditional switch in go-test, in YAML or in a run line.
neg=$(mktemp "${TMPDIR:-/tmp}/ci-cache-test.XXXXXX") || exit 1
awk '{print} /^  go-test:/{j=1} j&&/KOINON_REQUIRE: docker/{sub(/KOINON_REQUIRE.*/, "KOINON_GO_TEST_CACHE: '"'"'1'"'"'"); print; j=0}' "$wf" >"$neg"
# shellcheck disable=SC2329 # called through check
rejects() { grep -q -- "$2" <<<"$(job_body "$1" go-test)" && ! cache_switches "$1"; } # FILE PATTERN-in-go-test
check "self-test: cache_switches rejects KOINON_GO_TEST_CACHE: '1' in go-test" rejects "$neg" "^ *KOINON_GO_TEST_CACHE: '1'\$"
sed "s|run: hack/go-test.sh$|run: KOINON_GO_TEST_CACHE=1 hack/go-test.sh|" "$wf" >"$neg"
check "self-test: cache_switches rejects KOINON_GO_TEST_CACHE=1 in a run line" rejects "$neg" 'run: KOINON_GO_TEST_CACHE=1 hack'
rm -f "$neg"

# ci-stamp-mtime.sh: two fresh clones of one commit get the same file and directory mtimes; a same-size
# edit changes the file's mtime and its directories', not a sibling directory's.
st=$(mktemp -d "${TMPDIR:-/tmp}/ci-stamp-test.XXXXXX") || exit 1
mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1"; }
# shellcheck disable=SC2329 # called through check
same() { [ "$(mtime "$1")" = "$(mtime "$2")" ]; }
# shellcheck disable=SC2329
differ() { ! same "$@"; }
(
  set -e
  git init -q "$st/src" && cd "$st/src"
  mkdir -p a/testdata b && echo one >a/testdata/x.txt && echo two >b/y.txt
  git add . && git -c user.name=t -c user.email=t@t commit -qm one
  git clone -q "$st/src" "$st/c1" && git clone -q "$st/src" "$st/c2"
  find "$st/c2" -not -path '*/.git/*' -exec touch -t 202201010000 {} + # another checkout time
  echo uno >a/testdata/x.txt && git -c user.name=t -c user.email=t@t commit -qam same-size
  git clone -q "$st/src" "$st/c3"
  for c in c1 c2 c3; do "$repo/hack/ci-stamp-mtime.sh" "$st/$c"; done
) >/dev/null 2>&1
for p in a/testdata/x.txt a/testdata a b b/y.txt .; do
  check "ci-stamp-mtime: two checkouts agree on $p" same "$st/c1/$p" "$st/c2/$p"
done
check "ci-stamp-mtime: the edit keeps the size" test "$(wc -c <"$st/c1/a/testdata/x.txt")" = "$(wc -c <"$st/c3/a/testdata/x.txt")"
for p in a/testdata/x.txt a/testdata a .; do
  check "ci-stamp-mtime: a same-size edit changes the mtime of $p" differ "$st/c1/$p" "$st/c3/$p"
done
check "ci-stamp-mtime: a same-size edit leaves a sibling directory alone" same "$st/c1/b" "$st/c3/b"
rm -rf "$st"
check "go-test stamps file mtimes before the tests" has "$gotest" 'hack/ci-stamp-mtime\.sh'
check "warm-caches stamps file mtimes (same stamps as go-test)" has "$warm" 'hack/ci-stamp-mtime\.sh'
check "warm-caches may delete caches (actions: write)" has "$warm" 'actions: write'
check "warm-caches prunes older go-testres entries" has "$warm" 'gh cache delete'
check "the prune deletes only entries created before the one just saved (overlapping runs keep the newer)" has "$warm" 'select\(\(\.createdAt \| sub\(.*\)\) < env\.MINE\)'
check "the prune cannot fail the job" test "$(awk '/Prune older test-result caches/{f=1} f&&/continue-on-error: true/{print 1; exit}' <<<"$warm")" = 1

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
