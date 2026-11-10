#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/ci-skip-heavy.sh: a scratch git repository with real merge commits and a fake
# `gh` that answers from fixture files (jq applies the script's --jq expression, as gh does).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

script=$(cd "$(dirname "$0")" && pwd)/ci-skip-heavy.sh
command -v jq >/dev/null || { echo "SKIPPED: jq is missing"; exit 0; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-skip-heavy-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

# --- fake gh ---------------------------------------------------------------------------------------
mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
# fake gh api <path> [--paginate] [--jq EXPR]; fixtures in $FAKE_DIR; FAKE_FAIL=1 fails every call.
[[ ${FAKE_FAIL:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }
path="" jq_expr="."
shift # api
while (($#)); do
	case $1 in
	--paginate) ;;
	--jq) jq_expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
case $path in
repos/o/r/pulls/*) f=$FAKE_DIR/pull.json ;;
repos/o/r/actions/workflows/ci.yml/runs*) [[ ${FAKE_FAIL_RUNS:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }; f=$FAKE_DIR/mq-runs.json ;;
repos/o/r/git/commits/*) f=$FAKE_DIR/commit-${path##*/}.json; [[ -f $f ]] || { echo "gh: HTTP 404" >&2; exit 1; } ;;
repos/o/r/commits/*/check-runs*) f=$FAKE_DIR/checks.json ;;
repos/o/r/commits/*/statuses*) [[ ${FAKE_FAIL_STATUSES:-0} == 0 ]] || { echo "gh: HTTP 403" >&2; exit 1; }; f=$FAKE_DIR/statuses.json ;;
*) echo "fake gh: unknown $path" >&2; exit 1 ;;
esac
jq -r "$jq_expr" "$f"
EOF
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp"

export REPO=o/r

fails=0
# expect NAME EXPECTED_SKIP REASON_SUBSTRING [env...]: runs `decide` with the given environment.
expect() {
	local name=$1 want=$2 sub=$3 out
	shift 3
	out=$(env "$@" "$script" decide 2>&1) || true
	if grep -qx "skip=$want" <<<"$out" && grep -q -- "reason=.*$sub" <<<"$out"; then
		echo "ok: $name"
	else
		echo "FAIL: $name (want skip=$want, reason containing '$sub')"
		printf '    %s\n' "${out//$'\n'/$'\n    '}"
		fails=1
	fi
}

# --- pull requests ---------------------------------------------------------------------------------
# The decision comes from the event payload flag (PR_DRAFT), never from the API (old issue 676): the fake
# pull.json says the opposite, and FAKE_FAIL=1 makes every gh call fail, to prove gh is not asked.
echo '{"draft":false}' >"$tmp/pull.json"
expect "draft event skips, whatever the live state" true "draft" EVENT_NAME=pull_request PR_DRAFT=true
expect "draft event skips with the API down" true "draft" EVENT_NAME=pull_request PR_DRAFT=true FAKE_FAIL=1
echo '{"draft":true}' >"$tmp/pull.json"
expect "ready event runs everything, whatever the live state" false "ready" EVENT_NAME=pull_request PR_DRAFT=false
expect "ready event runs everything with the API down" false "ready" EVENT_NAME=pull_request PR_DRAFT=false FAKE_FAIL=1
expect "odd draft flag runs everything" false "no draft flag" EVENT_NAME=pull_request PR_DRAFT=maybe
expect "no draft flag runs everything" false "no draft flag" EVENT_NAME=pull_request PR_DRAFT=
# Recorded payloads: the shape of github.event.pull_request.draft for the event types ci.yml listens to.
for ev in opened synchronize reopened; do
	expect "recorded $ev payload of a draft" true "draft" EVENT_NAME=pull_request PR_DRAFT="$(printf '{"action":"%s","pull_request":{"draft":true}}' "$ev" | jq -r .pull_request.draft)"
done
expect "recorded ready_for_review payload" false "ready" EVENT_NAME=pull_request PR_DRAFT="$(echo '{"action":"ready_for_review","pull_request":{"draft":false}}' | jq -r .pull_request.draft)"
# A re-run of a draft run after the PR became ready replays the draft payload: still a draft run.
expect "re-run of a draft run after ready is a draft run again" true "draft" EVENT_NAME=pull_request PR_DRAFT=true
# ci.yml labels the run from the same flag as it feeds the script: pr-verdict.sh filters on that title.
if grep -q "^run-name: .*github.event.pull_request.draft.*'ci (draft)'" "$(dirname "$script")/../.github/workflows/ci.yml"; then
	echo "ok: ci.yml run-name labels draft runs"
