#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/release-gate.sh with a stubbed gh and git (fixture JSON files, filtered by the real
# jq, as gh --jq would): the required list is read from the ruleset, a missing or red required
# check fails, the pull-request-only contexts are read from the merged pull request's head, a
# push-triggered context is not excused by a PR run, and an unreadable ruleset fails closed. The
# quickstart rule (#444) is tested the same way: skipped runs are ignored, and a green merge queue
# run on the same tree stands in for a missing run.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
gate=$here/release-gate.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/release-gate-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# git: fetch and merge-base succeed unless GIT_NOT_ANCESTOR is set.
cat >"$tmp/bin/git" <<'EOF'
#!/usr/bin/env bash
[[ $1 == merge-base && -n ${GIT_NOT_ANCESTOR:-} ]] && exit 1
[[ $1 == diff ]] && { printf '%s\n' "${GIT_DIFF:-}"; exit 0; }
exit 0
EOF

# gh api [--paginate] <path>[?query] [--jq <expr>]: serve $FX/<fixture> through jq.
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
shift # api
path= expr=.
while (($#)); do
	case $1 in
	--paginate) ;;
	--jq) expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
path=${path%%\?*}
case $path in
repos/o/r) f=repo.json ;;
repos/o/r/rules/branches/main) f=rules.json ;;
repos/o/r/commits/*/check-runs) c=${path#repos/o/r/commits/}; f=checks-${c%%/*}.json ;;
repos/o/r/commits/*/pulls) f=pulls.json ;;
repos/o/r/git/commits/*) f=commit-${path##*/}.json ;;
repos/o/r/actions/workflows/e2e-kind.yml/runs) f=mg-runs.json ;;
repos/o/r/actions/runs/*/jobs) c=${path#repos/o/r/actions/runs/}; f=jobs-${c%%/*}.json ;;
repos/o/r/commits/*/statuses) c=${path#repos/o/r/commits/}; f=statuses-${c%%/*}.json ;;
*) echo "gh stub: unexpected $path" >&2; exit 1 ;;
esac
[[ -f $FX/$f ]] || { echo "gh stub: HTTP 404 $path" >&2; exit 1; }
exec jq -r "$expr" "$FX/$f"
EOF
chmod +x "$tmp/bin/git" "$tmp/bin/gh"

pass=0
failed=0
check() { # <name> <got> <want>
	if [[ $2 == "$3" ]]; then
		pass=$((pass + 1))
	else
		echo "FAIL $1: got '$2', want '$3'" >&2
		failed=$((failed + 1))
	fi
}

sha=aaaa111
pr_head=bbbb222

# new_case: a fresh fixture dir with the rules requiring `go`, `dco` and `e2e result`, the
# checks green on the tagged commit (go, quickstart) and on the PR head (dco, e2e result).
new_case() {
	FX=$tmp/fx.$((++n))
	mkdir -p "$FX"
	export FX
	echo '{"default_branch":"main"}' >"$FX/repo.json"
	set_ruleset go dco "e2e result"
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	echo '{"check_runs":[]}' >"$FX/checks-$pr_head.json"
	echo "[{\"merged_at\":\"2026-01-02T00:00:00Z\",\"head\":{\"sha\":\"$pr_head\"}}]" >"$FX/pulls.json"
	run_on "$sha" go success
	run_on "$sha" quickstart success
	run_on "$pr_head" dco success
	run_on "$pr_head" "e2e result" success
}
set_ruleset() { # <context>...: the rules/branches answer
	local ctx
	ctx=$(printf '%s\n' "$@" | jq -R '{context: .}' | jq -s .)
	jq -n --argjson ctx "$ctx" \
		'[{type: "deletion"}, {type: "required_status_checks", ruleset_id: 7, parameters: {required_status_checks: $ctx}}]' >"$FX/rules.json"
}
run_on() { # <sha> <name> <conclusion> [started_at]
	local f=$FX/checks-$1.json
	[[ -f $f ]] || echo '{"check_runs":[]}' >"$f"
	jq --arg n "$2" --arg c "$3" --arg t "${4:-2026-01-01T00:00:00Z}" '.check_runs += [{name: $n, started_at: $t, conclusion: $c}]' "$f" >"$f.new"
	mv "$f.new" "$f"
}
n=0
gate_rc() { # TAG env as given: exit code; stderr to $tmp/err, stdout to $tmp/out
	local rc=0
	PATH=$tmp/bin:$PATH TAG=${TAG:-v1.2.3} SHA=$sha GITHUB_REPOSITORY=o/r GH_TOKEN=koinon_test_not-a-real-token \
		bash "$gate" >"$tmp/out" 2>"$tmp/err" || rc=$?
	echo "$rc"
}

new_case
check "everything green passes" "$(gate_rc)" 0
check "pass prints the PR-only contexts" "$(grep -c '^ok: ' "$tmp/out")" 4

