#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/needs-e2e.sh with fixture path lists, and checks that every hack/ script
# test/e2e/run.sh calls is covered by the rule.
set -euo pipefail
cd "$(dirname "$0")/.."

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

# Needed.
expect 0 cmd/koinon-api/main.go
expect 0 cmd/koinon-fakemcp/main.go
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
expect 1 hack/chart-check.sh
expect 1 hack/needs-e2e.sh
expect 1 .github/workflows/ci.yml
expect 1 internal/zzz-new-package/x.go            # a package no binary imports
expect 1 internal/auth/testdata/fixture.json
expect 1 cmd/koinon-cli/main.go                   # a command the e2e does not build (if it exists)
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

# Against git: a branch touching only a doc is "not needed", one touching cmd/ is "needed".
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q -b main
mkdir -p "$tmp/charts" "$tmp/docs"
git -C "$tmp" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m base
echo x >"$tmp/docs/a.md"
git -C "$tmp" add . && git -C "$tmp" -c user.name=t -c user.email=t@example.invalid commit -q -m doc
if (cd "$tmp" && "$OLDPWD/hack/needs-e2e.sh" HEAD~1 >/dev/null); then echo "FAIL: doc-only diff needs e2e" >&2; fail=1; fi
echo x >"$tmp/charts/m.yaml"
git -C "$tmp" add . && git -C "$tmp" -c user.name=t -c user.email=t@example.invalid commit -q -m code
(cd "$tmp" && "$OLDPWD/hack/needs-e2e.sh" HEAD~2 >/dev/null) || { echo "FAIL: chart diff does not need e2e" >&2; fail=1; }

[[ $fail -eq 0 ]] && echo "needs-e2e tests passed"
exit "$fail"
