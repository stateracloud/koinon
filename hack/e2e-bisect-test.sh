#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-bisect.sh --dry-run on a fixture history.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-bisect-test.XXXXXX")
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
mkdir -p hack && cp "$repo/hack/needs-e2e.sh" "$repo/hack/paths-lib.sh" "$repo/hack/paths.rules" hack/ && cp "$repo/hack/e2e-bisect.sh" hack/
g add -A && g commit -q -m "base"
green=$(git rev-parse HEAD)
commit() { mkdir -p "$(dirname "$1")"; echo "$2" >"$1"; g add -A; g commit -q -m "$3"; }
commit docs/a.md one "docs: tweak"
commit charts/values.yaml one "feat(chart): add a value"
commit web/app.ts one "feat(web): a button"
commit charts/values.yaml two "fix(chart): the culprit"
commit docs/b.md two "docs: more"
chart2=$(git rev-parse HEAD~1)
chart1=$(git rev-parse HEAD~3)

fail=0
check() { if grep -q -- "$2" <<<"$1"; then echo "ok: $3"; else echo "FAIL: $3 (missing '$2')" >&2; fail=1; fi; }
nocheck() { if grep -q -- "$2" <<<"$1"; then echo "FAIL: $3 (found '$2')" >&2; fail=1; else echo "ok: $3"; fi; }
b() { hack/e2e-bisect.sh --dry-run "$@" 2>&1; }

# Two relevant merges; the second fails.
out=$(b --green "$green" --head HEAD --results "${chart2:0:7}=fail")
check "$out" "e2e-relevant candidates: 2" "finds the two chart merges among five"
check "$out" "would dispatch: gh workflow run e2e-kind.yml -f commit=$chart2 -f bisect_id=" "dispatches the second with the bisect id"
nocheck "$out" "feat(web)" "the web-only merge is not a candidate"
check "$out" "Culprit: .* fix(chart): the culprit" "names the culprit"
check "$out" "fix forward within 2 hours" "states the policy"
check "$out" "label e2e-regression" "labels the issue"
first_open=$(grep -n "issue opened" <<<"$out" | head -1 | cut -d: -f1)
first_dispatch=$(grep -n "would dispatch" <<<"$out" | head -1 | cut -d: -f1)
if [[ $first_open -lt $first_dispatch ]]; then echo "ok: the issue is opened before anything is dispatched"; else echo "FAIL: issue opened after dispatch" >&2; fail=1; fi
check "$out" "issue updated" "and updated with the result"

# Transition only: main was already red.
out=$(b --green "$green" --head HEAD --prev failure)
check "$out" "already red" "stops when the previous run also failed"
nocheck "$out" "would dispatch" "and dispatches nothing"

# An open issue: comment, never dispatch again.
out=$(b --green "$green" --head HEAD --open-issue 42)
check "$out" "would comment on #42" "comments on the open issue"
nocheck "$out" "would dispatch" "and does not dispatch again"

# Unknown outcome never names a culprit.
out=$(b --green "$green" --head HEAD --results "${chart1:0:7}=unknown,${chart2:0:7}=unknown")
nocheck "$out" "Culprit:" "unknown names nobody"
check "$out" "Outcome unknown" "and is reported as unknown"

# Both pass: no culprit.
out=$(b --green "$green" --head HEAD)
check "$out" "No candidate failed on its own" "a flake is reported as such"

# One relevant merge: a confirming re-run decides.
one=$(git rev-parse HEAD~1)
only=$(git rev-parse HEAD~3)
out=$(b --green "$(git rev-parse "$only~1")" --head "$only" --results "${only:0:7}=fail")
check "$out" "e2e-relevant candidates: 1" "one candidate"
check "$out" "would dispatch" "a single candidate gets a confirming run"
check "$out" "Culprit:" "named when the confirmation fails"
out=$(b --green "$(git rev-parse "$only~1")" --head "$only")
check "$out" "Suspected:" "only suspected when it does not"
check "$out" "Re-run before reverting" "with the advice"
nocheck "$out" "Culprit:" "and not named"
: "$one"

# No green baseline: only HEAD is re-run.
out=$(b --head HEAD --results "$(git rev-parse --short=7 HEAD)=fail")
check "$out" "no green baseline" "says so"
check "$out" "-f commit=$(git rev-parse HEAD)" "dispatches HEAD only"

# More candidates than the cap: the newest are tested, the rest listed.
out=$(MAX_CANDIDATES=1 b --green "$green" --head HEAD)
check "$out" "untested older: 1" "caps the candidates"
check "$out" "not tested (more than 1)" "and lists the untested"

# Nothing relevant.
out=$(b --green "$(git rev-parse HEAD~1)" --head HEAD)
check "$out" "e2e-relevant candidates: 0" "no candidates"
check "$out" "suspect a flake" "says so"
nocheck "$out" "would dispatch" "and runs nothing"

# A merge commit is titled by its PR title, not "Merge pull request ...".
git checkout -q -b feature "$green"
commit charts/feature.yaml f "feat(chart): from a branch"
git checkout -q main
g merge --no-ff -q feature -m "Merge pull request #7 from org/feature" -m "feat(chart): the real PR title"
out=$(b --green "$(git rev-parse HEAD~1)" --head HEAD)
check "$out" "feat(chart): the real PR title" "uses the PR title for a merge commit"
nocheck "$out" "Merge pull request" "never the merge subject"

# Non-dry-run against a stub gh: the dispatched run is matched exactly (commit and bisect id).
export GITHUB_RUN_ID=999 POLL_SECONDS=1 CANDIDATE_TIMEOUT=2
head=$(git rev-parse HEAD)
fx="$tmp-gh"
mkdir -p "$fx"
trap 'rm -rf "$tmp" "$fx"' EXIT
cat >"$fx/runs.json" <<JSON
[{"databaseId":50,"headSha":"$green","event":"push","conclusion":"success"}]
JSON
cat >"$fx/dispatched.json" <<JSON
[{"databaseId":111,"displayTitle":"e2e-kind ($head) bisect 555"},
 {"databaseId":222,"displayTitle":"e2e-kind ($head) bisect 999"}]
JSON
echo '{"status":"completed","conclusion":"success"}' >"$fx/view-111.json"
echo '{"status":"completed","conclusion":"failure"}' >"$fx/view-222.json"
cat >"$fx/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FX/calls"
jqexpr="" ; args=("$@")
for i in "${!args[@]}"; do [[ ${args[$i]} == --jq ]] && jqexpr=${args[$((i + 1))]}; done
case "$1 $2" in
"run list") if [[ $* == *workflow_dispatch* ]]; then f=dispatched.json; else f=runs.json; fi; jq -r "${jqexpr:-.}" "$FX/$f" ;;
"run view") jq -r "${jqexpr:-.}" "$FX/view-$3.json" ;;
"issue list") ;;
"issue create") echo "https://example.invalid/issues/1" ;;
esac
SH
chmod +x "$fx/gh"
export FX="$fx" GH="$fx/gh"
out=$(hack/e2e-bisect.sh --head HEAD 2>&1)
check "$out" "candidate .*: fail" "the run named for this bisect id decides (222 fail), not the other one (111 pass)"
nocheck "$out" "candidate .*: pass" "the run of another bisect is ignored"
check "$(cat "$fx/calls")" "issue edit https://example.invalid/issues/1" "the issue is updated"
check "$(cat "$fx/calls")" "bisect_id=999" "the dispatch carries the bisect id"
nocheck "$(cat "$fx/calls")" "run watch" "never gh run watch"

[[ $fail -eq 0 ]] && echo "e2e-bisect tests passed"
exit "$fail"
