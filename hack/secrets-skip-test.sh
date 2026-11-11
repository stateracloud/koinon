#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/secrets-skip.sh: a fake `gh` answers from fixture files (jq applies the
# script's --jq expression, as gh does).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

script=$(cd "$(dirname "$0")" && pwd)/secrets-skip.sh
command -v jq >/dev/null || { echo "SKIPPED: jq is missing"; exit 0; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/secrets-skip-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
# fake gh api <path> [--jq EXPR]; fixtures in $FAKE_DIR; FAKE_FAIL=1 fails every call.
[[ ${FAKE_FAIL:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }
path="" jq_expr="."
shift # api
while (($#)); do
	case $1 in
	--jq) jq_expr=$2; shift ;;
	*) path=$1 ;;
	esac
	shift
done
case $path in
repos/o/r/actions/workflows/secrets.yml/runs*head_sha=abc123*per_page=1) [[ ${FAKE_FAIL_RUNS:-0} == 0 ]] || { echo "gh: HTTP 500" >&2; exit 1; }; f=$FAKE_DIR/runs.json ;;
*) echo "fake gh: unknown $path" >&2; exit 1 ;;
esac
jq -r "$jq_expr" "$f"
EOF
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH" FAKE_DIR="$tmp" REPO=o/r

# one queue run on the pushed commit, one on another commit, one that failed on the pushed commit;
# runs_sametree: a successful queue run with the pushed tree but another head_sha (other commits)
runs_match='{"workflow_runs":[{"head_sha":"q1","conclusion":"success","head_commit":{"tree_id":"tree-other"}},{"head_sha":"abc123","conclusion":"success","head_commit":{"tree_id":"tree-main"}}]}'
runs_none='{"workflow_runs":[{"head_sha":"q1","conclusion":"success","head_commit":{"tree_id":"tree-other"}}]}'
runs_failed='{"workflow_runs":[{"head_sha":"abc123","conclusion":"failure","head_commit":{"tree_id":"tree-main"}}]}'
runs_sametree='{"workflow_runs":[{"head_sha":"q9","conclusion":"success","head_commit":{"tree_id":"tree-main"}}]}'

fails=0
# expect NAME WANT_SKIP REASON_SUBSTRING RUNS_JSON [env...]
expect() {
	local name=$1 want=$2 sub=$3 runs=$4 out
	shift 4
	printf '%s' "$runs" >"$tmp/runs.json"
	out=$(env "$@" "$script" decide 2>&1) || true
	if grep -qx "skip=$want" <<<"$out" && grep -q -- "reason=.*$sub" <<<"$out"; then
		echo "ok: $name"
	else
		echo "FAIL: $name (want skip=$want, reason containing '$sub')"
		printf '    %s\n' "${out//$'\n'/$'\n    '}"
		fails=1
	fi
}

push=(EVENT_NAME=push REF=refs/heads/main SHA=abc123)
expect "queue-made push to main is skipped" true "scanned commit abc123" "$runs_match" "${push[@]}"
expect "admin or direct push (no queue run on the commit) scans" false "no successful merge_group" "$runs_none" "${push[@]}"
expect "a queue run with the same tree but another head_sha scans" false "no successful merge_group" "$runs_sametree" "${push[@]}"
expect "a failed queue run does not count" false "no successful merge_group" "$runs_failed" "${push[@]}"
expect "pull_request scans" false "event 'pull_request'" "$runs_match" EVENT_NAME=pull_request REF=refs/pull/1/merge SHA=abc123
expect "merge_group scans" false "event 'merge_group'" "$runs_match" EVENT_NAME=merge_group REF=refs/heads/gh-readonly-queue/main/x SHA=abc123
expect "push to another branch scans" false "not a push to main" "$runs_match" EVENT_NAME=push REF=refs/heads/other SHA=abc123
expect "no event scans" false "event ''" "$runs_match" SHA=abc123
expect "gh failing scans" false "cannot list the merge_group runs" "$runs_match" "${push[@]}" FAKE_FAIL=1
expect "gh failing on the runs scans" false "cannot list the merge_group runs" "$runs_match" "${push[@]}" FAKE_FAIL_RUNS=1

# the workflow wires the decision in and keeps the required check reporting
yml=$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/secrets.yml
if grep -q 'hack/secrets-skip.sh decide' "$yml" && grep -q "steps.skip.outputs.skip != 'true'" "$yml"; then
	echo "ok: secrets.yml runs the decision and gates the scan on it"
else
	echo "FAIL: secrets.yml does not run hack/secrets-skip.sh or does not gate its scan steps on it"
	fails=1
fi
# the decision is possible only on a push, by a condition in the workflow (the script is outside .github/)
if awk '/^      - id: skip$/{f=1;next} f&&/^      - /{exit} f&&/^        if: github.event_name == .push.$/{ok=1} END{exit !ok}' "$yml"; then
	echo "ok: the skip step runs only on push"
else
	echo "FAIL: the skip step of secrets.yml has no 'if: github.event_name == push' condition"
	fails=1
fi
# every scan step is gated by the same plain condition, so an empty skip output scans
if [[ $(grep -c "if: steps.skip.outputs.skip != 'true'" "$yml") -eq 4 ]] && ! grep -q "skip == 'true'" "$yml"; then
	echo "ok: the four scan steps scan unless skip is exactly true"
else
	echo "FAIL: secrets.yml scan steps are not all gated by steps.skip.outputs.skip != 'true'"
	fails=1
fi

exit "$fails"
