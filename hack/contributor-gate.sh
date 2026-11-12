#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The first-time contributor gate (#848, docs/maintainers/contributor-gate.md): may the author of a pull
# request open one without a maintainer's vouch? Run by .github/workflows/contributor-gate.yml from a checkout of
# the base branch (its tip on a pull request, the queue's base on a merge queue build), so this script and the
# vouch list are main's. The workflow file itself is the pull request's own, as for every `pull_request` run; the
# guard against a pull request that edits it is the code-owner review of .github/ (rule W6 of
# docs/dev/ci-rules.md). The workflow file holds the on/off switch; this script only decides.
#
# The author passes when any of these holds:
#   - GitHub's author_association is OWNER, MEMBER, COLLABORATOR or CONTRIBUTOR (a commit already merged)
#   - the login is in the vouch list (.github/vouched.txt; one login a line, `#` comments, any case)
#   - the pull request carries the `vouched` label (only people with triage rights can set a label)
# Anything else fails, an empty or unknown association included (fail closed).
#
# Usage:
#   hack/contributor-gate.sh check        judge GATE_AUTHOR, GATE_ASSOCIATION (author_association) and GATE_LABELS
#                                         (JSON array of label names, default []): a pull_request event's payload
#   hack/contributor-gate.sh check-pr <N> read those three from pull request N with `gh api` (a merge queue build,
#                                         whose event names no pull request); REPO=owner/name, GH (default gh)
# GATE_VOUCHED is the vouch list (default .github/vouched.txt of this checkout).
# Prints one `verdict=pass|fail reason=...` line. Exit 0 pass, 1 fail (a failed API read included), 2 usage
# error. Needs jq; check-pr needs gh.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

VOUCH_LABEL=vouched

usage() {
	echo "usage: GATE_AUTHOR=<login> GATE_ASSOCIATION=<author_association> [GATE_LABELS=<json>] [GATE_VOUCHED=<file>] $0 check" >&2
	echo "       REPO=<owner/name> [GH=gh] [GATE_VOUCHED=<file>] $0 check-pr <N>" >&2
	exit 2
}

# vouched FILE LOGIN: is LOGIN (any case) a line of FILE, after `#` comments and blanks are dropped?
vouched() {
	local file=$1 login=${2,,} line
	while IFS= read -r line || [[ -n $line ]]; do
		line=${line%%#*}
		line=${line#"${line%%[![:space:]]*}"}
		line=${line%"${line##*[![:space:]]}"}
		[[ -n $line && ${line,,} == "$login" ]] && return 0
	done <"$file"
	return 1
}

verdict() {
	echo "verdict=$1 reason=$2"
	if [[ $1 == fail ]]; then
		echo "::error::$GATE_AUTHOR has no merged contribution here yet, and no maintainer has vouched for them. A maintainer vouches by adding the \`$VOUCH_LABEL\` label to this pull request or the login to .github/vouched.txt (docs/maintainers/contributor-gate.md). Until then, say on the issue this pull request addresses what you plan to change, and ask for a vouch there."
		exit 1
	fi
	exit 0
}

command -v jq >/dev/null || { echo "contributor-gate: jq is missing" >&2; exit 2; }
case ${1:-} in
check) [[ $# -eq 1 ]] || usage ;;
check-pr)
	[[ $# -eq 2 && $2 =~ ^[0-9]+$ && -n ${REPO:-} ]] || usage
	# Fails closed: a read that fails, or a reply without a login, fails the check (exit 1), never passes it.
	pr=$("${GH:-gh}" api "repos/$REPO/pulls/$2") || { echo "::error::cannot read pull request #$2 of $REPO"; echo "verdict=fail reason=cannot read pull request #$2"; exit 1; }
	GATE_AUTHOR=$(jq -r '.user.login // empty' <<<"$pr" 2>/dev/null) || GATE_AUTHOR=
	[[ -n $GATE_AUTHOR ]] || { echo "::error::pull request #$2 of $REPO has no author login"; echo "verdict=fail reason=no author login on #$2"; exit 1; }
	GATE_ASSOCIATION=$(jq -r '.author_association // empty' <<<"$pr")
	GATE_LABELS=$(jq -c '[.labels[]?.name]' <<<"$pr")
	;;
*) usage ;;
esac
[[ -n ${GATE_AUTHOR:-} ]] || usage
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
