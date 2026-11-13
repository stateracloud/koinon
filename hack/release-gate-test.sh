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
[[ $1 == tag ]] && { printf '%s\n' ${GIT_TAGS:-}; exit 0; }
[[ $1 == rev-list ]] && { cat "$FX/tag-${3}" 2>/dev/null || exit 1; exit 0; }
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
repos/o/r/actions/runs) f=qruns.json ;;
repos/o/r/actions/artifacts) f=drill-artifacts.json ;;
repos/o/r/actions/artifacts/*/zip) c=${path#repos/o/r/actions/artifacts/}; cat "$FX/artifact-${c%%/*}.zip" 2>/dev/null || { echo "gh stub: HTTP 404 $path" >&2; exit 1; }; exit ;;
repos/o/r/releases/tags/*) f=release-${path##*/}.json ;;
repos/o/r/releases/assets/*) cat "$FX/asset-${path##*/}.json" 2>/dev/null || { echo "gh stub: HTTP 404 $path" >&2; exit 1; }; exit ;;
repos/o/r/actions/runs/*/jobs) c=${path#repos/o/r/actions/runs/}; f=jobs-${c%%/*}.json ;;
repos/o/r/commits/*/statuses) c=${path#repos/o/r/commits/}; f=statuses-${c%%/*}.json ;;
*) echo "gh stub: unexpected $path" >&2; exit 1 ;;
esac
# the workflow runs of one commit: none unless a case adds some
[[ $f == mg-runs.json && $query == *head_sha=* && ! -f $FX/$f ]] && { echo '{"workflow_runs":[]}' | jq -r "$expr"; exit; }
# every merge_group workflow run of one head (qruns.json): none unless a case adds some
if [[ $f == qruns.json ]]; then
	[[ -f $FX/$f ]] || echo '{"workflow_runs":[]}' >"$FX/$f"
	hs=; [[ $query == *head_sha=* ]] && { hs=${query#*head_sha=}; hs=${hs%%&*}; }
	jq --arg hs "$hs" '.workflow_runs |= map(select(.head_sha == $hs))' "$FX/$f" | jq -r "$expr"
	exit
fi
# the merge_group ci runs: none unless a case adds some
if [[ $f == ci-runs.json ]]; then
	[[ -f $FX/$f ]] || echo '{"workflow_runs":[]}' >"$FX/$f"
	jq -r "$expr" "$FX/$f"
	exit
fi
# the drill-logs artifacts: none unless a case adds some
if [[ $f == drill-artifacts.json ]]; then
	[[ -f $FX/$f ]] || echo '{"artifacts":[]}' >"$FX/$f"
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
	kind_artifact 501 "$sha" "$sha" kind pass
}
set_ruleset() { # <context>...: the rules/branches answer
	local ctx
	ctx=$(printf '%s\n' "$@" | jq -R '{context: .}' | jq -s .)
	jq -n --argjson ctx "$ctx" \
		'[{type: "deletion"}, {type: "required_status_checks", ruleset_id: 7, parameters: {required_status_checks: $ctx}}]' >"$FX/rules.json"
}
default_run() { # <sha> <name>: ci is run 900; dco, claim and e2e result are their own workflows' runs
	[[ $1 == "$qh" ]] || { echo 1; return; }
	case $2 in dco) echo 910 ;; claim) echo 920 ;; "e2e result") echo 930 ;; *) echo 900 ;; esac
}
queue_wf_run() { # <id> <head sha>: a merge_group workflow run (any workflow) on that head
	local f=$FX/qruns.json
	[[ -f $f ]] || echo '{"workflow_runs":[]}' >"$f"
	jq --argjson i "$1" --arg h "$2" '.workflow_runs += [{id: $i, head_sha: $h, event: "merge_group"}]' "$f" >"$f.new"
	mv "$f.new" "$f"
}
run_on() { # <sha> <name> <conclusion> [started_at] [workflow run id]
	local f=$FX/checks-$1.json
	[[ -f $f ]] || echo '{"check_runs":[]}' >"$f"
	jq --arg n "$2" --arg c "$3" --arg t "${4:-2026-01-01T00:00:00Z}" '.check_runs += [{name: $n, started_at: $t, conclusion: $c, app: {id: 15368}, details_url: ("https://github.com/o/r/actions/runs/" + ($r | tostring) + "/job/1")}]' --argjson r "${5:-$(default_run "$1" "$2")}" "$f" >"$f.new"
	mv "$f.new" "$f"
}
ci_queue_run() { # <head sha of the queue commit> <tree id> [conclusion]: a merge_group ci run on the tree
	local f=$FX/ci-runs.json
	[[ -f $f ]] || echo '{"workflow_runs":[]}' >"$f"
	jq --arg h "$1" --arg t "$2" --arg c "${3:-success}" '.workflow_runs += [{id: 900, head_sha: $h, event: "merge_group", conclusion: $c, head_commit: {id: $h, tree_id: $t}}]' "$f" >"$f.new"
	mv "$f.new" "$f"
	local i
	for i in 900 910 920 930; do queue_wf_run "$i" "$1"; done
}
# The restore drill (#518). drill_result <file> <commit> <profile> <verdict> [check verdict]: a drill-result.json.
drill_result() {
	jq -n --arg c "$2" --arg p "$3" --arg v "$4" --arg cv "${5:-pass}" --arg nb "${NO_BROWSER:-0}" \
		'{runId: "abc123", commit: $c, topology: ({name: "t", profile: $p} + (if $p == "lab" then {browserSignin: true} else {} end)), checks: ([{name: "signin", verdict: "pass"}, {name: "hold", verdict: $cv}] + (if $p == "lab" and $nb != "1" then [{name: "browser-signin", verdict: "pass"}] else [] end)),
		  knownGaps: (if $cv == "known-gap" then [{name: "hold", issue: "#516", verdict: "known-gap"}] else [] end), verdict: $v}' >"$1"
}
kind_artifact() { # <artifact id> <head sha> <result commit> <profile> <verdict> [check verdict] [expired]: a drill-logs artifact
	local d=$FX/art.$1
	mkdir -p "$d/drill"
	drill_result "$d/drill/drill-result.json" "$3" "$4" "$5" "${6:-pass}"
	echo log >"$d/drill.log"
	(cd "$d" && zip -qr "$FX/artifact-$1.zip" drill.log drill)
	local f=$FX/drill-artifacts.json
	[[ -f $f ]] || echo '{"artifacts":[]}' >"$f"
	jq --argjson i "$1" --arg h "$2" --argjson e "${7:-false}" '.artifacts += [{id: $i, name: "drill-logs", expired: $e, workflow_run: {head_sha: $h, head_repository_id: 1, repository_id: 1}}]' "$f" >"$f.new"
	mv "$f.new" "$f"
}
n=0
gate_rc() { # TAG env as given: exit code; stderr to $tmp/err, stdout to $tmp/out
	local rc=0
	PATH=$tmp/bin:$PATH TAG=${TAG:-v1.2.3-rc.1} SHA=$sha GITHUB_REPOSITORY=o/r GH_TOKEN=koinon_test_not-a-real-token \
		bash "$gate" >"$tmp/out" 2>"$tmp/err" || rc=$?
	echo "$rc"
}

