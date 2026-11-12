#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/preflight.sh: a temp git repository with a bare "origin", and every tool the
# gates call (go, make, golangci-lint, pnpm, shellcheck, gitleaks, the hack/ checkers) as a stub that
# logs its call. Nothing is built or linted for real, and no cluster is touched.
# shellcheck disable=SC2016 # the stubs are written with single quotes on purpose
# shellcheck disable=SC2317 # has/hasnt/eq are called indirectly, through check

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-test.XXXXXX") || exit 1 # explicit template: macOS mktemp -d ignores TMPDIR (old issue 466)
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
"list ./...") echo example.com/m/a; echo example.com/m/test/harnesses ;;
"list -f") echo "$PWD"; echo "$PWD/internal/foo"; echo "$PWD/test/harnesses" ;; # go list -f {{.Dir}} ./...: absolute directories
"test "*) [[ ${FAKE_GO_TEST_RC:-0} -eq 0 ]] || echo "--- FAIL: TestX"; exit "${FAKE_GO_TEST_RC:-0}" ;;
"run mvdan.cc/gofumpt@v0.12.0") [[ -z ${FAKE_GOFUMPT_OUT:-} ]] || echo "$FAKE_GOFUMPT_OUT" ;;
esac
exit 0'
stub golangci-lint 'exit "${FAKE_LINT_RC:-0}"'
stub make '[[ -z ${FAKE_GEN_CHANGE:-} ]] || echo gen >>generated.txt'
# `pnpm --dir web install` makes web/node_modules/.bin/protoc-gen-es unless FAKE_PNPM_INSTALL_FAIL is set.
stub pnpm 'if [[ " $* " == *" install "* ]]; then [[ -z ${FAKE_PNPM_INSTALL_FAIL:-} ]] || exit 1; mkdir -p web/node_modules/.bin; : >web/node_modules/.bin/protoc-gen-es; fi; exit 0'
stub shellcheck 'exit 0'
stub actionlint '[[ ${FAKE_ACTIONLINT_RC:-0} -eq 0 ]] || echo "x.yml:1:1: fake finding"; exit "${FAKE_ACTIONLINT_RC:-0}"'
stub zizmor '[[ $1 != --version ]] || { echo "zizmor ${FAKE_ZIZMOR_VERSION:-$(sed -n "s/^ZIZMOR_VERSION=//p" "'"$repo"'/hack/versions.env")}"; exit 0; }; [[ ${FAKE_ZIZMOR_RC:-0} -eq 0 ]] || echo "x.yml:1:1: fake finding"; exit "${FAKE_ZIZMOR_RC:-0}"'
stub buf 'exit "${FAKE_BUF_RC:-0}"'
stub gitleaks '[[ ${FAKE_GITLEAKS_RC:-0} -eq 0 ]] || echo "Finding: fake"; exit "${FAKE_GITLEAKS_RC:-0}"'
export PATH="$tmp/bin:$PATH"

# The real path rules, but the `scripts` gate runs one stub (the real checks are the repository's own).
{ grep -v '^script-check ' "$repo/hack/paths.rules"; echo 'script-check - hack/script-check-fake.sh @BASE@ HEAD'; } >"$tmp/paths.rules"
export HACK_PATHS_RULES=$tmp/paths.rules

git init -q --bare -b main "$tmp/origin.git"
seed=$tmp/seed
git init -q -b main "$seed"
git -C "$seed" config user.name seed
git -C "$seed" config user.email seed@example.invalid
mkdir -p "$seed/hack" "$seed/internal/foo" "$seed/charts/koinon" "$seed/web" "$seed/docs/adr" "$seed/internal/db/queries" "$seed/internal/db/migrations"
for s in migrate-lint adr-lint adr-lint-test doc-meta-lint doc-meta-lint-test chart-check dep-age-check script-check-fake mktemp-lint mktemp-lint-test runner-lint runner-lint-test fetch-pinned-test pinned-download-lint bash5-lint bash5-lint-test audit-batch-lint audit-batch-lint-test versions-check versions-check-test govulncheck govulncheck-test go-fuzz; do
	printf '#!/usr/bin/env bash\necho "%s $* [KUBECONFIG=${KUBECONFIG:-}]" >>"$FAKE_LOG"\nexit "${FAKE_%s_RC:-0}"\n' "$s.sh" "$(tr a-z- A-Z_ <<<"$s")" >"$seed/hack/$s.sh"
