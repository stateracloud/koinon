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
repos/o/r/commits/*/pulls*) [[ ${FAKE_FAIL_PULLS:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }; f=$FAKE_DIR/pulls.json ;;
repos/o/r/pulls/*/files*) f=$FAKE_DIR/files.json ;;
*) echo "fake gh: unknown $path" >&2; exit 1 ;;
esac
jq -r "$jq_expr" "$f"
EOF2
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp" REPO=o/r SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

fails=0
reset() {
	rm -f "$tmp"/calls "$tmp"/open-issue
	unset FAKE_FAIL FAKE_FAIL_RUNS FAKE_FAIL_PULLS FAKE_LABEL_FAILS FAKE_ISSUE_FAILS
	: >"$tmp/calls"
	echo '{"tree":{"sha":"tree1"}}' >"$tmp/commit.json"
	echo '{"workflow_runs":[]}' >"$tmp/mq-runs.json"
	echo '[{"number":42}]' >"$tmp/pulls.json"
	echo '[{"filename":"hack/x.sh"}]' >"$tmp/files.json"
}
mq_run_on() { # <tree>: a successful merge_group ci run on that tree
	jq -n --arg t "$1" '{workflow_runs:[{conclusion:"success", head_sha:"bbbb", head_commit:{tree_id:$t}}]}' >"$tmp/mq-runs.json"
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

# 1. a queue merge: a successful merge_group ci run on the pushed tree. No issue.
reset
mq_run_on tree1
run
exit0 "queue merge"
hasnt "queue merge: no issue" 'issue create\|issue comment'
hasnt "queue merge: no pull request lookup" 'commits/.*/pulls'

# 2. a .github/** admin merge: no queue run, the merged PR changed a .github/ path
reset
echo '[{"filename":"hack/x.sh"},{"filename":".github/workflows/ci.yml"}]' >"$tmp/files.json"
run
exit0 "github admin merge"
has "github admin merge: issue created" 'issue create'
has "github admin merge: labelled admin:github" '--label admin:github'
has "github admin merge: keyed by the sha" 'aaaaaaaaaaaa'

# 3. any other admin merge: break-glass
reset
mq_run_on othertree
run
exit0 "other admin merge"
has "other admin merge: issue created" 'issue create'
has "other admin merge: labelled break-glass" '--label break-glass'
hasnt "other admin merge: not labelled admin:github" 'admin:github'

# 4. a path that only contains .github/ is not under .github/
reset
echo '[{"filename":"docs/.github/x.md"}]' >"$tmp/files.json"
run
has "nested .github is break-glass" '--label break-glass'

# 5. an API error (the runs cannot be listed): an issue "could not verify", exit 0
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
