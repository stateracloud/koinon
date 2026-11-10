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
path= expr=. pag=
while (($#)); do
	case $1 in
	--paginate) pag=1 ;;
	--jq) expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
query=
[[ $path == *\?* ]] && query=${path#*\?}
path=${path%%\?*}
case $path in
repos/o/r) f=repo.json ;;
repos/o/r/rules/branches/main) f=rules.json ;;
repos/o/r/commits/*/check-runs) c=${path#repos/o/r/commits/}; f=checks-${c%%/*}.json ;;
repos/o/r/commits/*/pulls) f=pulls.json ;;
repos/o/r/git/commits/*) f=commit-${path##*/}.json ;;
repos/o/r/actions/workflows/e2e-kind.yml/runs) f=mg-runs.json ;;
repos/o/r/actions/workflows/ci.yml/runs) f=ci-runs.json ;;
repos/o/r/actions/runs/*/jobs) c=${path#repos/o/r/actions/runs/}; f=jobs-${c%%/*}.json ;;
repos/o/r/commits/*/statuses) c=${path#repos/o/r/commits/}; f=statuses-${c%%/*}.json ;;
*) echo "gh stub: unexpected $path" >&2; exit 1 ;;
esac
# the workflow runs of one commit: none unless a case adds some
[[ $f == mg-runs.json && $query == *head_sha=* && ! -f $FX/$f ]] && { echo '{"workflow_runs":[]}' | jq -r "$expr"; exit; }
# the merge_group ci runs: none unless a case adds some
if [[ $f == ci-runs.json ]]; then
	[[ -f $FX/$f ]] || echo '{"workflow_runs":[]}' >"$FX/$f"
	jq -r "$expr" "$FX/$f"
	exit