else
	echo "FAIL: ci.yml run-name does not label draft runs"; fails=1
fi
# shellcheck disable=SC2016 # a literal workflow expression, not a shell one
if grep -q 'PR_DRAFT: ${{ github.event.pull_request.draft }}' "$(dirname "$script")/../.github/workflows/ci.yml"; then
	echo "ok: ci.yml feeds PR_DRAFT from the event"
else
	echo "FAIL: ci.yml does not feed PR_DRAFT from the event"; fails=1
fi
expect "other event runs everything" false "event" EVENT_NAME=schedule
expect "a merge group build runs everything (#225)" false "merge group" EVENT_NAME=merge_group
expect "a merge group build runs everything with the API down" false "merge group" EVENT_NAME=merge_group FAKE_FAIL=1

# --- push to main ----------------------------------------------------------------------------------
repo="$tmp/repo"
git init -q -b main "$repo"
g() { git -C "$repo" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false "$@"; }
echo a >"$repo/a"; g add a; g commit -qm base
g checkout -qb feat; echo b >"$repo/b"; g add b; g commit -qm feat
feat_head=$(g rev-parse HEAD)
g checkout -q main
g merge -q --no-ff feat -m "Merge feat" # main did not move: the merge tree equals the PR head tree
merge_same=$(g rev-parse HEAD)
g checkout -qb feat2 HEAD~1; echo c >"$repo/c"; g add c; g commit -qm feat2
g checkout -q main; echo d >"$repo/d"; g add d; g commit -qm "main moved"
g merge -q --no-ff feat2 -m "Merge feat2" # main moved after feat2 branched: tree differs from the head
merge_diff=$(g rev-parse HEAD)
plain=$(g rev-parse HEAD~1) # a one-parent commit
# a merge that brings a file the flavors suite builds from
g checkout -qb feat3; mkdir -p "$repo/flavors"; echo f >"$repo/flavors/x"; g add flavors; g commit -qm feat3
g checkout -q main; g merge -q --no-ff feat3 -m "Merge feat3"
merge_flav=$(g rev-parse HEAD)
export CI_CHANGES_FLAVORS_DIRS=test/flavors # the directories the flavors suite builds (no go list in the scratch repo)

cd "$repo"
push() { echo "EVENT_NAME=push REF=refs/heads/main SHA=$1"; }
# The queue build: a successful merge_group `ci` run whose head commit has the pushed commit's tree.
tree_of() { git -C "$repo" rev-parse "$1^{tree}"; }
commit_json() { # commit_json SHA TREE
	printf '{"sha":"%s","tree":{"sha":"%s"}}' "$1" "$2" >"$tmp/commit-$1.json"
}
mq_runs() { # mq_runs SHA... (newest first) -> mq-runs.json (call commit_json first: the run's tree comes from it)
	local first=1 s tree
	printf '{"workflow_runs":[' >"$tmp/mq-runs.json"
	for s in "$@"; do
		[[ $first == 1 ]] || printf ',' >>"$tmp/mq-runs.json"
		first=0
		tree=$(jq -r '.tree.sha // ""' "$tmp/commit-$s.json" 2>/dev/null || true)
		printf '{"id":1,"head_sha":"%s","event":"merge_group","conclusion":"success","head_commit":{"id":"%s","tree_id":"%s"}}' "$s" "$s" "$tree" >>"$tmp/mq-runs.json"
	done
	printf ']}' >>"$tmp/mq-runs.json"
}
checks() { # checks NAME:CONCLUSION ... -> checks.json (the queue head's check runs)
	local first=1 n c
	printf '{"check_runs":[' >"$tmp/checks.json"
	for spec in "$@"; do
		IFS=: read -r n c <<<"$spec"
		[[ $first == 1 ]] || printf ',' >>"$tmp/checks.json"
		first=0
		printf '{"name":"%s","id":1,"status":"completed","conclusion":"%s","app":{"slug":"github-actions"}}' "$n" "$c" >>"$tmp/checks.json"
	done
	printf ']}' >>"$tmp/checks.json"
}
qh_same=1111111111111111111111111111111111111111 # a queue commit with merge_same's tree
qh_other=2222222222222222222222222222222222222222 # a queue commit with another tree
commit_json "$qh_same" "$(tree_of "$merge_same")"
commit_json "$qh_other" "$(tree_of "$merge_diff")"
checks flavors:success

