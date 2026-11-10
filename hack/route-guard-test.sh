#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/route-guard.sh (ADR 0105): `gh` is a stub that answers from fixture files (jq applies the
# script's --jq expression, as gh does) and records every call. The script must always exit 0.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

script=$(cd "$(dirname "$0")" && pwd)/route-guard.sh
command -v jq >/dev/null || { echo "SKIPPED: jq is missing"; exit 0; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/route-guard-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF2'
#!/usr/bin/env bash
# fake gh: api <path> [--paginate] [--jq EXPR] | issue list/create/comment. Fixtures in $FAKE_DIR.
echo "gh $*" >>"$FAKE_DIR/calls"
if [[ $1 == issue ]]; then
	case $2 in
	list) [[ -f $FAKE_DIR/open-issue ]] && cat "$FAKE_DIR/open-issue" || true ;;
	create) [[ -z ${FAKE_LABEL_FAILS:-} || $* != *--label* ]] || exit 1; [[ -z ${FAKE_ISSUE_FAILS:-} ]] || exit 1 ;;
	comment) ;;
	esac
	exit 0
fi
[[ ${FAKE_FAIL:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }
path="" jq_expr="."
shift
while (($#)); do
	case $1 in
	--paginate) ;;
	--jq) jq_expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
case $path in
repos/o/r/git/commits/*) f=$FAKE_DIR/commit.json ;;
repos/o/r/actions/workflows/ci.yml/runs*) [[ ${FAKE_FAIL_RUNS:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }; f=$FAKE_DIR/mq-runs.json ;;
repos/o/r/actions/runs/*/jobs*)
	n=$(($(cat "$FAKE_DIR/jobs-n" 2>/dev/null || echo 0) + 1)); echo "$n" >"$FAKE_DIR/jobs-n"
	f=$FAKE_DIR/jobs.json
	[[ $n -lt 3 || ! -f $FAKE_DIR/jobs-later.json ]] || f=$FAKE_DIR/jobs-later.json ;;
repos/o/r/commits/*/pulls*) [[ ${FAKE_FAIL_PULLS:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }; f=$FAKE_DIR/pulls.json ;;
repos/o/r/commits/*/check-runs*) f=$FAKE_DIR/checks.json ;;
repos/o/r/pulls/*/files*) f=$FAKE_DIR/files.json ;;
repos/o/r/pulls/*) f=$FAKE_DIR/pull.json ;;
*) echo "fake gh: unknown $path" >&2; exit 1 ;;
esac
jq -r "$jq_expr" "$f"
EOF2
chmod +x "$tmp/bin/gh"
printf '# c\naggregator go\nquick checks\nchart\nexternal dco\nexternal e2e result\nadvisory flavors\n' >"$tmp/required.txt"
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp" REPO=o/r SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa REQUIRED="$tmp/required.txt" POLL_SLEEP=0 POLL_MAX=0

fails=0
job() { printf '{"name":"%s","status":"%s","conclusion":%s}' "$1" "${3:-completed}" "$([[ ${2:-success} == null ]] && echo null || echo "\"${2:-success}\"")"; }
jobs_file() { local IFS=,; printf '{"jobs":[%s]}' "$*" >"$tmp/${F:-jobs.json}"; }
all_green_jobs() { jobs_file "$(job go)" "$(job 'quick checks')" "$(job chart skipped)" "$(job flavors failure)"; }
chk() { printf '{"id":%s,"name":"%s","status":"completed","conclusion":"%s","app":{"id":%s}}' "$1" "$2" "$3" "${4:-15368}"; }
checks_file() { local IFS=,; printf '{"check_runs":[%s]}' "$*" >"$tmp/checks.json"; }
green_checks() { checks_file "$(chk 1 go success)" "$(chk 2 'quick checks' success)" "$(chk 3 chart skipped)" "$(chk 4 dco success)" "$(chk 5 'e2e result' success)" "$(chk 6 review success)"; }
reset() {
	rm -f "$tmp"/calls "$tmp"/open-issue "$tmp"/jobs-later.json "$tmp"/jobs-n
	unset FAKE_FAIL FAKE_FAIL_RUNS FAKE_FAIL_PULLS FAKE_LABEL_FAILS FAKE_ISSUE_FAILS
	: >"$tmp/calls"
	echo '{"tree":{"sha":"tree1"}}' >"$tmp/commit.json"
	echo '{"workflow_runs":[]}' >"$tmp/mq-runs.json"
	echo '[{"number":42}]' >"$tmp/pulls.json"
	echo '{"head":{"sha":"headsha"}}' >"$tmp/pull.json"
	echo '[{"filename":"hack/x.sh"}]' >"$tmp/files.json"
	all_green_jobs
	green_checks
}
mq_run_on() { # <tree> [status]: a merge_group ci run on that tree
	jq -n --arg t "$1" --arg s "${2:-completed}" '{workflow_runs:[{id:9, status:$s, head_sha:"bbbb", head_commit:{tree_id:$t}}]}' >"$tmp/mq-runs.json"
}
run() { # sets rc; the output is in $tmp/out
	rc=0
	"$script" >"$tmp/out" 2>&1 || rc=$?
}
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fails=1; }
has() { if grep -q -- "$2" "$tmp/calls"; then ok "$1"; else bad "$1 (no /$2/ in calls)"; fi; }
hasnt() { if grep -q -- "$2" "$tmp/calls"; then bad "$1 (found /$2/)"; else ok "$1"; fi; }
exit0() { if [[ $rc == 0 ]]; then ok "$1: exit 0"; else bad "$1: exit $rc"; fi; }

# 1. a queue merge: the required jobs passed on a merge_group run of the pushed tree (flavors red, advisory). No issue.
reset
mq_run_on tree1
run
exit0 "queue merge, flavors red"
hasnt "queue merge, flavors red: no issue" 'issue create\|issue comment'
hasnt "queue merge: no pull request lookup" 'commits/.*/pulls'