done
# The Go gate runs hack/go-test.sh (and its verdict, old issue 507), not bare go test.
printf '#!/usr/bin/env bash\necho "go-test.sh $* [KUBECONFIG=${KUBECONFIG:-}]" >>"$FAKE_LOG"\ncase ${FAKE_GO_TEST_RC:-0} in 0) ;; 3) echo "go-test verdict: INCOMPLETE (missing docker): 1 passed, 0 partly skipped, 0 failed, 0 not run, 1 skipped (of 2 suites)" ;; *) echo "--- FAIL: TestX" ;; esac\nexit "${FAKE_GO_TEST_RC:-0}"\n' >"$seed/hack/go-test.sh"
printf '#!/usr/bin/env bash\necho "needs-e2e.sh $*" >>"$FAKE_LOG"\nrc=${FAKE_NEEDS_RC:-1}\n[[ $rc -ne 0 ]] || echo "kind e2e needed"\nexit "$rc"\n' >"$seed/hack/needs-e2e.sh"
cp "$repo/hack/golangci-lint.sh" "$seed/hack/golangci-lint.sh" # the real wrapper; it execs the golangci-lint stub
cp "$repo/hack/buf-breaking-against.sh" "$seed/hack/buf-breaking-against.sh" # real helper (#542); needs only git
chmod +x "$seed/hack/"*.sh
printf 'module example.com/m\n\ngo 1.25\n' >"$seed/go.mod"
echo "package foo" >"$seed/internal/foo/a.go"
echo base >"$seed/README.txt"
echo 'lockfileVersion: 1' >"$seed/web/pnpm-lock.yaml"
for d in internal/foo charts/koinon web docs/adr internal/db/queries internal/db/migrations; do : >"$seed/$d/.gitkeep"; done
(cd "$seed" && git add -A && git commit -q -m seed && git remote add origin "$tmp/origin.git" && git push -q origin main)