new_case
set_ruleset go dco "e2e result" newcheck
check "a required context with no run on the commit fails" "$(gate_rc)" 1
check "...and names it" "$(grep -c 'FAIL: newcheck' "$tmp/err")" 1

new_case
run_on "$pr_head" newcheck success
set_ruleset go dco "e2e result" newcheck
check "a push-type context is not excused by a run on the PR head" "$(gate_rc)" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go failure
check "a red required check fails" "$(gate_rc)" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$pr_head.json"
run_on "$pr_head" "e2e result" success
check "a PR-only context (dco) missing on the PR head fails" "$(gate_rc)" 1
check "...and says so" "$(grep -c "FAIL: dco: no check run found on the merged pull request's head" "$tmp/err")" 1

new_case
run_on "$pr_head" claim success "2026-01-01T00:00:00Z"
run_on "$pr_head" claim skipped "2026-01-02T00:00:05Z"
set_ruleset go dco claim "e2e result"
check "a claim run skipped by the closed event (after the merge) is ignored" "$(gate_rc)" 0

new_case
run_on "$pr_head" claim success "2026-01-01T00:00:00Z"
run_on "$pr_head" claim failure "2026-01-01T12:00:00Z"
set_ruleset go dco claim "e2e result"
check "a claim run that failed before the merge fails" "$(gate_rc)" 1

new_case
run_on "$sha" "quick checks" success
set_ruleset go dco "quick checks" "e2e result"
check "quick checks must also be on the PR head: missing there fails" "$(gate_rc)" 1

new_case
run_on "$sha" "quick checks" success
run_on "$pr_head" "quick checks" success
set_ruleset go dco "quick checks" "e2e result"
check "quick checks green on the commit and on the PR head passes" "$(gate_rc)" 0

new_case
run_on "$sha" "quick checks" success
run_on "$pr_head" "quick checks" failure
set_ruleset go dco "quick checks" "e2e result"
check "quick checks red on the PR head (dependency age) fails although main is green" "$(gate_rc)" 1

new_case
echo '[]' >"$FX/pulls.json"
check "no merged PR (direct push): PR-only contexts fail" "$(gate_rc)" 1

new_case
run_on "$sha" dco success
echo '{"check_runs":[]}' >"$FX/checks-$pr_head.json"
check "a PR-only context is read from the PR head, not the commit" "$(gate_rc)" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" go success
check "quickstart missing on the commit fails" "$(gate_rc)" 1

new_case
# old issue 595: main skipped the heavy job because the merge tree is the PR head; the head is the evidence.
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
run_on "$pr_head" go success
check "a check skipped on main is read from the PR head (green)" "$(gate_rc)" 0

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
run_on "$pr_head" go failure
check "a check skipped on main and red on the PR head fails" "$(gate_rc)" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
check "a check skipped on main and absent on the PR head fails" "$(gate_rc)" 1

new_case
echo '[]' >"$FX/pulls.json"
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
check "a check skipped on main with no merged PR fails" "$(gate_rc)" 1

# go-test (old issue 610): skipped on main (the merge tree is the PR head), the head decides.
go_test_case() { # a fresh case in which main skipped go-test, the head's go-test is $1 ("" = no run)
	new_case
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	run_on "$sha" quickstart success
	run_on "$sha" go skipped
	run_on "$sha" go-test skipped
	run_on "$pr_head" go success
	[[ -z $1 ]] || run_on "$pr_head" go-test "$1"
}
echo '[{"state":"success","context":"go-test-lab"}]' >"$tmp/lab-ok.json"
echo '[{"state":"failure","context":"go-test-lab"},{"state":"success","context":"go-test-lab"}]' >"$tmp/lab-red.json"
export GIT_DIFF=internal/x/x.go
go_test_case success
check "go-test green on the PR head passes" "$(gate_rc)" 0
go_test_case skipped
cp "$tmp/lab-ok.json" "$FX/statuses-$pr_head.json"
check "go-test skipped by an external run (green go-test-lab on the PR head) passes" "$(gate_rc)" 0
check "...and says it passed in an external run" "$(grep -c 'ok: go-test (passed in an external run' "$tmp/out")" 1
go_test_case skipped
check "go-test skipped with no go-test-lab status and Go in the change fails" "$(gate_rc)" 1
check "...and names go-test" "$(grep -c 'FAIL: go-test: skipped on the merged pull request' "$tmp/err")" 1
go_test_case skipped
cp "$tmp/lab-red.json" "$FX/statuses-$pr_head.json"
check "go-test skipped with a red newest go-test-lab status fails" "$(gate_rc)" 1
go_test_case skipped
cp "$tmp/lab-ok.json" "$FX/statuses-$sha.json" # a status on the merge commit is not the PR head's
check "a go-test-lab on another commit does not count" "$(gate_rc)" 1
go_test_case skipped
GIT_DIFF=docs/guide.md
check "go-test skipped with no Go in the change passes" "$(gate_rc)" 0
GIT_DIFF=
check "go-test skipped and the change cannot be read fails" "$(gate_rc)" 1
export GIT_DIFF=internal/x/x.go
go_test_case failure
check "go-test red on the PR head fails" "$(gate_rc)" 1
go_test_case ""
check "go-test skipped on main and absent on the PR head fails" "$(gate_rc)" 1