# 1b. the run is still in progress but the required jobs are done (the queue merged on them): no issue
reset
mq_run_on tree1 in_progress
run
exit0 "run in progress, required jobs green"
hasnt "run in progress, required jobs green: no issue" 'issue create'

# 1c. required jobs still running, then green after a poll: no issue
reset
mq_run_on tree1 in_progress
jobs_file "$(job go null in_progress)" "$(job 'quick checks')" "$(job chart skipped)"
F=jobs-later.json all_green_jobs
POLL_MAX=60 POLL_SLEEP=1 run
exit0 "pending then green"
hasnt "pending then green: no issue" 'issue create'
if [[ $(grep -c 'actions/runs/9/jobs' "$tmp/calls") -ge 3 ]]; then ok "pending then green: it polled"; else bad "pending then green: it did not poll"; fi

# 1d. required jobs never finish within the poll window: treated as not queue-built
reset
mq_run_on tree1 in_progress
jobs_file "$(job go null in_progress)" "$(job 'quick checks')" "$(job chart skipped)"
run
exit0 "pending forever"
has "pending forever: issue" 'issue create'

# 1e. a required job red: issue
reset
mq_run_on tree1
jobs_file "$(job go failure)" "$(job 'quick checks')" "$(job chart skipped)"
run
exit0 "required job red"
has "required job red: issue" 'issue create'
has "required job red: break-glass" '--label break-glass'

# 1f. a required job missing from the run: not proof (pending, then none)
reset
mq_run_on tree1
jobs_file "$(job go)" "$(job 'quick checks')"
run
has "required job missing: issue" 'issue create'

# 2. a .github/** admin merge: no queue run; the PR changes .github/ and every required check and review were green: NO issue
reset
echo '[{"filename":"hack/x.sh"},{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
run
exit0 "github admin merge, green"
hasnt "github admin merge, green: no issue" 'issue create\|issue comment'