fresh() { # a fresh clone on a feature branch, echoes its path
	local w
	# Fail hard (old issue 485): a failed mktemp or clone must never leave $w empty, or the git -C calls below
	# would run in the caller's own checkout and switch its branch.
	w=$(mktemp -d "$tmp/w.XXXXXX") || exit 1
	[[ -n $w && -d $w ]] || { echo "mktemp gave no directory" >&2; exit 1; }
	git clone -q "$tmp/origin.git" "$w" 2>/dev/null || { echo "clone into $w failed" >&2; exit 1; }
	[[ -d $w/.git ]] || { echo "$w is not a checkout" >&2; exit 1; }
	git -C "$w" config user.name dev
	git -C "$w" config user.email dev@example.invalid
	git -C "$w" checkout -q -b feat/x
	echo "$w"
}
# old issue 485: when mktemp fails, fresh() must stop and leave the caller's checkout alone.
guard=$(mktemp -d "$tmp/guard.XXXXXX")
git -C "$guard" init -q -b keep
git -C "$guard" -c user.name=d -c user.email=d@example.invalid commit -q --allow-empty -m g
mkdir -p "$tmp/failbin"
printf '#!/bin/sh\nexit 1\n' >"$tmp/failbin/mktemp"
chmod +x "$tmp/failbin/mktemp"
mktemp_fail_rc=0
(cd "$guard" && PATH="$tmp/failbin:$PATH" && w=$(fresh)) >/dev/null 2>&1 || mktemp_fail_rc=$?
reset_state() { : >"$FAKE_LOG"; unset FAKE_TIDY_CHANGE FAKE_GO_TEST_RC FAKE_LINT_RC FAKE_GITLEAKS_RC FAKE_GEN_CHANGE FAKE_NEEDS_RC FAKE_GOFUMPT_OUT FAKE_MIGRATE_LINT_RC FAKE_CHART_CHECK_RC FAKE_DEP_AGE_CHECK_RC FAKE_BUF_RC FAKE_MKTEMP_LINT_RC FAKE_BASH5_LINT_RC FAKE_SCRIPT_CHECK_FAKE_RC FAKE_GOVULNCHECK_RC || true; rm -rf "$tmp/tmpdir"/*; }

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
for g in chart web generate migrate adr shell mktemp pinned bash5; do check "$g selected by its path" has "^$g:" "$tmp/l2"; done
check "no Go files: go not selected" hasnt '^go:' "$tmp/l2"
check "no Go files: tidy not selected" hasnt '^tidy:' "$tmp/l2"
check "no go.mod change: vuln not selected" hasnt '^vuln:' "$tmp/l2"
check "a Go file change alone does not select vuln" hasnt '^vuln:' "$tmp/l1"

# govulncheck (#579 part 2): a go.mod change selects the vuln gate, which runs the pinned-version wrapper
reset_state
w=$(fresh); cd "$w"
echo "// require changed" >>go.mod
commit_all
pf "$tmp/lv" --list >/dev/null
check "go.mod change selects vuln" has '^vuln: go.mod or go.sum changed' "$tmp/lv"
rc=$(pf "$tmp/rv" --only vuln)
check "vuln gate: exit 0" eq "$rc" 0
check "vuln gate ran the test and the wrapper" has '^govulncheck.sh' "$FAKE_LOG"
export FAKE_GOVULNCHECK_RC=1
rc=$(pf "$tmp/rvf" --only vuln)
check "a failing govulncheck fails preflight" eq "$rc" 1
check "vuln FAIL line" has '^vuln FAIL' "$tmp/rvf"
unset FAKE_GOVULNCHECK_RC

# a workflow-only change selects the mktemp and actionlint gates but not the shell gate
reset_state
w=$(fresh); cd "$w"
mkdir -p .github/workflows
echo "on: push" >.github/workflows/x.yml
commit_all
pf "$tmp/l3" --list >/dev/null
check "workflow change selects mktemp" has '^mktemp:' "$tmp/l3"
check "workflow change selects actionlint" has '^actionlint:' "$tmp/l3"
check "workflow change selects zizmor" has '^zizmor:' "$tmp/l3"
check "workflow change selects runnerlint" has '^runnerlint:' "$tmp/l3"
check "go change does not select runnerlint" hasnt '^runnerlint:' "$tmp/l1"
check "workflow change selects versions" has '^versions:' "$tmp/l3"
check "go change does not select versions" hasnt '^versions:' "$tmp/l1"
check "go change selects audit" has '^audit:' "$tmp/l1"

check "workflow change does not select shell" hasnt '^shell:' "$tmp/l3"
check "workflow change does not select bash5" hasnt '^bash5:' "$tmp/l3"

# a change to the runner lint alone selects its gate, not actionlint
reset_state
w=$(fresh); cd "$w"
printf '#!/bin/sh\n' >hack/runner-lint-test.sh
commit_all
pf "$tmp/l3b" --list >/dev/null
check "runner-lint script change selects runnerlint" has '^runnerlint:' "$tmp/l3b"
check "runner-lint script change does not select actionlint" hasnt '^actionlint:' "$tmp/l3b"

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
check "go-test.sh (with its verdict, old issue 507) on the changed package" has '^go-test.sh ./internal/foo ' "$FAKE_LOG"
check "every gate ran with KUBECONFIG=/dev/null" hasnt 'KUBECONFIG=\]' "$FAKE_LOG"
check "gitleaks scans base..HEAD" has 'gitleaks git --redact --no-banner --log-opts=origin/main..HEAD' "$FAKE_LOG"
check "chart gate did not run" hasnt 'chart-check' "$FAKE_LOG"
check "logs are kept" compgen -G "$TMPDIR/koinon-preflight/*/go.log"
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
check "falls back to gofumpt" has '^go run mvdan.cc/gofumpt@v0.12.0 -l internal/foo/a.go' "$FAKE_LOG"
check "falls back to go vet" has '^go vet ./internal/foo ' "$FAKE_LOG"
check "no golangci-lint call" hasnt '^golangci-lint' "$FAKE_LOG"

# 3b. no Go file changed (--only go): golangci-lint and go-test.sh get ./-relative directories, never
# import paths (golangci-lint reads each as a directory: "directory not found"), and not test/harnesses
reset_state
w=$(fresh); cd "$w"
echo "x" >>README.txt; commit_all
: >"$FAKE_LOG"
check "no Go file: go gate exit 0" eq "$(pf "$tmp/r3b" --only go)" 0
check "no Go file: lint gets directories" has '^golangci-lint run [.] [.]/internal/foo ' "$FAKE_LOG"
check "no Go file: no import path" hasnt 'example[.]com/m' "$FAKE_LOG"
check "no Go file: no test/harnesses" hasnt 'test/harnesses' "$FAKE_LOG"
check "no Go file: go-test.sh gets the same directories" has '^go-test.sh [.] [.]/internal/foo ' "$FAKE_LOG"

# 4. failures: exit 1, the gate is named, the log path is shown, at most 5 error lines, other gates still run
reset_state
w=$(fresh); cd "$w"
echo "package foo // f" >>internal/foo/a.go; commit_all
export FAKE_GO_TEST_RC=1
rc=$(pf "$tmp/f1")
check "go test failure: exit 1" eq "$rc" 1
check "FAIL line names the gate and log" has "^go FAIL [0-9]*s log=$TMPDIR/koinon-preflight/[0-9a-f]*/go.log$" "$tmp/f1"
check "error excerpt shown" has '^  | --- FAIL: TestX' "$tmp/f1"
check "excerpt at most 5 lines" test "$(grep -c '^  | ' "$tmp/f1")" -le 5
check "later gates still run" has '^gitleaks ok' "$tmp/f1"
check "summary names the failed gate" has '^preflight FAIL (go)' "$tmp/f1"
unset FAKE_GO_TEST_RC
# 4b. incomplete (go-test.sh exit 3, an expected requirement missing): exit 3, never "ok"
export FAKE_GO_TEST_RC=3
rc=$(pf "$tmp/f1b" --only go)
check "go incomplete: exit 3" eq "$rc" 3
check "INCOMPLETE line names the skipped requirement and log" has "^go INCOMPLETE [0-9]*s [(]skipped: docker[)] log=" "$tmp/f1b"
check "summary says incomplete" has '^preflight incomplete' "$tmp/f1b"
check "no preflight ok" hasnt '^preflight ok' "$tmp/f1b"
check "no go ok" hasnt '^go ok' "$tmp/f1b"
unset FAKE_GO_TEST_RC
# 4c. golangci-lint's own exit 3 (its findings code) is a failure, not INCOMPLETE
export FAKE_LINT_RC=3
rc=$(pf "$tmp/f1c" --only go)
check "lint exit 3: exit 1" eq "$rc" 1
check "lint exit 3 is a FAIL" has '^go FAIL ' "$tmp/f1c"
check "lint exit 3 is not INCOMPLETE" hasnt '[Ii][Nn][Cc][Oo][Mm][Pp][Ll][Ee][Tt][Ee]' "$tmp/f1c"
unset FAKE_LINT_RC
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

# 6b. a fresh worktree has no web/node_modules (#541): one offline install, then the gate runs; if the
# store cannot serve it the gate is a skip (lane-gates: notrun), never a failure
reset_state
w=$(fresh); cd "$w"
echo "-- q" >internal/db/queries/q.sql; echo "x" >web/app.ts; commit_all
check "fresh worktree has no node_modules" hasnt 'x' <(ls web/node_modules 2>/dev/null)
rc=$(pf "$tmp/n1" --only generate)
check "generate in a fresh worktree: exit 0" eq "$rc" 0
check "offline frozen install ran" has '^pnpm --dir web install --offline --frozen-lockfile' "$FAKE_LOG"
check "make generate ran after the install" has '^make generate' "$FAKE_LOG"
rm -rf web/node_modules; reset_state
export FAKE_PNPM_INSTALL_FAIL=1
for g in generate web; do
	rc=$(pf "$tmp/n2$g" --only "$g")
	check "$g without deps: exit 0 (skip, not fail)" eq "$rc" 0
	check "$g without deps: skip line says why" has "^$g skip (web dependencies missing" "$tmp/n2$g"
done
check "make generate not run without deps" hasnt '^make generate' "$FAKE_LOG"
unset FAKE_PNPM_INSTALL_FAIL

# 7. the checker gates run their scripts with the right arguments
reset_state
w=$(fresh); cd "$w"
echo "-- m" >internal/db/migrations/00001_x.sql
echo "# adr" >docs/adr/0002-x.md
echo "a: b" >charts/koinon/values.yaml
echo "x" >web/app.ts
printf '#!/bin/sh\n' >hack/new.sh
echo "# touched" >>hack/audit-batch-lint.sh
mkdir -p .github/workflows
echo "on: push" >.github/workflows/y.yml
commit_all
rc=$(pf "$tmp/c1")
check "multi-area change passes: exit 0" eq "$rc" 0
check "migrate-lint gets the base" has '^migrate-lint.sh origin/main' "$FAKE_LOG"
check "adr-lint ran" has '^adr-lint.sh' "$FAKE_LOG"
check "adr-lint-test ran" has '^adr-lint-test.sh' "$FAKE_LOG"
check "doc-meta-lint ran" has '^doc-meta-lint.sh' "$FAKE_LOG"
check "doc-meta-lint-test ran" has '^doc-meta-lint-test.sh' "$FAKE_LOG"
check "chart-check ran" has '^chart-check.sh' "$FAKE_LOG"
check "pnpm ran for web" has '^pnpm ' "$FAKE_LOG"
check "shellcheck on the changed script" has '^shellcheck .*hack/new.sh' "$FAKE_LOG"
check "actionlint ran for the workflow change" has '^actionlint' "$FAKE_LOG"
check "zizmor ran for the workflow change" has '^zizmor --offline' "$FAKE_LOG"
check "runner-lint (with the trigger rule) ran" has '^runner-lint.sh' "$FAKE_LOG"
check "mktemp-lint ran" has '^mktemp-lint.sh' "$FAKE_LOG"
check "mktemp-lint-test ran" has '^mktemp-lint-test.sh' "$FAKE_LOG"
check "pinned-download-lint ran" has '^pinned-download-lint.sh' "$FAKE_LOG"
check "fetch-pinned-test ran" has '^fetch-pinned-test.sh' "$FAKE_LOG"
check "bash5-lint ran" has '^bash5-lint.sh' "$FAKE_LOG"
check "bash5-lint-test ran" has '^bash5-lint-test.sh' "$FAKE_LOG"
export FAKE_BASH5_LINT_RC=1
rc=$(pf "$tmp/c5")
check "a failing bash5 lint fails preflight" eq "$rc" 1
check "bash5 FAIL line" has '^bash5 FAIL' "$tmp/c5"
unset FAKE_BASH5_LINT_RC
check "audit-batch-lint ran" has '^audit-batch-lint.sh' "$FAKE_LOG"
check "audit-batch-lint-test ran" has '^audit-batch-lint-test.sh' "$FAKE_LOG"
check "versions-check ran" has '^versions-check.sh' "$FAKE_LOG"
check "versions-check-test ran" has '^versions-check-test.sh' "$FAKE_LOG"
check "no Go change: no go test" hasnt '^go-test.sh' "$FAKE_LOG"
export FAKE_MKTEMP_LINT_RC=1
rc=$(pf "$tmp/c3")
check "a failing mktemp lint fails preflight" eq "$rc" 1
check "mktemp FAIL line" has '^mktemp FAIL' "$tmp/c3"
unset FAKE_MKTEMP_LINT_RC
export FAKE_ACTIONLINT_RC=1
rc=$(pf "$tmp/c4")
check "a failing actionlint fails preflight" eq "$rc" 1
check "actionlint FAIL line" has '^actionlint FAIL' "$tmp/c4"
unset FAKE_ACTIONLINT_RC
export FAKE_ZIZMOR_RC=1
rc=$(pf "$tmp/c4z")
check "a failing zizmor fails preflight" eq "$rc" 1
check "zizmor FAIL line" has '^zizmor FAIL' "$tmp/c4z"
check "no version warning when zizmor is the pinned version" hasnt '^zizmor warn' "$tmp/c4z"
export FAKE_ZIZMOR_VERSION=0.0.1
pf "$tmp/c4w" >/dev/null
check "a zizmor other than the pinned one warns" has '^zizmor warn: CI pins ' "$tmp/c4w"
unset FAKE_ZIZMOR_RC FAKE_ZIZMOR_VERSION
export FAKE_CHART_CHECK_RC=1
rc=$(pf "$tmp/c2")
check "a failing checker fails preflight" eq "$rc" 1
check "chart FAIL line" has '^chart FAIL' "$tmp/c2"
unset FAKE_CHART_CHECK_RC

# 8. deps and proto gates
reset_state
w=$(fresh); cd "$w"
echo "x" >>README.txt; commit_all
pf "$tmp/d0" --list >/dev/null
check "no manifest change: deps not selected" hasnt '^deps:' "$tmp/d0"
check "no proto change: proto not selected" hasnt '^proto:' "$tmp/d0"
reset_state
w=$(fresh); cd "$w"
echo "require example.com/x v1.0.0" >>go.mod
echo '{}' >web/package.json
echo "FROM scratch" >Dockerfile
echo "syntax = 'proto3';" >api.proto
commit_all
pf "$tmp/d1" --list >/dev/null
check "go.mod selects deps" has '^deps:' "$tmp/d1"
check "proto change selects proto" has '^proto:' "$tmp/d1"
rc=$(pf "$tmp/d2" --only deps,proto)
check "deps and proto pass: exit 0" eq "$rc" 0
check "dep-age-check gets the base" has '^dep-age-check.sh origin/main' "$FAKE_LOG"
check "buf lint ran" has '^buf lint' "$FAKE_LOG"
check "buf breaking against the merge base" has '^buf breaking --against .git#ref=' "$FAKE_LOG"
export FAKE_DEP_AGE_CHECK_RC=1
rc=$(pf "$tmp/d3" --only deps)
check "dep-age failure fails preflight" eq "$rc" 1
check "deps FAIL line" has '^deps FAIL' "$tmp/d3"
unset FAKE_DEP_AGE_CHECK_RC
export FAKE_BUF_RC=1
rc=$(pf "$tmp/d4" --only proto)
check "buf failure fails preflight" eq "$rc" 1
unset FAKE_BUF_RC
rc=0
PATH="$tmp/limited" "$repo/hack/preflight.sh" --only proto >"$tmp/d5" 2>&1 || rc=$?
check "no buf: proto skipped, exit 0" eq "$rc" 0
check "no buf: skip line" has '^proto skip' "$tmp/d5"

check "mktemp failure: fresh() fails" test "$mktemp_fail_rc" -ne 0
check "mktemp failure: caller's checkout branch unchanged" eq "$(git -C "$guard" branch --show-current)" keep
# 15. The scripts gate: seconds-long CI script checks run when tooling changes (hack/paths.rules).
reset_state
w=$(fresh); cd "$w"
printf '#!/bin/sh\n' >hack/newtool.sh; commit_all
pf "$tmp/s0" --list >/dev/null
check "tooling change selects scripts" has '^scripts:' "$tmp/s0"
rc=$(pf "$tmp/s1")
check "scripts gate passes: exit 0" eq "$rc" 0
check "scripts ok line" has '^scripts ok' "$tmp/s1"
check "scripts check got the merge base and HEAD" has '^script-check-fake.sh [0-9a-f]\{40\} HEAD' "$FAKE_LOG"
export FAKE_SCRIPT_CHECK_FAKE_RC=1
rc=$(pf "$tmp/s2")
check "a failing script check fails preflight: exit 1" eq "$rc" 1
check "scripts FAIL line" has '^scripts FAIL' "$tmp/s2"
unset FAKE_SCRIPT_CHECK_FAKE_RC
reset_state
w=$(fresh); cd "$w"
mkdir -p docs/dev; echo "# notes" >docs/dev/notes.md; commit_all
pf "$tmp/s3" --list >/dev/null
check "a Markdown-only change does not select scripts (the job rule, #572)" hasnt '^scripts:' "$tmp/s3"
echo "package foo // y" >>internal/foo/a.go; commit_all
pf "$tmp/s3b" --list >/dev/null
check "a Go change selects scripts (the checks read Go sources, #572)" has '^scripts:' "$tmp/s3b"

echo "$pass passed, $fail failed"
exit $((fail > 0))
