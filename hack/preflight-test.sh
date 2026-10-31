#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/preflight.sh: a temp git repository with a bare "origin", and every tool the
# gates call (go, make, golangci-lint, pnpm, shellcheck, gitleaks, the hack/ checkers) as a stub that
# logs its call. Nothing is built or linted for real, and no cluster is touched.
# shellcheck disable=SC2016 # the stubs are written with single quotes on purpose
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap '[[ -n ${KEEP_TMP:-} ]] || rm -rf "$tmp"' EXIT
export FAKE_LOG="$tmp/calls.log" TMPDIR="$tmp/tmpdir" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
mkdir -p "$tmp/bin" "$tmp/tmpdir" "$tmp/limited"

# A PATH with the stubs and only the plain tools the script needs (no real golangci-lint).
for t in git bash env sed grep awk sort cat cmp cp rm mktemp tr tail head wc cksum dirname basename mkdir ls sh find diff true false sleep; do
	p=$(command -v "$t") && ln -sf "$p" "$tmp/limited/$t"
done

stub() { # <name> <body>: a stub that logs its call (with KUBECONFIG) and then runs the body
	printf '#!/usr/bin/env bash\necho "%s $* [KUBECONFIG=${KUBECONFIG:-}]" >>"$FAKE_LOG"\n%s\n' "$1" "$2" >"$tmp/bin/$1"
	chmod +x "$tmp/bin/$1"
}
stub go 'case "$1 $2" in
"mod tidy") [[ -z ${FAKE_TIDY_CHANGE:-} ]] || echo "// changed" >>go.mod ;;
"list ./...") echo example.com/m/a; echo example.com/m/test/flavors ;;
"test "*) [[ ${FAKE_GO_TEST_RC:-0} -eq 0 ]] || echo "--- FAIL: TestX"; exit "${FAKE_GO_TEST_RC:-0}" ;;
"run mvdan.cc/gofumpt@latest") [[ -z ${FAKE_GOFUMPT_OUT:-} ]] || echo "$FAKE_GOFUMPT_OUT" ;;
esac
exit 0'
stub golangci-lint 'exit "${FAKE_LINT_RC:-0}"'
stub make '[[ -z ${FAKE_GEN_CHANGE:-} ]] || echo gen >>generated.txt'
stub pnpm 'exit 0'
stub shellcheck 'exit 0'
stub gitleaks '[[ ${FAKE_GITLEAKS_RC:-0} -eq 0 ]] || echo "Finding: fake"; exit "${FAKE_GITLEAKS_RC:-0}"'
export PATH="$tmp/bin:$PATH"

git init -q --bare -b main "$tmp/origin.git"
seed=$tmp/seed
git init -q -b main "$seed"
git -C "$seed" config user.name seed
git -C "$seed" config user.email seed@example.invalid
mkdir -p "$seed/hack" "$seed/internal/foo" "$seed/charts/koinon" "$seed/web" "$seed/docs/adr" "$seed/internal/db/queries" "$seed/internal/db/migrations"
for s in migrate-lint adr-lint docs-check chart-check; do
	printf '#!/usr/bin/env bash\necho "%s $* [KUBECONFIG=${KUBECONFIG:-}]" >>"$FAKE_LOG"\nexit "${FAKE_%s_RC:-0}"\n' "$s.sh" "$(tr a-z- A-Z_ <<<"$s")" >"$seed/hack/$s.sh"
done
printf '#!/usr/bin/env bash\necho "needs-e2e.sh $*" >>"$FAKE_LOG"\nrc=${FAKE_NEEDS_RC:-1}\n[[ $rc -ne 0 ]] || echo "kind e2e needed"\nexit "$rc"\n' >"$seed/hack/needs-e2e.sh"
chmod +x "$seed/hack/"*.sh
printf 'module example.com/m\n\ngo 1.25\n' >"$seed/go.mod"
echo "package foo" >"$seed/internal/foo/a.go"
echo base >"$seed/README.txt"
for d in internal/foo charts/koinon web docs/adr internal/db/queries internal/db/migrations; do : >"$seed/$d/.gitkeep"; done
(cd "$seed" && git add -A && git commit -q -m seed && git remote add origin "$tmp/origin.git" && git push -q origin main)