mq_runs "$qh_same"
# shellcheck disable=SC2046 # push prints separate KEY=VALUE words
expect "a successful merge_group run on the pushed tree: skip" true "merge_group" $(push "$merge_same")
# shellcheck disable=SC2046
expect "a queue run on another tree does not count" false "no successful merge_group" $(push "$merge_diff")
mq_runs "$qh_other" "$qh_same"
# shellcheck disable=SC2046
expect "an older queue run on the same tree counts" true "merge_group" $(push "$merge_same")
mq_runs
# shellcheck disable=SC2046
expect "no queue run (an admin merge) runs everything" false "no successful merge_group" $(push "$merge_same")
# A draft-era or ready pull request head that is green never skips a push: its check runs are not read.
checks go:success "quick checks:success" "generated code:success" flavors:success
# shellcheck disable=SC2046
expect "a green pull request head without a queue run runs everything" false "no successful merge_group" $(push "$merge_same")
# A queue run whose head commit cannot be read is not trusted.
mq_runs 3333333333333333333333333333333333333333
# shellcheck disable=SC2046
expect "an unreadable queue commit runs everything" false "no successful merge_group" $(push "$merge_same")
mq_runs "$qh_same"
# shellcheck disable=SC2046
expect "an API error on the runs list runs everything" false "cannot list" $(push "$merge_same") FAKE_FAIL_RUNS=1
# shellcheck disable=SC2046
expect "an unknown commit runs everything" false "cannot read" $(push 0000000000000000000000000000000000000000)
expect "a tag push runs everything" false "not a push to main" EVENT_NAME=push REF=refs/tags/v1 SHA="$merge_same"
expect "no repo runs everything" false "not a push to main" EVENT_NAME=push REF=refs/heads/main SHA="$merge_same" REPO=
# flavors is advisory but flavor-publish ships what it tested: judged on the queue head.
checks flavors:failure
# shellcheck disable=SC2046
expect "flavors red on the queue head, nothing it builds from changed: skip" true "merge_group" $(push "$merge_same")
commit_json "$qh_same" "$(tree_of "$merge_flav")"
# shellcheck disable=SC2046
expect "flavors red and its files changed runs everything" false "flavors did not pass" $(push "$merge_flav")
checks flavors:success
# shellcheck disable=SC2046
expect "flavors green and its files changed: skip" true "merge_group" $(push "$merge_flav")
checks
# shellcheck disable=SC2046
expect "flavors never ran and its files changed runs everything" false "no completed run" $(push "$merge_flav")
# A one-parent commit (squash or rebase landing) is judged the same way, by tree.
commit_json "$qh_same" "$(tree_of "$plain")"
checks flavors:success
# shellcheck disable=SC2046
expect "a one-parent commit with a queue run on its tree: skip" true "merge_group" $(push "$plain")
: "$feat_head"

statuses() { # statuses CONTEXT:STATE ... (newest first) -> statuses.json
	local first=1 c st
	printf '[' >"$tmp/statuses.json"
	for spec in "$@"; do
		IFS=: read -r c st <<<"$spec"
		[[ $first == 1 ]] || printf ',' >>"$tmp/statuses.json"
		first=0
		printf '{"context":"%s","state":"%s"}' "$c" "$st" >>"$tmp/statuses.json"
	done
	printf ']' >>"$tmp/statuses.json"
}

# --- lab (old issue 610) -----------------------------------------------------------------------------------
cd "$tmp"
head_sha=0123456789abcdef0123456789abcdef01234567
expect_lab() { # expect_lab NAME WANT [env...]: runs `lab`
	local name=$1 want=$2 out
	shift 2
	out=$(env "$@" "$script" lab 2>&1) || true
	if grep -qx "go_test_lab=$want" <<<"$out" && grep -q '^go_test_lab_reason=' <<<"$out"; then
		echo "ok: $name"
	else
		echo "FAIL: $name (want go_test_lab=$want)"
		printf '    %s\n' "${out//$'\n'/$'\n    '}"
		fails=1
	fi
}
statuses go-test-lab:success
expect_lab "lab: a green go-test-lab on the head satisfies go-test" true EVENT_NAME=pull_request HEAD_SHA=$head_sha SKIP=false
statuses go-test-lab:failure
expect_lab "lab: a red go-test-lab does not" false EVENT_NAME=pull_request HEAD_SHA=$head_sha SKIP=false
statuses
expect_lab "lab: no status does not" false EVENT_NAME=pull_request HEAD_SHA=$head_sha SKIP=false
statuses e2e-lab:success
expect_lab "lab: e2e-lab is another context" false EVENT_NAME=pull_request HEAD_SHA=$head_sha SKIP=false
statuses go-test-lab:success
expect_lab "lab: a draft (skip=true) never counts" false EVENT_NAME=pull_request HEAD_SHA=$head_sha SKIP=true
expect_lab "lab: a push never counts" false EVENT_NAME=push HEAD_SHA=$head_sha SKIP=false
expect_lab "lab: no head sha does not" false EVENT_NAME=pull_request HEAD_SHA= SKIP=false
expect_lab "lab: an API error means go-test runs" false EVENT_NAME=pull_request HEAD_SHA=$head_sha SKIP=false FAKE_FAIL_STATUSES=1

