#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/needs-e2e.sh with fixture path lists, and checks that every hack/ script
# test/e2e/run.sh calls is covered by the rule.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."

# First, and on its own line so set -e stops the script when mktemp fails (old issue 476).
tmp=$(mktemp -d "${TMPDIR:-/tmp}/needs-e2e-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
# expect <0|1> <paths, space separated>
expect() {
  local want=$1 got=0
  shift
  printf '%s\n' "$@" | hack/needs-e2e.sh --stdin >/dev/null || got=$?
  if [[ $got -ne $want ]]; then
    echo "FAIL: want exit $want, got $got for: $*" >&2
    fail=1
  fi
}

# --forced-only: just the e2e's own files.
forced() { local want=$1 got=0; shift; printf '%s\n' "$@" | hack/needs-e2e.sh --stdin --forced-only >/dev/null || got=$?; [[ $got -eq $want ]] || { echo "FAIL: --forced-only want $want got $got for: $*" >&2; fail=1; }; }
forced 0 test/e2e/run.sh
forced 0 test/e2e/kind/up.sh
forced 0 .github/workflows/e2e-kind.yml
forced 0 hack/docs-e1.sh
forced 0 docs/site/src/content/docs/quickstart.md
forced 0 docs/site/src/content/docs/guides/backup-restore.md
forced 0 flavors/opencode/prompt.md
forced 0 hack/versions.env
forced 0 hack/versions-env.sh
forced 0 hack/e2e-toolchain/web.Dockerfile
forced 0 hack/e2e-toolchain/Dockerfile
forced 1 internal/api/server.go
forced 1 charts/koinon/values.yaml
forced 1 go.mod

# Needed.
expect 0 cmd/koinon-api/main.go
expect 0 cmd/koinon-fakemcp/main.go
expect 0 cmd/koinon-cli/main.go                   # not built by the e2e, but cmd/ is a backstop
expect 0 internal/auth/token.go                   # derived: imported by a binary
expect 0 internal/tracing/scrub/always.go          # derived: a nested package the api imports
expect 0 charts/koinon/templates/migrate-job.yaml
expect 0 internal/db/migrations/00042_x.sql
expect 0 internal/agentd/run.go
expect 0 internal/reconciler/reconcile.go
expect 0 internal/services/agents/service.go
expect 0 internal/api/server.go
expect 0 internal/execution/run.go
expect 0 internal/gateway/policy.go
expect 0 internal/ledger/ledger.go
expect 0 internal/githubtools/branch.go
expect 0 internal/tracing/export/export.go
expect 0 flavors/opencode/Dockerfile
expect 0 test/e2e/run.sh
expect 0 test/e2e/quickstart_test.go
expect 0 test/e2e/kind/up.sh
expect 0 test/fakemodel/server.go
expect 0 .github/workflows/e2e-kind.yml
expect 0 go.mod
expect 0 go.sum
expect 0 release/versions.yaml
expect 0 hack/docs-e1.sh
expect 0 docs/site/src/content/docs/quickstart.md
expect 0 docs/site/src/content/docs/guides/backup-restore.md
expect 0 docs/guide.md internal/api/server.go    # one relevant path among others is enough

# Not needed.
expect 1 README.md
expect 1 internal/db/queries/agents.sql        # sqlc input: the generated Go is what a binary builds
expect 1 docs/design.md
expect 1 docs/adr/0075-x.md
expect 1 charts/koinon/README.md                  # Markdown inside a needed tree
expect 1 web/src/App.tsx
expect 1 web/e2e/login.spec.ts
expect 1 internal/agentd/run_test.go              # tests outside test/e2e
expect 1 internal/services/agents/service_test.go
expect 1 test/fakemodel/server_test.go
expect 0 flavors/opencode/prompts/system.md   # flavors' Markdown can be prompts
expect 0 Makefile
expect 0 go.work
expect 1 hack/chart-check.sh
expect 1 hack/needs-e2e.sh
expect 1 .github/workflows/ci.yml
expect 1 internal/zzz-new-package/x.go            # a package no binary imports
expect 1 internal/auth/testdata/fixture.json
expect 1 web/package.json docs/x.md
expect 1 # empty change

# The binaries listed in the rule are the ones run.sh builds.
want=$(grep -oE '\./cmd/koinon-[a-z]+' test/e2e/run.sh | sort -u | tr '\n' ' ')
have=$(sed -n '/^BINARIES=(/,/^)/p' hack/needs-e2e.sh | grep -oE '\./cmd/koinon-[a-z]+' | sort -u | tr '\n' ' ')
[[ $want == "$have" ]] || { echo "FAIL: BINARIES ($have) differ from run.sh's ko build ($want)" >&2; fail=1; }

# Every hack/ path run.sh calls must be matched.
while IFS= read -r p; do
  [[ -f $p ]] || continue
  expect 0 "$p"
done < <(grep -v '^ *#' test/e2e/run.sh | grep -oE 'hack/[A-Za-z0-9_.-]+\.sh' | sort -u)

# old issue 565: every file the jobs that run the e2e (the `quickstart` job of e2e-kind.yml, not decide, result
# or bisect, which are about whether to run) read, and every file under hack/e2e-toolchain/, is an
# input: a kind, ko or helm bump must run the e2e.
while IFS= read -r p; do
  [[ -f $p ]] || continue
  expect 0 "$p"
done < <(awk '/^  quickstart:/ {on=1; next} /^  [a-z][a-z-]*:/ {on=0} on' .github/workflows/e2e-kind.yml |
  grep -v '^ *#' | grep -oE 'hack/[A-Za-z0-9_.-]+\.(sh|env)' | sort -u)
for p in hack/versions.env hack/e2e-toolchain/*; do expect 0 "$p"; done
expect 0 test/escape/kind.sh test/escape/exfil_test.go   # run.sh runs them through test/e2e/escape-openshell.sh and escape-mcp.sh

# Against git, in a scratch module shaped like the repository: the six binaries and the driver.
g() { git -C "$tmp" -c user.name=t -c user.email=t@example.invalid "$@"; }
git -C "$tmp" init -q -b main
printf 'module example.invalid/m\n\ngo 1.21\n' >"$tmp/go.mod"
while read -r b; do
  mkdir -p "$tmp/cmd/$b"
  printf 'package main\n\nfunc main() {}\n' >"$tmp/cmd/$b/main.go"
done < <(sed -n '/^BINARIES=(/,/^)/p' hack/needs-e2e.sh | grep -oE 'koinon-[a-z]+')
mkdir -p "$tmp/test/e2e" "$tmp/charts/k/templates" "$tmp/docs"
printf 'package e2e\n' >"$tmp/test/e2e/e2e.go"
echo a >"$tmp/charts/k/templates/job.yaml"
g add -A && g commit -q -m base
needs() { (cd "$tmp" && "$OLDPWD/hack/needs-e2e.sh" "$1" >/dev/null 2>&1); }

echo x >"$tmp/docs/a.md"
g add -A && g commit -q -m doc
if needs HEAD~1; then echo "FAIL: doc-only diff needs e2e" >&2; fail=1; fi
echo x >"$tmp/charts/m.yaml"
g add -A && g commit -q -m chart
needs HEAD~2 || { echo "FAIL: chart diff does not need e2e" >&2; fail=1; }

# A rename out of a needed tree counts: the old path is a change too.
g mv charts/k/templates/job.yaml docs/job.yaml && g commit -q -m rename
needs HEAD~1 || { echo "FAIL: moving a chart template to docs/ does not need e2e" >&2; fail=1; }

# Fail closed: a broken build graph (a renamed binary directory) exits 2, not 1.
g mv cmd/koinon-api cmd/koinon-apiserver && g commit -q -m rename-binary
rc=0
(cd "$tmp" && "$OLDPWD/hack/needs-e2e.sh" HEAD~1 >/dev/null 2>&1) || rc=$?
[[ $rc -eq 2 ]] || { echo "FAIL: broken go list exited $rc, want 2" >&2; fail=1; }

[[ $fail -eq 0 ]] && echo "needs-e2e tests passed"
exit "$fail"
