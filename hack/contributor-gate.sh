#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The first-time contributor gate (#848, docs/maintainers/contributor-gate.md): may the author of a pull
# request open one without a maintainer's vouch? Run by .github/workflows/contributor-gate.yml, from the
# BASE commit of the pull request, so a pull request cannot edit the rule or the vouch list that judges it.
# The workflow file holds the on/off switch (rule W6 of docs/dev/ci-rules.md); this script only decides.
#
# The author passes when any of these holds:
#   - GitHub's author_association is OWNER, MEMBER, COLLABORATOR or CONTRIBUTOR (a commit already merged)
#   - the login is in the vouch list (.github/vouched.txt; one login a line, `#` comments, any case)
#   - the pull request carries the `vouched` label (only people with triage rights can set a label)
# Anything else fails, an empty or unknown association included (fail closed).
#
# Usage: hack/contributor-gate.sh check
# Environment: GATE_AUTHOR (login), GATE_ASSOCIATION (author_association), GATE_LABELS (JSON array of
# label names, default []), GATE_VOUCHED (the vouch list, default .github/vouched.txt of this checkout).
# Prints one `verdict=pass|fail reason=...` line. Exit 0 pass, 1 fail, 2 usage error. Needs jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

VOUCH_LABEL=vouched

usage() {
	echo "usage: GATE_AUTHOR=<login> GATE_ASSOCIATION=<author_association> [GATE_LABELS=<json>] [GATE_VOUCHED=<file>] $0 check" >&2
	exit 2
}

# vouched FILE LOGIN: is LOGIN (any case) a line of FILE, after `#` comments and blanks are dropped?
vouched() {
	local file=$1 login=${2,,} line
	while IFS= read -r line || [[ -n $line ]]; do
		line=${line%%#*}
		line=${line//[[:space:]]/}
		[[ -n $line && ${line,,} == "$login" ]] && return 0
	done <"$file"
	return 1
}

verdict() {
	echo "verdict=$1 reason=$2"
	if [[ $1 == fail ]]; then
		echo "::error::$GATE_AUTHOR has no merged contribution here yet, and no maintainer has vouched for them. A maintainer vouches by adding the \`$VOUCH_LABEL\` label to this pull request or the login to .github/vouched.txt (docs/maintainers/contributor-gate.md). Until then, say on the issue this pull request addresses what you plan to change (CONTRIBUTING.md)."
		exit 1
	fi
	exit 0
}

[[ ${1:-} == check && $# -eq 1 ]] || usage
[[ -n ${GATE_AUTHOR:-} ]] || usage
command -v jq >/dev/null || { echo "contributor-gate: jq is missing" >&2; exit 2; }
GATE_ASSOCIATION=${GATE_ASSOCIATION:-}
GATE_LABELS=${GATE_LABELS:-[]}
GATE_VOUCHED=${GATE_VOUCHED:-$(cd "$(dirname "$0")/.." && pwd)/.github/vouched.txt}
[[ -f $GATE_VOUCHED ]] || { echo "contributor-gate: vouch list $GATE_VOUCHED is missing" >&2; exit 2; }
labels=$(jq -r 'if type == "array" then .[] | strings else error("not an array") end' <<<"$GATE_LABELS" 2>/dev/null) ||
	{ echo "contributor-gate: GATE_LABELS is not a JSON array of names: $GATE_LABELS" >&2; exit 2; }

case $GATE_ASSOCIATION in
OWNER | MEMBER | COLLABORATOR | CONTRIBUTOR) verdict pass "association $GATE_ASSOCIATION" ;;
esac
if vouched "$GATE_VOUCHED" "$GATE_AUTHOR"; then
	verdict pass "in the vouch list"
fi
if grep -qxF -- "$VOUCH_LABEL" <<<"$labels"; then
	verdict pass "label $VOUCH_LABEL"
fi
verdict fail "association ${GATE_ASSOCIATION:-empty}, not vouched"