# #444 (ADR 0102): the quickstart on a queue-merged commit.
qs_case() { # a fresh case whose tagged commit has the given quickstart runs only (set by the caller after)
	new_case
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	run_on "$sha" go success
}
mg_run() { # <run id> <head sha> <tree of that head> [quickstart conclusion]: a green merge_group run
	local f=$FX/mg-runs.json
	[[ -f $f ]] || echo '{"workflow_runs":[]}' >"$f"
	jq --argjson i "$1" --arg h "$2" '.workflow_runs += [{id: $i, head_sha: $h, event: "merge_group", conclusion: "success"}]' "$f" >"$f.new"
	mv "$f.new" "$f"
	echo "{\"sha\":\"$2\",\"tree\":{\"sha\":\"$3\"}}" >"$FX/commit-$2.json"
	echo "{\"jobs\":[{\"id\":1,\"name\":\"decide\",\"conclusion\":\"success\"},{\"id\":2,\"name\":\"quickstart\",\"conclusion\":\"${4:-success}\"}]}" >"$FX/jobs-$1.json"
}
qs_case
run_on "$sha" quickstart success 2026-01-03T00:00:00Z
run_on "$sha" quickstart skipped 2026-01-03T00:30:00Z
check "quickstart: queue run green, then the push run skipped: passes" "$(gate_rc)" 0
check "...and reports the quickstart" "$(grep -c 'ok: kind e2e (quickstart)' "$tmp/out")" 1
qs_case
run_on "$sha" quickstart skipped
check "quickstart: only skipped runs fails" "$(gate_rc)" 1
qs_case
run_on "$sha" quickstart failure 2026-01-03T00:00:00Z
run_on "$sha" quickstart skipped 2026-01-03T00:30:00Z
check "quickstart: newest non-skipped run is a failure fails" "$(gate_rc)" 1
check "...and names the conclusion" "$(grep -c 'quickstart job): failure' "$tmp/err")" 1
qs_case
run_on "$sha" quickstart cancelled 2026-01-03T00:00:00Z
mg_run 77 mgsha tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a cancelled run is not excused by a queue run" "$(gate_rc)" 1
qs_case
mg_run 77 mgsha tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: no run on the commit, green queue run on the same tree passes" "$(gate_rc)" 0
check "...and names the queue run" "$(grep -c 'merge queue run 77' "$tmp/out")" 1
qs_case
run_on "$sha" quickstart skipped
mg_run 77 mgsha tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: only skipped on the commit plus a queue run on the same tree passes" "$(gate_rc)" 0
qs_case
mg_run 77 mgsha tree2
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a queue run on a different tree fails" "$(gate_rc)" 1
qs_case
mg_run 77 mgsha tree1 skipped
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a queue run whose quickstart job was skipped fails" "$(gate_rc)" 1
qs_case
mg_run 77 mgsha tree1
check "quickstart: the tagged commit's tree cannot be read fails" "$(gate_rc)" 1
check "...and says the queue runs could not be read" "$(grep -c 'could not be read' "$tmp/err")" 1
qs_case
mg_run 77 mgsha tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
rm "$FX/commit-mgsha.json"
check "quickstart: a queue run head whose tree cannot be read fails" "$(gate_rc)" 1
qs_case
mg_run 77 mgsha tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
rm "$FX/jobs-77.json"
check "quickstart: a queue run whose jobs cannot be read fails" "$(gate_rc)" 1

new_case
rm "$FX/rules.json"
check "unreadable rules fail closed" "$(gate_rc)" 1
check "...with a clear message" "$(grep -c 'cannot read the rules that apply' "$tmp/err")" 1

new_case
echo '[{"type":"deletion"}]' >"$FX/rules.json"
check "no required status checks fails closed" "$(gate_rc)" 1
check "...with a clear message (none)" "$(grep -c 'no required status checks apply' "$tmp/err")" 1

new_case
rm "$FX/repo.json"
check "an unreadable default branch fails closed" "$(gate_rc)" 1

new_case
check "a commit off main fails" "$(GIT_NOT_ANCESTOR=1 gate_rc)" 1
check "a bad tag fails" "$(TAG=1.2.3 gate_rc)" 1

echo "release-gate-test: $pass passed, $failed failed"
((failed == 0))