new_case
check "everything green passes" "$(gate_rc)" 0
check "pass prints the PR-only contexts" "$(grep -c '^ok: ' "$tmp/out")" 5

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

# contributor gate (#848) has no push trigger: once required, its pull request run decides
new_case
run_on "$pr_head" "contributor gate" success
set_ruleset go dco "e2e result" "contributor gate"
check "a required contributor gate is read from the PR head" "$(gate_rc)" 0

new_case
run_on "$pr_head" "contributor gate" failure
set_ruleset go dco "e2e result" "contributor gate"
check "a red contributor gate on the PR head fails" "$(gate_rc)" 1

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
	kind_artifact 512 "$qh" "$qh" kind pass
	echo "{\"sha\":\"$sha\",\"tree\":{\"sha\":\"treeS\"}}" >"$FX/commit-$sha.json"
	echo '{"check_runs":[]}' >"$FX/checks-$sha.json"
	run_on "$sha" quickstart success
	ci_queue_run "$sha" treeS
	run_on "$sha" go success "2026-01-01T00:00:00Z" 900
	run_on "$sha" dco success "2026-01-01T00:00:00Z" 910
	run_on "$sha" "e2e result" success "2026-01-01T00:00:00Z" 930
	run_on "$sha" go-test "$1" "2026-01-01T00:00:00Z" 900
	for c in go go-test dco "e2e result"; do run_on "$sha" "$c" skipped "2026-01-01T01:00:00Z" 901; done
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
# pr_only contexts on a fast-forward commit: the queue's own dco / e2e result run decides, not the newer push
# skipped and not the PR head (where they passed).
for c in dco "e2e result"; do
	ff_case success
	run_on "$pr_head" "$c" success
	jq --arg c "$c" '.check_runs |= map(if .name == $c and (.details_url | test("/runs/(910|930)/")) then .conclusion = "failure" else . end)' "$FX/checks-$qh.json" >"$FX/x" && mv "$FX/x" "$FX/checks-$qh.json"
	check "fast-forward queue commit: a red queue $c with a green PR head and a newer push skipped is refused" "$(gate_rc)" 1
