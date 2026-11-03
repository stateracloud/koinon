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
repos/o/r/commits/*/check-runs*) f=$FAKE_DIR/checks.json ;;
*) echo "fake gh: unknown $path" >&2; exit 1 ;;
esac
jq -r "$jq_expr" "$f"
EOF
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp"

# --- required list ---------------------------------------------------------------------------------
cat >"$tmp/required.txt" <<'EOF'
# comment
aggregator go
quick checks
generated code   # trailing comment
external dco
advisory flavors
EOF
export CI_REQUIRED="$tmp/required.txt" REPO=o/r

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

checks() { # checks NAME:CONCLUSION[:ID] ... -> checks.json (status completed unless CONCLUSION is "running")
	local first=1 n c i
	printf '{"check_runs":[' >"$tmp/checks.json"
	for spec in "$@"; do
		IFS=: read -r n c i <<<"$spec"
		[[ $first == 1 ]] || printf ',' >>"$tmp/checks.json"
		first=0
		if [[ $c == running ]]; then
			printf '{"name":"%s","id":%s,"status":"in_progress","conclusion":null,"app":{"slug":"github-actions"}}' "$n" "${i:-1}" >>"$tmp/checks.json"
		elif [[ $c == other-app ]]; then
			printf '{"name":"%s","id":%s,"status":"completed","conclusion":"success","app":{"slug":"some-bot"}}' "$n" "${i:-1}" >>"$tmp/checks.json"
		else
			printf '{"name":"%s","id":%s,"status":"completed","conclusion":"%s","app":{"slug":"github-actions"}}' "$n" "${i:-1}" "$c" >>"$tmp/checks.json"
		fi
	done
	printf ']}' >>"$tmp/checks.json"
}

# --- pull requests ---------------------------------------------------------------------------------
echo '{"draft":true}' >"$tmp/pull.json"
expect "draft PR skips" true "draft" EVENT_NAME=pull_request PR_NUMBER=7
echo '{"draft":false}' >"$tmp/pull.json"
expect "ready PR runs everything" false "ready" EVENT_NAME=pull_request PR_NUMBER=7
expect "API error runs everything" false "could not read" EVENT_NAME=pull_request PR_NUMBER=7 FAKE_FAIL=1
echo '{"draft":"maybe"}' >"$tmp/pull.json"
expect "odd draft value runs everything" false "unexpected" EVENT_NAME=pull_request PR_NUMBER=7
expect "no PR number runs everything" false "no pull request" EVENT_NAME=pull_request PR_NUMBER=
expect "other event runs everything" false "event" EVENT_NAME=schedule

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
# a merge whose tree equals the head's but whose head does not contain the first parent
base=$(g rev-parse HEAD)
g checkout -qb feat4 "$base"; echo e >"$repo/e"; g add e; g commit -qm feat4
g checkout -q main; echo e >"$repo/e"; g add e; g commit -qm "main adds the same e"
g merge -q --no-ff feat4 -m "Merge feat4"
merge_noanc=$(g rev-parse HEAD)
# a merge that brings a file the flavors suite builds from
g checkout -qb feat3; mkdir -p "$repo/flavors"; echo f >"$repo/flavors/x"; g add flavors; g commit -qm feat3
g checkout -q main; g merge -q --no-ff feat3 -m "Merge feat3"
merge_flav=$(g rev-parse HEAD)
export CI_CHANGES_FLAVORS_DIRS=test/flavors # the directories the flavors suite builds (no go list in the scratch repo)

all_green=(go:success "quick checks:success" "generated code:skipped" dco:failure)
checks "${all_green[@]}"
cd "$repo"
push() { echo "EVENT_NAME=push REF=refs/heads/main SHA=$1"; }
# shellcheck disable=SC2046 # push prints separate KEY=VALUE words
expect "merge tree equals a green head: skip" true "equals the tree" $(push "$merge_same")
# dco is external and failing above: it must not matter. Now break a required one.
checks go:success "quick checks:failure" "generated code:success"
# shellcheck disable=SC2046
expect "a red required check runs everything" false "quick checks" $(push "$merge_same")
checks go:success "generated code:success"
# shellcheck disable=SC2046
expect "a missing required check runs everything" false "no run" $(push "$merge_same")
checks go:success "quick checks:running" "generated code:success"
# shellcheck disable=SC2046
expect "a running required check runs everything" false "not completed" $(push "$merge_same")
checks go:success "quick checks:other"-app "generated code:success"
# shellcheck disable=SC2046
expect "a check from another app does not count" false "no run" $(push "$merge_same")
checks go:success "quick checks:failure:1" "quick checks:success:2" "generated code:success"
# shellcheck disable=SC2046
expect "a newer green run replaces an older red one" true "equals the tree" $(push "$merge_same")
checks go:success "quick checks:success:1" "quick checks:cancelled:2" "generated code:success"
# shellcheck disable=SC2046
expect "a newer red run replaces an older green one" false "quick checks" $(push "$merge_same")
checks "${all_green[@]}"
# shellcheck disable=SC2046
expect "a head that does not contain the first parent runs everything" false "not an ancestor" $(push "$merge_noanc")
# flavors is advisory: failed on the head with nothing under its paths changed, the skip stands
checks "${all_green[@]}" flavors:failure
# shellcheck disable=SC2046
expect "flavors red but not affected: skip" true "equals the tree" $(push "$merge_same")
# shellcheck disable=SC2046
expect "flavors red and its files changed runs everything" false "flavors did not pass" $(push "$merge_flav")
checks "${all_green[@]}" flavors:cancelled
# shellcheck disable=SC2046
expect "flavors cancelled and its files changed runs everything" false "flavors did not pass" $(push "$merge_flav")
checks "${all_green[@]}"
# shellcheck disable=SC2046
expect "flavors never ran and its files changed runs everything" false "no completed run" $(push "$merge_flav")
checks "${all_green[@]}" flavors:success
# shellcheck disable=SC2046
expect "flavors green and its files changed: skip" true "equals the tree" $(push "$merge_flav")
# a draft run leaves a failed go and web (by design); the ready run is still going, or was cancelled
checks "quick checks:success" "generated code:success" go:success:1 go:failure:2 flavors:success
# shellcheck disable=SC2046
expect "the newest go is a draft run's failure: run everything" false "'go'" $(push "$merge_same")
checks "quick checks:success" "generated code:success" go:failure:1 go:running:2 flavors:success
# shellcheck disable=SC2046
expect "ready run still going after a draft failure: run everything" false "not completed" $(push "$merge_same")
checks "quick checks:success" "generated code:success" go:failure:1 go:cancelled:2 flavors:success
# shellcheck disable=SC2046
expect "ready run cancelled: run everything" false "'go'" $(push "$merge_same")
checks "quick checks:success" "generated code:success" go:success:1 go:failure:2 go:success:3 flavors:success
# shellcheck disable=SC2046
expect "ready, draft, push, ready again: the newest green go counts" true "equals the tree" $(push "$merge_same")
checks "${all_green[@]}"
# shellcheck disable=SC2046
expect "a different tree runs everything" false "differs" $(push "$merge_diff")
# shellcheck disable=SC2046
expect "a one-parent commit runs everything" false "not a merge commit" $(push "$plain")
# shellcheck disable=SC2046
expect "an unknown commit runs everything" false "cannot read" $(push 0000000000000000000000000000000000000000)
# shellcheck disable=SC2046
expect "an API error runs everything" false "cannot read the check runs" $(push "$merge_same") FAKE_FAIL=1
expect "a tag push runs everything" false "not a push to main" EVENT_NAME=push REF=refs/tags/v1 SHA="$merge_same"
# shellcheck disable=SC2046
expect "an unreadable required list runs everything" false "required checks" $(push "$merge_same") CI_REQUIRED="$tmp/missing.txt"
: "$feat_head"

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

# --- ci.yml wiring (#594) --------------------------------------------------------------------------
# `scripts` and `lint-shell` are skipped on a push to main whose heavy jobs are switched off (#595):
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
