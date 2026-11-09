#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The merge queue (#225) builds a commit that belongs to no pull request and waits for every
# required check on it. A workflow that produces a required context but does not listen to
# `merge_group` leaves that check absent, and the queue entry waits until it times out. This test
# keeps the workflows that produce a required context (hack/ci-required.txt) subscribed, and keeps
# the jobs that read pull request data from running on a queue build.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wfdir=${1:-$repo/.github/workflows}

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }
# The workflows that produce a required context: ci.yml (the jobs), and the external ones named in the
# header of hack/ci-required.txt. That they listen to merge_group at all is checked by hack/ci-required-test.sh;
# here the trigger must also ask for checks_requested only.
while IFS= read -r wf; do
	# shellcheck disable=SC2016
	check "$wf asks for checks_requested only" awk '/^  merge_group:/ { getline; if ($0 ~ /types: \[checks_requested\]/) ok = 1 } END { exit !ok }' "$wfdir/$wf"
done < <({ echo ci.yml; sed -n -E 's@^#[[:space:]]+[^[:space:]].*[[:space:]]\.github/workflows/([A-Za-z0-9_.-]+\.yml)[[:space:]]*$@\1@p' "$repo/hack/ci-required.txt"; } | sort -u)
# The jobs that are not part of a queue build stay off it: they read a pull request or push to main.
# shellcheck disable=SC2016
check "claims.yml: release and sweeper do not run on merge_group" bash -c '
	! awk "/^  (release|sweeper):/ { j = 1 } /^  claim:/ { j = 0 } j && /merge_group/ { bad = 1 } END { exit !bad }" "$1"' _ "$wfdir/claims.yml"
check "claims.yml: the claim job reports on merge_group" grep -Eq "if: github.event_name == 'merge_group' \|\|" "$wfdir/claims.yml"
# shellcheck disable=SC2016
check "claims.yml: on merge_group the claim job runs check-pr for the queued PR from the base sha" bash -c '
	grep -q "ref: \${{ github.event.merge_group.base_sha }}" "$1" && grep -q "gh-readonly-queue/\*/pr-" "$1" &&
	awk "/event_name == .merge_group.$/ { g = 1 } g && /claim.sh check-pr/ { ok = 1 } END { exit !ok }" "$1"' _ "$wfdir/claims.yml"
# #444: the queue commit runs the quickstart itself (decide answers needed); `e2e result` reads the
# queued PR's head only in the bootstrap shim (hack/e2e-queue-head.sh), and only when an OLD base's decide
# answered required=true on merge_group. The lab's e2e-lab status cannot test the queue ref.
# shellcheck disable=SC2016
check "e2e-kind.yml: e2e result looks up the queued PR head only through the guarded shim" bash -c '
	! grep -q "headRefOid" "$1" && ! grep -q "gh-readonly-queue" "$1" &&
	[ "$(grep -c "e2e-queue-head.sh" "$1")" -ge 2 ] &&
	grep -q "if \[\[ \$EVENT == merge_group && \$REQUIRED == true \]\]; then" "$1" &&
	grep -A1 "if \[\[ \$EVENT == merge_group && \$REQUIRED == true \]\]; then" "$1" | grep -q "hack/e2e-queue-head.sh"' _ "$wfdir/e2e-kind.yml"
check "e2e-kind.yml: the shim names its exit condition and the follow-up" grep -q "BOOTSTRAP SHIM (follow-up: see PR #451)" "$wfdir/e2e-kind.yml"
# The shim: merge_group + required=true (only an old base's decide produces it) -> the PR-head lookup runs,
# and hack/e2e-gate.sh then judges that head. A fake gh answers `pr view`.
fakebin=$(mktemp -d "${TMPDIR:-/tmp}/ci-mg-test.XXXXXX")
trap 'rm -rf "$fakebin"' EXIT
cat >"$fakebin/gh" <<'EOF'
#!/usr/bin/env bash
# fake gh: `pr view N --json headRefOid|headRefName --jq ...`
case "$*" in
*headRefOid*) echo "${FAKE_HEAD_SHA-0123456789abcdef0123456789abcdef01234567}" ;;
*headRefName*) echo "ci/444-fake-branch" ;;
esac
EOF
chmod +x "$fakebin/gh"
shim="$repo/hack/e2e-queue-head.sh"
export GH="$fakebin/gh"
check "shim: reads the PR head and branch from the queue ref" bash -c '
	out=$(GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-444-aaaa "$1" 2>/dev/null) &&
	grep -q "^SHA=0123456789abcdef0123456789abcdef01234567$" <<<"$out" && grep -q "^BRANCH=ci/444-fake-branch$" <<<"$out"' _ "$shim"
check "shim: fails closed on a ref without a PR number" bash -c '! GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-x-aaaa "$1" 2>/dev/null' _ "$shim"
check "shim: fails closed when the PR head is empty" bash -c '! FAKE_HEAD_SHA= GITHUB_REF=refs/heads/gh-readonly-queue/main/pr-444-aaaa "$1" 2>/dev/null' _ "$shim"
# Old-style decide on merge_group (needed=false, required=true, quickstart skipped): without a head the gate
# fails (the bug), with the shim's head and an e2e-lab success it passes.
gate="$repo/hack/e2e-gate.sh"
labgh="$fakebin/labgh"
cat >"$labgh" <<'EOF'
#!/usr/bin/env bash
case "$*" in
*"run list"*) ;;
*"/statuses"*) echo '[{"context":"e2e-lab","state":"success"}]' ;;
esac
EOF
chmod +x "$labgh"
check "gate: merge_group + required=true without a looked-up head fails closed" bash -c '! DECIDE=success QUICKSTART=skipped REQUIRED=true SHA= GH_REPO=x/y GH="$2" "$1"' _ "$gate" "$labgh"
check "gate: merge_group + required=true with the shim's head and e2e-lab success passes" bash -c '
	DECIDE=success QUICKSTART=skipped REQUIRED=true SHA=0123456789abcdef0123456789abcdef01234567 BRANCH=b GH_REPO=x/y GH="$2" "$1"' _ "$gate" "$labgh"
unset GH
# shellcheck disable=SC2016
check "e2e-kind.yml: the quickstart job is not limited to pull requests and pushes" bash -c '
	! awk "/^  quickstart:/ { j = 1 } /^  result:/ { j = 0 } j && /^    if:.*event_name/ { bad = 1 } END { exit !bad }" "$1"' _ "$wfdir/e2e-kind.yml"
# shellcheck disable=SC2016
check "e2e-kind.yml: a queue build is never cancelled" bash -c '
	grep -q "cancel-in-progress: \${{ github.event_name == .pull_request. }}" "$1"' _ "$wfdir/e2e-kind.yml"

echo "$pass passed, $fail failed"
exit $((fail > 0))