done
# contributor gate (#848) in the queue run: success passes; skipped (a commit queued while the switch was off,
# docs/maintainers/contributor-gate.md) is refused, not excused by the PR head.
for c in success skipped; do
	ff_case success
	set_ruleset go dco "e2e result" "contributor gate"
	run_on "$qh" "contributor gate" "$c" "2026-01-01T00:00:00Z" 900
	run_on "$pr_head" "contributor gate" success
	check "queue run with contributor gate $c" "$(gate_rc)" "$([[ $c == success ]] && echo 0 || echo 1)"
done
ff_case success
set_ruleset go dco claim "e2e result"
run_on "$qh" claim failure "2026-01-01T00:00:00Z" 920
run_on "$qh" claim skipped "2026-01-01T01:00:00Z" 901
run_on "$pr_head" claim success
check "fast-forward queue commit: a red queue claim with a newer push skipped is refused" "$(gate_rc)" 1
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

# --- the restore drill (#518): rc needs a green kind drill, a final tag a green lab drill for its rc ---
new_case
check "an rc with a green kind drill passes" "$(gate_rc)" 0
check "...and says so" "$(grep -c '^ok: kind restore drill (artifact 501; known gaps: none)' "$tmp/out")" 1

new_case
rm "$FX/drill-artifacts.json"
check "an rc with no drill artifact fails" "$(gate_rc)" 1
check "...and says to run the drill" "$(grep -c 'no drill-logs artifact for aaaa111' "$tmp/err")" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 502 other999 other999 kind pass
check "a drill artifact for another commit does not count" "$(gate_rc)" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 503 "$sha" "$sha" kind fail
check "a red kind drill fails" "$(gate_rc)" 1
check "...with its verdict" "$(grep -c 'its verdict is fail' "$tmp/err")" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 504 "$sha" other999 kind pass
check "a result for another commit inside an artifact of this one fails" "$(gate_rc)" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 505 "$sha" "$sha" lab pass
check "a lab result does not stand in for the kind drill" "$(gate_rc)" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 506 "$sha" "$sha" kind pass pass true
check "an expired artifact does not count" "$(gate_rc)" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 507 "$sha" "$sha" kind pass notrun
check "a notrun check makes the verdict untrustworthy even when the file says pass" "$(gate_rc)" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 508 "$sha" "$sha" kind pass known-gap
check "a kind drill that failed only on a known gap passes" "$(gate_rc)" 0
check "...and prints the gap" "$(grep -c 'known gaps: #516 hold' "$tmp/out")" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 509 "$sha" "$sha" kind fail
kind_artifact 510 "$sha" "$sha" kind pass
check "a later green artifact for the commit is enough after a red one" "$(gate_rc)" 0

new_case
rm "$FX/artifact-501.zip"
check "an artifact that cannot be downloaded fails closed" "$(gate_rc)" 1

new_case
rm "$FX/drill-artifacts.json"
mkdir -p "$FX/empty" && echo log >"$FX/empty/drill.log" && (cd "$FX/empty" && zip -q "$FX/artifact-511.zip" drill.log)
echo '{"artifacts":[{"id":511,"name":"drill-logs","expired":false,"workflow_run":{"head_sha":"'$sha'","head_repository_id":1,"repository_id":1}}]}' >"$FX/drill-artifacts.json"
check "an artifact with no drill-result.json fails (the suite did not finish)" "$(gate_rc)" 1
check "...and says so" "$(grep -c 'has no drill-result.json' "$tmp/err")" 1

new_case
rm "$FX/drill-artifacts.json"
kind_artifact 513 "$sha" "$sha" kind pass
cat "$FX/art.513/drill/drill-result.json" >>"$FX/art.513/drill/drill-result.json.2"
jq -c '.verdict = "fail"' "$FX/art.513/drill/drill-result.json" >"$FX/art.513/drill/drill-result.json.red"
cat "$FX/art.513/drill/drill-result.json.red" "$FX/art.513/drill/drill-result.json.2" >"$FX/art.513/drill/drill-result.json.both"
mv "$FX/art.513/drill/drill-result.json.both" "$FX/art.513/drill/drill-result.json"
rm -f "$FX/art.513/drill/"drill-result.json.* "$FX/artifact-513.zip"
(cd "$FX/art.513" && zip -qr "$FX/artifact-513.zip" drill.log drill)
check "a file with a red result followed by a green one is not green" "$(gate_rc)" 1