# 2b. the same, but review was not green on the head: issue, labelled break-glass (not admin:github)
reset
echo '[{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
checks_file "$(chk 1 go success)" "$(chk 2 'quick checks' success)" "$(chk 3 chart skipped)" "$(chk 4 dco success)" "$(chk 5 'e2e result' success)" "$(chk 6 review failure)"
run
exit0 "github admin merge, review red"
has "github admin merge, review red: issue" 'issue create'
has "github admin merge, review red: break-glass" '--label break-glass'
hasnt "never admin:github" 'admin:github'
has "names the check" "check 'review' was not green"

# 2c. a required check missing on the head
reset
echo '[{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
checks_file "$(chk 1 go success)" "$(chk 6 review success)"
run
has "github admin merge, checks missing: issue" 'issue create'

# 2d. a newer red re-run of a check outvotes an older green one
reset
echo '[{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
checks_file "$(chk 1 go success)" "$(chk 2 'quick checks' success)" "$(chk 3 chart skipped)" "$(chk 4 dco success)" "$(chk 5 'e2e result' success)" "$(chk 6 review success)" "$(chk 7 go failure)"
run
has "newest row per check wins: issue" 'issue create'

# 2e. a same-named green check from another app does not count (review red from Actions, green from elsewhere)
reset
echo '[{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
checks_file "$(chk 1 go success)" "$(chk 2 'quick checks' success)" "$(chk 3 chart skipped)" "$(chk 4 dco success)" "$(chk 5 'e2e result' success)" "$(chk 6 review failure)" "$(chk 7 review success 999)"
run
has "other-app green review does not count: issue" 'issue create'
reset
echo '[{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
checks_file "$(chk 1 go success 999)" "$(chk 2 'quick checks' success)" "$(chk 3 chart skipped)" "$(chk 4 dco success)" "$(chk 5 'e2e result' success)" "$(chk 6 review success)"
run
has "other-app only go does not count: issue" 'issue create'

# 3. any other admin merge: break-glass
reset
mq_run_on othertree
run
exit0 "other admin merge"
has "other admin merge: issue created" 'issue create'
has "other admin merge: labelled break-glass" '--label break-glass'

# 4. a path that only contains .github/ is not under .github/
reset
echo '[{"filename":"docs/.github/x.md"}]' >"$tmp/files.json"
run
has "nested .github is break-glass" '--label break-glass'

# 4b. a direct push (no pull request): a real hit, said so
reset
echo '[]' >"$tmp/pulls.json"
run
exit0 "direct push"
has "direct push: issue" 'issue create'
has "direct push: title says direct push" 'reached main by a direct push'
has "direct push: a real hit" 'a real hit, not a false alarm'
hasnt "direct push: not called a false alarm" 'may be a false alarm'

# 5. an API error: an issue "could not verify", exit 0
reset
FAKE_FAIL_RUNS=1 run
exit0 "runs unreadable"
has "runs unreadable: issue created" 'issue create'
has "runs unreadable: says could not verify" 'could not verify'
reset
FAKE_FAIL=1 run
exit0 "every call fails"
has "every call fails: an issue is still attempted" 'issue create'
reset
FAKE_FAIL_PULLS=1 run
exit0 "pull request unreadable"
has "pull request unreadable: break-glass (the safe class)" '--label break-glass'
has "pull request unreadable: says could not verify" 'could not verify'

# 6. the same sha twice: a comment on the open issue, one issue
reset
echo 77 >"$tmp/open-issue"
run
exit0 "same sha twice"
has "same sha twice: comments on the open issue" 'issue comment 77'
hasnt "same sha twice: no second issue" 'issue create'

# 7. a label that does not exist yet: the issue is still opened, unlabelled
reset
FAKE_LABEL_FAILS=1 run
exit0 "missing label"
if [[ $(grep -c '^gh issue create' "$tmp/calls") == 2 ]]; then ok "missing label: retried without the label"; else bad "missing label: retry"; fi

# 8. even the issue cannot be opened: still exit 0
reset
FAKE_ISSUE_FAILS=1 run
exit0 "issue creation fails"

# 9. no SHA: a usage problem, still exit 0 (the job never goes red)
rc=0
env -u SHA "$script" >/dev/null 2>&1 || rc=$?
exit0 "no SHA"

[[ $fails == 0 ]] && echo "route-guard tests passed"
exit "$fails"