fresh() { # a fresh clone on a feature branch, echoes its path
	local w
	w=$(mktemp -d "$tmp/w.XXXXXX")
	git clone -q "$tmp/origin.git" "$w" 2>/dev/null
	git -C "$w" config user.name dev
	git -C "$w" config user.email dev@example.invalid
	git -C "$w" checkout -q -b feat/x
	echo "$w"
}
reset_state() { : >"$FAKE_LOG"; unset FAKE_TIDY_CHANGE FAKE_GO_TEST_RC FAKE_LINT_RC FAKE_GITLEAKS_RC FAKE_GEN_CHANGE FAKE_NEEDS_RC FAKE_GOFUMPT_OUT FAKE_MIGRATE_LINT_RC FAKE_CHART_CHECK_RC || true; rm -rf "$tmp/tmpdir"/*; }

fail=0 pass=0
check() { # <description> <command...>
	local d=$1
	shift
	if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi
}
# shellcheck disable=SC2329 # called through check
has() { grep -q -- "$1" "$2"; }
# shellcheck disable=SC2329
hasnt() { ! grep -q -- "$1" "$2"; }
# shellcheck disable=SC2329
eq() { [[ $1 == "$2" ]]; }
pf() { # <outfile> <args...>: runs preflight in the cwd; echoes the exit code
	local out=$1 rc=0
	shift
	"$repo/hack/preflight.sh" "$@" >"$out" 2>&1 || rc=$?
	echo "$rc"
}
commit_all() { git add -A && git commit -q -m change; }

# 1. --list: gate selection per changed path
reset_state
w=$(fresh); cd "$w"
echo "package foo // x" >>internal/foo/a.go; commit_all
pf "$tmp/l1" --list >/dev/null
check "go change selects go" has '^go: 1 Go files changed' "$tmp/l1"
check "go change selects tidy" has '^tidy:' "$tmp/l1"
check "gitleaks always listed" has '^gitleaks:' "$tmp/l1"
check "e2e always listed" has '^e2e:' "$tmp/l1"
check "go change does not select chart" hasnt '^chart:' "$tmp/l1"
check "go change does not select web" hasnt '^web:' "$tmp/l1"
check "go change does not select migrate" hasnt '^migrate:' "$tmp/l1"
check "--list ran no gate" eq "$(wc -c <"$FAKE_LOG" | tr -d ' ')" 0

reset_state
w=$(fresh); cd "$w"
echo "a: b" >charts/koinon/values.yaml
echo "x" >web/app.ts
echo "-- q" >internal/db/queries/q.sql
echo "-- m" >internal/db/migrations/00001_x.sql
echo "# adr" >docs/adr/0002-x.md
echo "syntax = 'proto3';" >api.proto
printf '#!/bin/sh\n' >hack/new.sh
commit_all
pf "$tmp/l2" --list >/dev/null
for g in chart web generate migrate adr docs shell; do check "$g selected by its path" has "^$g:" "$tmp/l2"; done
check "no Go files: go not selected" hasnt '^go:' "$tmp/l2"
check "no Go files: tidy not selected" hasnt '^tidy:' "$tmp/l2"

# uncommitted and untracked changes count
reset_state
w=$(fresh); cd "$w"
echo "package foo // dirty" >>internal/foo/a.go
echo "x" >web/untracked.ts
pf "$tmp/l3" --list >/dev/null
check "uncommitted Go change selected" has '^go:' "$tmp/l3"
check "untracked web file selected" has '^web:' "$tmp/l3"

# --only forces a gate, even without a trigger
pf "$tmp/l4" --only chart --list >/dev/null
check "--only chart --list shows chart only" eq "$(cat "$tmp/l4")" "chart: forced by --only"
rc=$(pf "$tmp/l5" --only nonsense)
check "unknown gate: exit 2" eq "$rc" 2
rc=$(pf "$tmp/l5b" --base nope/nope)
check "unknown base: exit 2" eq "$rc" 2

# 2. a passing run for a Go change
reset_state
w=$(fresh); cd "$w"
echo "package foo // y" >>internal/foo/a.go; commit_all
rc=$(pf "$tmp/r1")
check "all gates pass: exit 0" eq "$rc" 0
check "go gate ok line" has '^go ok [0-9]*s$' "$tmp/r1"
check "tidy ok line" has '^tidy ok [0-9]*s$' "$tmp/r1"
check "gitleaks ok line" has '^gitleaks ok [0-9]*s$' "$tmp/r1"
check "e2e verdict printed, not failing" has '^e2e info not needed' "$tmp/r1"
check "summary line" has '^preflight ok (' "$tmp/r1"
check "golangci-lint on the changed package" has '^golangci-lint run ./internal/foo ' "$FAKE_LOG"
check "go test on the changed package" has '^go test ./internal/foo ' "$FAKE_LOG"
check "every gate ran with KUBECONFIG=/dev/null" hasnt 'KUBECONFIG=\]' "$FAKE_LOG"
check "gitleaks scans base..HEAD" has 'gitleaks git --redact --no-banner --log-opts=origin/main..HEAD' "$FAKE_LOG"
check "chart gate did not run" hasnt 'chart-check' "$FAKE_LOG"
check "logs are kept" test -f "$TMPDIR/koinon-preflight/go.log"
# e2e needed is informational
: >"$FAKE_LOG"
FAKE_NEEDS_RC=0 pf "$tmp/r1b" --only e2e >/dev/null
check "e2e needed is printed" has '^e2e info needed' "$tmp/r1b"
check "e2e needed does not fail" eq "$(FAKE_NEEDS_RC=0 pf "$tmp/r1c" --only e2e)" 0

# 3. fallback when golangci-lint is not installed: gofumpt + vet
reset_state
w=$(fresh); cd "$w"
echo "package foo // z" >>internal/foo/a.go; commit_all
rc=0
mv "$tmp/bin/golangci-lint" "$tmp/golangci-lint.off"
PATH="$tmp/bin:$tmp/limited" "$repo/hack/preflight.sh" --only go >"$tmp/r2" 2>&1 || rc=$?
mv "$tmp/golangci-lint.off" "$tmp/bin/golangci-lint"
check "no golangci-lint: exit 0" eq "$rc" 0
check "falls back to gofumpt" has '^go run mvdan.cc/gofumpt@latest -l internal/foo/a.go' "$FAKE_LOG"
check "falls back to go vet" has '^go vet ./internal/foo ' "$FAKE_LOG"
check "no golangci-lint call" hasnt '^golangci-lint' "$FAKE_LOG"

# 4. failures: exit 1, the gate is named, the log path is shown, at most 5 error lines, other gates still run
reset_state
w=$(fresh); cd "$w"
echo "package foo // f" >>internal/foo/a.go; commit_all
export FAKE_GO_TEST_RC=1
rc=$(pf "$tmp/f1")
check "go test failure: exit 1" eq "$rc" 1
check "FAIL line names the gate and log" has "^go FAIL [0-9]*s log=$TMPDIR/koinon-preflight/go.log$" "$tmp/f1"
check "error excerpt shown" has '^  | --- FAIL: TestX' "$tmp/f1"
check "excerpt at most 5 lines" test "$(grep -c '^  | ' "$tmp/f1")" -le 5
check "later gates still run" has '^gitleaks ok' "$tmp/f1"
check "summary names the failed gate" has '^preflight FAIL (go)' "$tmp/f1"
unset FAKE_GO_TEST_RC
export FAKE_GITLEAKS_RC=1
rc=$(pf "$tmp/f2" --only gitleaks)
check "gitleaks failure: exit 1" eq "$rc" 1
unset FAKE_GITLEAKS_RC
export FAKE_GOFUMPT_OUT=internal/foo/a.go
rc=0
mv "$tmp/bin/golangci-lint" "$tmp/golangci-lint.off"
PATH="$tmp/bin:$tmp/limited" "$repo/hack/preflight.sh" --only go >"$tmp/f3" 2>&1 || rc=$?
mv "$tmp/golangci-lint.off" "$tmp/bin/golangci-lint"
check "gofumpt diff fails the go gate" eq "$rc" 1
unset FAKE_GOFUMPT_OUT

# 5. tidy restores go.mod
reset_state
w=$(fresh); cd "$w"
echo "package foo // t" >>internal/foo/a.go; commit_all
export FAKE_TIDY_CHANGE=1
rc=$(pf "$tmp/t1" --only tidy)
check "tidy changes go.mod: exit 1" eq "$rc" 1
check "go.mod restored afterwards" test -z "$(git status --porcelain go.mod)"
unset FAKE_TIDY_CHANGE
rc=$(pf "$tmp/t2" --only tidy)
check "tidy clean: exit 0" eq "$rc" 0

# 6. generate: no diff passes, a diff fails
reset_state
w=$(fresh); cd "$w"
echo "-- q" >internal/db/queries/q.sql; commit_all
rc=$(pf "$tmp/g1" --only generate)
check "generate clean: exit 0" eq "$rc" 0
check "make generate called" has '^make generate' "$FAKE_LOG"
export FAKE_GEN_CHANGE=1
rc=$(pf "$tmp/g2" --only generate)
check "generate leaves a diff: exit 1" eq "$rc" 1
rm -f generated.txt
unset FAKE_GEN_CHANGE

# 7. the checker gates run their scripts with the right arguments
reset_state
w=$(fresh); cd "$w"
echo "-- m" >internal/db/migrations/00001_x.sql
echo "# adr" >docs/adr/0002-x.md
echo "a: b" >charts/koinon/values.yaml
echo "x" >web/app.ts
printf '#!/bin/sh\n' >hack/new.sh
commit_all
rc=$(pf "$tmp/c1")
check "multi-area change passes: exit 0" eq "$rc" 0
check "migrate-lint gets the base" has '^migrate-lint.sh origin/main' "$FAKE_LOG"
check "adr-lint ran" has '^adr-lint.sh' "$FAKE_LOG"
check "docs-check ran" has '^docs-check.sh' "$FAKE_LOG"
check "chart-check ran" has '^chart-check.sh' "$FAKE_LOG"
check "pnpm ran for web" has '^pnpm ' "$FAKE_LOG"
check "shellcheck on the changed script" has '^shellcheck hack/new.sh' "$FAKE_LOG"
check "no Go change: no go test" hasnt '^go test' "$FAKE_LOG"
export FAKE_CHART_CHECK_RC=1
rc=$(pf "$tmp/c2")
check "a failing checker fails preflight" eq "$rc" 1
check "chart FAIL line" has '^chart FAIL' "$tmp/c2"
unset FAKE_CHART_CHECK_RC

echo "$pass passed, $fail failed"
exit $((fail > 0))