fi
# a second page (mg-runs-p2.json) follows the first only when --paginate is given and a case adds one
[[ -f $FX/$f ]] || { echo "gh stub: HTTP 404 $path" >&2; exit 1; }
# the runs endpoint honours event= and status= like GitHub (status=success is a run's conclusion here)
if [[ $f == mg-runs.json ]]; then
	ev= st= hs=
	IFS='&' read -ra kv <<<"$query"
	for p in "${kv[@]}"; do
		case $p in event=*) ev=${p#event=} ;; status=*) st=${p#status=} ;; head_sha=*) hs=${p#head_sha=} ;; esac
	done
	for pg in "$FX/$f" "$FX/mg-runs-p2.json"; do
		[[ -f $pg ]] || continue
		[[ $pg == */mg-runs-p2.json && -z $pag ]] && continue
		jq --arg ev "$ev" --arg st "$st" --arg hs "$hs" '.workflow_runs |= map(select(($ev == "" or .event == $ev) and ($st == "" or .conclusion == $st) and ($hs == "" or .head_sha == $hs)))' "$pg" |
			jq '.total_count //= (.workflow_runs | length)' | jq -r "$expr"
	done
	exit
fi
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
qh=qqqq333

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
	echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"treeS\"}}" >"$FX/commit-$sha.json"
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
run_on() { # <sha> <name> <conclusion> [started_at] [workflow run id]
	local f=$FX/checks-$1.json
	[[ -f $f ]] || echo '{"check_runs":[]}' >"$f"
	jq --arg n "$2" --arg c "$3" --arg t "${4:-2026-01-01T00:00:00Z}" '.check_runs += [{name: $n, started_at: $t, conclusion: $c, app: {id: 15368}, details_url: ("https://github.com/o/r/actions/runs/" + ($r | tostring) + "/job/1")}]' --argjson r "${5:-$([[ $1 == "$qh" ]] && echo 900 || echo 1)}" "$f" >"$f.new"
	mv "$f.new" "$f"
}
ci_queue_run() { # <head sha of the queue commit> <tree id> [conclusion]: a merge_group ci run on the tree
	local f=$FX/ci-runs.json
	[[ -f $f ]] || echo '{"workflow_runs":[]}' >"$f"
	jq --arg h "$1" --arg t "$2" --arg c "${3:-success}" '.workflow_runs += [{id: 900, head_sha: $h, event: "merge_group", conclusion: $c, head_commit: {id: $h, tree_id: $t}}]' "$f" >"$f.new"
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
# main skipped the heavy job because the merge queue tested the tree: the queue build is the evidence.
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
ci_queue_run "$qh" treeS
run_on "$qh" go success
run_on "$qh" dco success
run_on "$qh" "e2e result" success
check "a check skipped on main is read from the queue build (green)" "$(gate_rc)" 0

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
ci_queue_run "$qh" treeS
run_on "$qh" go failure
run_on "$qh" dco success
run_on "$qh" "e2e result" success
check "a check skipped on main and red in the queue build fails" "$(gate_rc)" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
run_on "$pr_head" go success
check "a check skipped on main is NOT read from the PR head (no queue run fails)" "$(gate_rc)" 1
check "...and says why" "$(grep -c 'no successful merge_group ci run has this tree' "$tmp/err")" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
ci_queue_run "$qh" otherTree
run_on "$qh" go success
check "a queue run on another tree does not count" "$(gate_rc)" 1

new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
ci_queue_run "$qh" treeS failure
run_on "$qh" go success
check "a failed queue run does not count" "$(gate_rc)" 1

new_case
echo '[]' >"$FX/pulls.json"
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" quickstart success
run_on "$sha" go skipped
check "a check skipped on main with no queue run and no merged PR fails" "$(gate_rc)" 1

# Queue commit: dco and e2e result come from the queue build, not the PR head.
new_case
echo '{"check_runs":[]}' >"$FX/checks-$pr_head.json"
ci_queue_run "$qh" treeS
run_on "$qh" dco success
run_on "$qh" "e2e result" success
check "queue commit: dco and e2e result from the queue build pass without a PR head" "$(gate_rc)" 0
new_case
run_on "$pr_head" dco failure "2026-01-01T12:00:00Z"
ci_queue_run "$qh" treeS
run_on "$qh" dco success
run_on "$qh" "e2e result" success
check "queue commit: a PR head's red dco is not read when the queue reported it" "$(gate_rc)" 0
new_case
ci_queue_run "$qh" treeS
run_on "$qh" dco failure
run_on "$qh" "e2e result" success
check "queue commit: a red dco in the queue build fails" "$(gate_rc)" 1
# Admin commit: no queue run; ci contexts on the commit itself, dco and e2e result from the PR head.
new_case
check "admin commit: ci from the push run, dco and e2e result from the PR head" "$(gate_rc)" 0
new_case
echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
run_on "$sha" go success
echo '{"workflow_runs":[]}' >"$FX/mg-runs.json"
check "admin commit: quickstart missing fails with the dispatch hint" "$(gate_rc)" 1
check "...and says to run e2e-kind" "$(grep -c 'run the e2e-kind workflow' "$tmp/err")" 1

# The queue commit is the tagged commit (a fast-forward merge): the sha has the merge_group run's result
# and the push run's later skipped; the queue run decides, not the newest check run on the sha.
ff_case() { # <go-test conclusion of the merge_group run>
	new_case
	sha=$qh
	echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"treeS\"}}" >"$FX/commit-$sha.json"
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	run_on "$sha" quickstart success
	ci_queue_run "$sha" treeS
	for c in go dco "e2e result"; do run_on "$sha" "$c" success "2026-01-01T00:00:00Z" 900; done
	run_on "$sha" go-test "$1" "2026-01-01T00:00:00Z" 900
	for c in go go-test; do run_on "$sha" "$c" skipped "2026-01-01T01:00:00Z" 901; done
}
export GIT_DIFF=internal/x/x.go
ff_case success
check "fast-forward queue commit: merge_group success beats the newer push skipped" "$(gate_rc)" 0
ff_case failure
check "fast-forward queue commit: merge_group failure plus push skipped is refused" "$(gate_rc)" 1
ff_case success
jq '.check_runs |= map(select(.details_url | test("/runs/900/") | not))' "$FX/checks-$qh.json" >"$FX/x" && mv "$FX/x" "$FX/checks-$qh.json"
check "fast-forward queue commit: only the push skipped, no queue result, is refused" "$(gate_rc)" 1
ff_case success
jq '.check_runs |= map(if .details_url | test("/runs/900/") then .app.id = 1 else . end)' "$FX/checks-$qh.json" >"$FX/x" && mv "$FX/x" "$FX/checks-$qh.json"
check "fast-forward queue commit: a run not from the Actions app is not read" "$(gate_rc)" 1
sha=aaaa111
unset GIT_DIFF

# go-test: skipped on main (the queue ran it), the queue build decides; or the change has no Go.
go_test_case() { # a fresh case in which main skipped go-test, the queue build's go-test is $1 ("" = no run)
	new_case
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	run_on "$sha" quickstart success
	run_on "$sha" go skipped
	run_on "$sha" go-test skipped
	ci_queue_run "$qh" treeS
	run_on "$qh" go success
	run_on "$qh" dco success
	run_on "$qh" "e2e result" success
	[[ -z $1 ]] || run_on "$qh" go-test "$1"
}
export GIT_DIFF=internal/x/x.go
go_test_case success
check "go-test green in the queue build passes" "$(gate_rc)" 0
go_test_case skipped
check "go-test skipped in the queue build with Go in the change fails" "$(gate_rc)" 1
check "...and names go-test" "$(grep -c 'FAIL: go-test: skipped in the merge queue run' "$tmp/err")" 1
go_test_case skipped
echo '[{"state":"success","context":"go-test-lab"}]' >"$FX/statuses-$pr_head.json"
check "a go-test-lab status on the PR head no longer excuses a skipped go-test" "$(gate_rc)" 1
go_test_case skipped
GIT_DIFF=docs/guide.md
check "go-test skipped with no Go in the change passes" "$(gate_rc)" 0
GIT_DIFF=
check "go-test skipped and the change cannot be read fails" "$(gate_rc)" 1
export GIT_DIFF=internal/x/x.go
go_test_case failure
check "go-test red in the queue build fails" "$(gate_rc)" 1
go_test_case ""
check "go-test skipped on main and absent in the queue build fails" "$(gate_rc)" 1

# #444 (ADR 0102): the quickstart on a queue-merged commit.
qs_case() { # a fresh case whose tagged commit has the given quickstart runs only (set by the caller after)
	new_case
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	run_on "$sha" go success
}
mg_run() { # <run id> <head sha> <tree of that head> [quickstart conclusion] [event] [run conclusion]: a merge_group run, green by default
	local f=$FX/mg-runs.json
	[[ -f $f ]] || echo '{"workflow_runs":[]}' >"$f"
	jq --argjson i "$1" --arg h "$2" --arg e "${5:-merge_group}" --arg c "${6:-success}" '.workflow_runs += [{id: $i, head_sha: $h, event: $e, conclusion: $c}]' "$f" >"$f.new"
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
rm -f "$FX/commit-$sha.json"
check "quickstart: the tagged commit's tree cannot be read fails" "$(gate_rc)" 1
check "...and says what could not be read" "$(grep -c 'could not be read\|cannot read the tree' "$tmp/err")" 1
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

# the query parameters are honoured: a failed run and a run of another event are not queue evidence
qs_case
mg_run 77 mgsha tree1 success merge_group failure
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a failed merge_group run on the same tree fails" "$(gate_rc)" 1
qs_case
mg_run 77 mgsha tree1 success workflow_dispatch
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a green run of another event on the same tree fails" "$(gate_rc)" 1
qs_case
mg_run 77 mgsha tree1 success workflow_dispatch
mg_run 78 mgsha2 tree1 success merge_group failure
mg_run 79 mgsha3 tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a good run among filtered-out ones passes" "$(gate_rc)" 0
check "...and names the good run" "$(grep -c 'merge queue run 79' "$tmp/out")" 1
# the bound: only the newest QUEUE_RUN_LIMIT runs are read
qs_case
mg_run 80 mgsha4 tree2
mg_run 81 mgsha5 tree1
echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"tree1\"}}" >"$FX/commit-$sha.json"
check "quickstart: a queue run beyond the limit is not looked at" "$(QUEUE_RUN_LIMIT=1 gate_rc)" 1
# #453: a bisect dispatch (inputs.commit) is named `quickstart (commit)` and is no evidence for the head
qs_case
run_on "$sha" quickstart failure 2026-01-03T00:00:00Z
run_on "$sha" "quickstart (commit)" success 2026-01-03T01:00:00Z
check "quickstart: a green bisect dispatch does not excuse a red main head" "$(gate_rc)" 1
qs_case
run_on "$sha" "quickstart (commit)" success
check "quickstart: only a bisect dispatch is no evidence" "$(gate_rc)" 1
# the head_sha == SHA shortcut: the tagged commit is itself the queue commit, so no commit lookup is needed
qs_case
mg_run 90 "$sha" tree1 # writes commit-$sha.json too; the loop itself needs no lookup for a head equal to SHA
check "quickstart: a green queue run on the tagged commit itself passes" "$(gate_rc)" 0
check "...and names that queue run" "$(grep -c 'merge queue run 90' "$tmp/out")" 1
# QUEUE_RUN_LIMIT is a per_page: 1..100 only
for bad in 0 101 abc -1 3.5; do
	qs_case
	check "QUEUE_RUN_LIMIT '$bad' fails closed" "$(QUEUE_RUN_LIMIT=$bad gate_rc)" 1
	check "...with a clear message ('$bad')" "$(grep -c 'QUEUE_RUN_LIMIT .* is not an integer from 1 to 100' "$tmp/err")" 1
done
for ok in 1 30 100; do
	qs_case
	run_on "$sha" quickstart success
	check "QUEUE_RUN_LIMIT $ok is accepted" "$(QUEUE_RUN_LIMIT=$ok gate_rc)" 0
done
# #453: runs from before the fix still carry the name `quickstart`; their workflow run is titled as a bisect
bisect_run() { # <run id> <sha> <display title>
	local f=$FX/mg-runs.json
	[[ -f $f ]] || echo '{"workflow_runs":[]}' >"$f"
	jq --argjson i "$1" --arg h "$2" --arg t "$3" '.workflow_runs += [{id: $i, head_sha: $h, event: "workflow_dispatch", conclusion: "success", display_title: $t}]' "$f" >"$f.new"
	mv "$f.new" "$f"
}
qs_case
run_on "$sha" quickstart failure 2026-01-03T00:00:00Z 10
run_on "$sha" quickstart success 2026-01-03T01:00:00Z 11
bisect_run 11 "$sha" "e2e-kind (bbbb222) bisect 7"
check "quickstart: an old-style green bisect run does not excuse a red head" "$(gate_rc)" 1
qs_case
run_on "$sha" quickstart success 2026-01-03T01:00:00Z 11
bisect_run 11 "$sha" "e2e-kind (bbbb222) bisect 7"
check "quickstart: only an old-style bisect run is no evidence" "$(gate_rc)" 1
qs_case
run_on "$sha" quickstart success 2026-01-03T01:00:00Z 12
bisect_run 12 "$sha" "e2e-kind"
check "quickstart: a plainly titled run still counts" "$(gate_rc)" 0
# more runs on the commit than one page holds: a bisect run could hide beyond it, so fail closed
qs_case
run_on "$sha" quickstart success 2026-01-03T01:00:00Z 11
jq --arg h "$sha" '.total_count = 101 | .workflow_runs = [range(100) as $i | {id: (1000 + $i), head_sha: $h, event: "push", conclusion: "success", display_title: "e2e-kind"}]' <<<'{}' >"$FX/mg-runs.json"
check "quickstart: total_count 101 with 100 runs fails closed" "$(gate_rc)" 1
check "...with a clear message" "$(grep -c 'read 100 of 101 e2e-kind runs on .*: cannot rule out a bisect run' "$tmp/err")" 1
jq '.total_count = 100' "$FX/mg-runs.json" >"$FX/mg-runs.new" && mv "$FX/mg-runs.new" "$FX/mg-runs.json"
check "quickstart: total_count equal to the runs returned passes" "$(gate_rc)" 0
# the only bisect run is on the second page: it is found, so its newer red check run is dropped and the
# older green one passes the gate (a lookup that reads only page 1 fails closed instead)
qs_case
run_on "$sha" quickstart success 2026-01-03T00:00:00Z 10
run_on "$sha" quickstart failure 2026-01-03T01:00:00Z 1100
jq --arg h "$sha" '.total_count = 101 | .workflow_runs = [range(100) as $i | {id: (1000 + $i), head_sha: $h, event: "push", conclusion: "success", display_title: "e2e-kind"}]' <<<'{}' >"$FX/mg-runs.json"
jq --arg h "$sha" '.total_count = 101 | .workflow_runs = [{id: 1100, head_sha: $h, event: "workflow_dispatch", conclusion: "success", display_title: "e2e-kind (bbbb222) bisect 7"}]' <<<'{}' >"$FX/mg-runs-p2.json"
check "quickstart: a bisect run on the second page is found and dropped" "$(gate_rc)" 0
# GitHub's cap for a filtered run list (1000 results): fail closed
jq '.total_count = 1000' "$FX/mg-runs.json" >"$FX/mg-runs.new" && mv "$FX/mg-runs.new" "$FX/mg-runs.json"
jq '.total_count = 1000' "$FX/mg-runs-p2.json" >"$FX/mg-runs.new" && mv "$FX/mg-runs.new" "$FX/mg-runs-p2.json"
check "quickstart: total_count at GitHub's 1000 cap fails closed" "$(gate_rc)" 1
check "...with a clear message (cap)" "$(grep -c '1000 or more e2e-kind runs' "$tmp/err")" 1
# the runs read equal total_count = 1000: only the cap check can stop it
qs_case
run_on "$sha" quickstart success 2026-01-03T01:00:00Z 11
jq --arg h "$sha" '.total_count = 1000 | .workflow_runs = [range(1000) as $i | {id: (2000 + $i), head_sha: $h, event: "push", conclusion: "success", display_title: "e2e-kind"}]' <<<'{}' >"$FX/mg-runs.json"
check "quickstart: 1000 runs read, total_count 1000 fails closed" "$(gate_rc)" 1
check "...with the cap message" "$(grep -c '1000 or more e2e-kind runs' "$tmp/err")" 1
# a run inserted while paging repeats on page 2: the distinct ids read stay below total_count, so one run went unread
qs_case
run_on "$sha" quickstart success 2026-01-03T01:00:00Z 11
jq --arg h "$sha" '.total_count = 102 | .workflow_runs = [range(100) as $i | {id: (1000 + $i), head_sha: $h, event: "push", conclusion: "success", display_title: "e2e-kind"}]' <<<'{}' >"$FX/mg-runs.json"
jq --arg h "$sha" '.total_count = 102 | .workflow_runs = [range(98; 100) as $i | {id: (1000 + $i), head_sha: $h, event: "push", conclusion: "success", display_title: "e2e-kind"}]' <<<'{}' >"$FX/mg-runs-p2.json"
check "quickstart: a duplicate across pages does not count as a run read" "$(gate_rc)" 1
check "...with the unread message" "$(grep -c 'read 100 of 102 e2e-kind runs' "$tmp/err")" 1

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
