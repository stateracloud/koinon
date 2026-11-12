#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/contributor-gate.sh (#848), and of the shape of .github/workflows/contributor-gate.yml:
# the switch line has one of its two documented forms, and the job checks out the base commit without
# persisted credentials.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
script=$root/hack/contributor-gate.sh
workflow=$root/.github/workflows/contributor-gate.yml
command -v jq >/dev/null || { echo "SKIPPED: jq is missing"; exit 0; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/contributor-gate-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/vouched.txt" <<'EOF'
# a comment
  Vouched-Person   # trailing comment and spaces
dependabot[bot]

EOF
printf 'no-newline-at-end' >"$tmp/vouched-nonl.txt"

fails=0
# expect NAME WANT_EXIT WANT_SUBSTRING [VAR=value...]
expect() {
	local name=$1 want=$2 sub=$3 out rc=0
	shift 3
	out=$(env -u GATE_AUTHOR -u GATE_ASSOCIATION -u GATE_LABELS GATE_VOUCHED="$tmp/vouched.txt" "$@" "$script" check 2>&1) || rc=$?
	if [[ $rc == "$want" ]] && grep -qF -- "$sub" <<<"$out"; then
		echo "ok: $name"
	else
		echo "FAIL: $name (want exit $want and '$sub', got exit $rc)"
		printf '    %s\n' "${out//$'\n'/$'\n    '}"
		fails=1
	fi
}

for a in OWNER MEMBER COLLABORATOR CONTRIBUTOR; do
	expect "association $a passes" 0 "verdict=pass reason=association $a" GATE_AUTHOR=someone GATE_ASSOCIATION=$a
done
for a in FIRST_TIME_CONTRIBUTOR FIRST_TIMER NONE MANNEQUIN owner; do
	expect "association $a fails" 1 "verdict=fail reason=association $a, not vouched" GATE_AUTHOR=someone GATE_ASSOCIATION=$a
done
expect "empty association fails closed" 1 "verdict=fail reason=association empty" GATE_AUTHOR=someone GATE_ASSOCIATION=
expect "failure explains how to get vouched" 1 "::error::someone has no merged contribution" GATE_AUTHOR=someone GATE_ASSOCIATION=NONE
expect "vouch list, other case, comment and spaces" 0 "verdict=pass reason=in the vouch list" GATE_AUTHOR=vouched-person GATE_ASSOCIATION=FIRST_TIME_CONTRIBUTOR
expect "vouch list holds a bot" 0 "reason=in the vouch list" "GATE_AUTHOR=dependabot[bot]" GATE_ASSOCIATION=NONE
expect "a comment is not a login" 1 "verdict=fail" "GATE_AUTHOR=a" GATE_ASSOCIATION=NONE
expect "a prefix of a vouched login is not vouched" 1 "verdict=fail" GATE_AUTHOR=vouched GATE_ASSOCIATION=NONE
expect "last line without a newline" 0 "reason=in the vouch list" GATE_VOUCHED="$tmp/vouched-nonl.txt" GATE_AUTHOR=no-newline-at-end GATE_ASSOCIATION=NONE
expect "vouched label passes" 0 "verdict=pass reason=label vouched" GATE_AUTHOR=someone GATE_ASSOCIATION=FIRST_TIMER 'GATE_LABELS=["area/ci","vouched"]'
expect "other labels do not pass" 1 "verdict=fail" GATE_AUTHOR=someone GATE_ASSOCIATION=NONE 'GATE_LABELS=["vouched-later","Vouched"]'
expect "no labels" 1 "verdict=fail" GATE_AUTHOR=someone GATE_ASSOCIATION=NONE 'GATE_LABELS=[]'
expect "labels not a JSON array" 2 "not a JSON array" GATE_AUTHOR=someone GATE_ASSOCIATION=NONE 'GATE_LABELS={"name":"vouched"}'
expect "labels not JSON" 2 "not a JSON array" GATE_AUTHOR=someone GATE_ASSOCIATION=NONE 'GATE_LABELS=vouched'
expect "no author" 2 "usage:" GATE_ASSOCIATION=OWNER
expect "missing vouch list" 2 "is missing" GATE_VOUCHED="$tmp/nope.txt" GATE_AUTHOR=someone GATE_ASSOCIATION=OWNER
expect "the committed vouch list parses" 0 "reason=in the vouch list" GATE_VOUCHED="$root/.github/vouched.txt" "GATE_AUTHOR=dependabot[bot]" GATE_ASSOCIATION=NONE

out=$("$script" 2>&1) && rc=0 || rc=$?
if [[ $rc == 2 ]] && grep -qF "usage:" <<<"$out"; then echo "ok: no subcommand"; else echo "FAIL: no subcommand (exit $rc)"; fails=1; fi

# The workflow: exactly one switch line, in a documented form; base checkout; no persisted credentials.
switch=$(grep -E '^    if: ' "$workflow" || true)
case $switch in
"    if: false && github.event_name == 'pull_request'") echo "ok: workflow switch parses (gate is OFF)" ;;
"    if: true && github.event_name == 'pull_request'") echo "ok: workflow switch parses (gate is ON)" ;;
*) echo "FAIL: the job-level if: of $workflow is not one of the two forms docs/maintainers/contributor-gate.md names: '$switch'"; fails=1 ;;
esac
for want in "ref: \${{ github.event.pull_request.base.sha }}" "persist-credentials: false" "run: hack/contributor-gate.sh check" "types: [checks_requested]"; do
	if grep -qF -- "$want" "$workflow"; then echo "ok: workflow has '$want'"; else echo "FAIL: $workflow lacks '$want'"; fails=1; fi
done
if grep -vE '^[[:space:]]*#' "$workflow" | grep -qE 'pull_request_target|secrets\.'; then echo "FAIL: $workflow uses pull_request_target or a secret"; fails=1; else echo "ok: workflow uses no pull_request_target and no secret"; fi

exit $fails