# --- apply -----------------------------------------------------------------------------------------
cd "$tmp"
input=$'go=true\nweb=true\ngenerate=true\nchart=false\nchartsmoke=true\ndocs=true\nescape=true\nflavors=true'
want_skip=$'go=false\nweb=false\ngenerate=false\nchart=false\nchartsmoke=false\ndocs=false\nescape=false\nflavors=false\nflavors_publish=true'
want_keep=$'go=true\nweb=true\ngenerate=true\nchart=false\nchartsmoke=true\ndocs=true\nescape=true\nflavors=true\nflavors_publish=true'
check_apply() { # check_apply NAME WANT SKIP INPUT
	local got
	got=$(SKIP=$3 "$script" apply <<<"$4")
	if [[ $got == "$2" ]]; then echo "ok: $1"; else echo "FAIL: $1"; diff <(echo "$2") <(echo "$got") | sed 's/^/    /'; fails=1; fi
}
check_apply "apply with skip=true switches every heavy output off" "$want_skip" true "$input"
check_apply "apply with skip=false changes nothing" "$want_keep" false "$input"
check_apply "apply with skip unset changes nothing" "$want_keep" "" "$input"
check_apply "flavors_publish keeps a false flavors" $'go=false\nflavors=false\nflavors_publish=false' true $'go=true\nflavors=false'
# an unknown name passes through untouched
check_apply "unknown outputs pass through" $'extra=true\nflavors_publish=true' true $'extra=true'

# --- ci.yml wiring (old issue 594) --------------------------------------------------------------------------
# `scripts` and `lint-shell` are skipped on a push to main whose heavy jobs are switched off (old issue 595):
# their job-level `if:` carries the condition. `quick-checks` never waits for `changes`, never skips,
# and every step after its first check (buf lint) runs even when an earlier one failed.
ci_yml=$(cd "$(dirname "$script")/.." && pwd)/.github/workflows/ci.yml
job_block() { awk -v j="$1" '$0 == "  " j ":" { on = 1; next } on && /^  [a-z0-9-]+:/ { exit } on' "$ci_yml"; }
for j in scripts lint-shell; do
	b=$(job_block "$j")
	cond=$(grep -E '^    if:' <<<"$b" || true)
	if grep -q 'needs: changes' <<<"$b" && grep -q "github.event_name == 'push' && needs.changes.outputs.skip == 'true'" <<<"$cond" && grep -q '!cancelled()' <<<"$cond"; then
		echo "ok: ci.yml skips $j on a push to main with skip=true"
	else
		echo "FAIL: the if: of ci.yml job $j lacks the push/skip/!cancelled() condition (or needs: changes)"
		fails=1
	fi
done
b=$(job_block quick-checks)
if [[ -n $b ]] && ! grep -qE '^    (needs|if):|outputs\.skip' <<<"$b"; then
	echo "ok: ci.yml quick-checks always runs, without waiting for changes"
else
	echo "FAIL: ci.yml quick-checks is missing, has needs:/if: or depends on the skip decision"
	fails=1
fi
# Steps after "proto (buf lint)": each step is a '      - ' item; its text must hold !cancelled().
bad=$(awk '/^      - /{ if (n && after && !ok) print name; n = 1; ok = 0; name = $0 }
	/!cancelled\(\)/{ ok = 1 }
	/name: proto \(buf lint\)/{ seen = 1; next }
	/^      - /{ after = seen }
	END { if (n && after && !ok) print name }' <<<"$b" || true)
if [[ -z $bad ]] && grep -q 'name: proto (buf lint)' <<<"$b"; then
	echo "ok: every quick-checks step after buf lint has !cancelled()"
else
	echo "FAIL: quick-checks steps without !cancelled(): ${bad:-(buf lint step missing)}"
	fails=1
fi

((fails == 0)) || exit 1
echo "all ci-skip-heavy tests passed"