# final tag: the lab drill for the rc it promotes
rc_sha=rrrr444
final_case() { # a final tag at $sha; rc.2 is on the same tree (commit rrrr444), rc.1 on another
	new_case
	rm "$FX/drill-artifacts.json"
	export GIT_TAGS="v1.2.3-rc.1 v1.2.3-rc.2 v1.2.3-rc.10"
	echo "$rc_sha" >"$FX/tag-v1.2.3-rc.2"
	echo "$rc_sha" >"$FX/tag-v1.2.3-rc.10"
	echo oldold5 >"$FX/tag-v1.2.3-rc.1"
	echo "{\"sha\":\"$rc_sha\",\"tree\":{\"sha\":\"treeS\"}}" >"$FX/commit-$rc_sha.json"
	echo '{"sha":"oldold5","tree":{"sha":"treeOLD"}}' >"$FX/commit-oldold5.json"
}
lab_asset() { # <rc tag> <asset id> <result commit> <profile> <verdict>
	echo "{\"assets\":[{\"id\":$2,\"name\":\"drill-result-lab.json\"}]}" >"$FX/release-$1.json"
	drill_result "$FX/asset-$2.json" "$3" "$4" "$5"
}
final_rc() { TAG=v1.2.3 GIT_TAGS="${GIT_TAGS:-}" gate_rc; }

final_case
lab_asset v1.2.3-rc.10 601 "$rc_sha" lab pass
check "a final tag with a green lab drill of its rc passes" "$(final_rc)" 0
check "...naming the rc (the highest N on the tree, rc.10 over rc.2)" "$(grep -c '^ok: lab restore drill (v1.2.3-rc.10 at rrrr444' "$tmp/out")" 1
check "...and it asks for no kind drill" "$(grep -c 'kind restore drill' "$tmp/out")" 0

final_case
echo '{"assets":[]}' >"$FX/release-v1.2.3-rc.10.json"
check "a final tag whose rc release has no lab result fails" "$(final_rc)" 1
check "...naming the asset" "$(grep -c 'has no drill-result-lab.json' "$tmp/err")" 1

final_case
echo '{"assets":[{"id":1,"name":"drill-result.json"}]}' >"$FX/release-v1.2.3-rc.10.json"
check "the kind result on the rc release is not a lab result" "$(final_rc)" 1

final_case
lab_asset v1.2.3-rc.10 602 "$rc_sha" kind pass
check "a kind result under the lab name fails" "$(final_rc)" 1
check "...with the profile" "$(grep -c 'its profile is kind, not lab' "$tmp/err")" 1

final_case
NO_BROWSER=1 lab_asset v1.2.3-rc.10 607 "$rc_sha" lab pass
check "a lab result without a passing browser-signin check fails" "$(final_rc)" 1
check "...saying so" "$(grep -c 'no passing browser-signin check' "$tmp/err")" 1

final_case
lab_asset v1.2.3-rc.10 603 oldold5 lab pass
check "a lab result for another commit than the rc fails" "$(final_rc)" 1

final_case
lab_asset v1.2.3-rc.10 604 "$rc_sha" lab fail
check "a red lab drill fails" "$(final_rc)" 1

final_case
echo '{"sha":"'$sha'","tree":{"sha":"treeS"}}' >"$FX/commit-$sha.json"
echo "{\"sha\":\"$rc_sha\",\"tree\":{\"sha\":\"treeOTHER\"}}" >"$FX/commit-$rc_sha.json"
lab_asset v1.2.3-rc.10 605 "$rc_sha" lab pass
check "a final tag on a tree no rc has fails" "$(final_rc)" 1
check "...saying a final tag promotes an rc" "$(grep -c 'a final tag promotes an rc' "$tmp/err")" 1

final_case
export GIT_TAGS=
check "a final tag with no rc tag at all fails" "$(final_rc)" 1

final_case
lab_asset v1.2.3-rc.10 606 "$rc_sha" lab pass
rm "$FX/asset-606.json"
check "a lab result that cannot be downloaded fails closed" "$(final_rc)" 1
unset GIT_TAGS

echo "release-gate-test: $pass passed, $failed failed"
((failed == 0))
