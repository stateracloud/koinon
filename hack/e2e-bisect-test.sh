#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-bisect.sh --dry-run on a fixture history.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main .
g() { git -c user.name=t -c user.email=t@example.invalid "$@"; }
printf 'module example.invalid/m\n\ngo 1.21\n' >go.mod
for b in koinon-api koinon-reconciler koinon-agentd koinon-fakemodel koinon-fakegithub koinon-fakemcp; do
  mkdir -p "cmd/$b"
  printf 'package main\n\nfunc main() {}\n' >"cmd/$b/main.go"
done
mkdir -p test/e2e charts docs web
printf 'package e2e\n' >test/e2e/e2e.go
mkdir -p hack && cp "$repo/hack/needs-e2e.sh" hack/ && cp "$repo/hack/e2e-bisect.sh" hack/
g add -A && g commit -q -m "base"
green=$(git rev-parse HEAD)
commit() { echo "$2" >"$1"; g add -A; g commit -q -m "$3"; }
commit docs/a.md one "docs: tweak"
commit charts/values.yaml one "feat(chart): add a value"
commit web/app.ts one "feat(web): a button"
commit charts/values.yaml two "fix(chart): the culprit"
commit docs/b.md two "docs: more"
chart2=$(git rev-parse HEAD~1)

fail=0
check() { if grep -q -- "$2" <<<"$1"; then echo "ok: $3"; else echo "FAIL: $3 (missing '$2')" >&2; fail=1; fi; }
nocheck() { if grep -q -- "$2" <<<"$1"; then echo "FAIL: $3 (found '$2')" >&2; fail=1; else echo "ok: $3"; fi; }

# Two relevant merges; the second fails.
out=$(hack/e2e-bisect.sh --dry-run --green "$green" --head HEAD --results "${chart2:0:7}=fail")
check "$out" "e2e-relevant candidates: 2" "finds the two chart merges among five"
check "$out" "would dispatch: gh workflow run e2e-kind.yml -f commit=$chart2" "dispatches the second"
nocheck "$out" "feat(web)" "the web-only merge is not a candidate"
check "$out" "Culprit: ${chart2:0:7} fix(chart): the culprit" "names the culprit"
check "$out" "fix forward within 2 hours" "states the policy"
check "$out" "label e2e-regression" "labels the issue"

# Both pass: no culprit.
out=$(hack/e2e-bisect.sh --dry-run --green "$green" --head HEAD)
check "$out" "No candidate failed on its own" "a flake is reported as such"

# One relevant merge: it is the culprit without runs.
first=$(git rev-parse HEAD~3)
out=$(hack/e2e-bisect.sh --dry-run --green "$(git rev-parse HEAD~3~1)" --head "$first")
check "$out" "e2e-relevant candidates: 1" "one candidate"
nocheck "$out" "would dispatch" "a single candidate needs no run"
check "$out" "Culprit:" "and is the culprit"

# Nothing relevant.
out=$(hack/e2e-bisect.sh --dry-run --green "$(git rev-parse HEAD~1)" --head HEAD)
check "$out" "e2e-relevant candidates: 0" "no candidates"
check "$out" "suspect a flake" "says so"

[[ $fail -eq 0 ]] && echo "e2e-bisect tests passed"
exit "$fail"
