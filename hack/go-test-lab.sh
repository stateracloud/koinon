#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Helpers for the commit status `go-test-lab` (old issue 610): the record that an external run of go-test
# (posted through hack/e2e-status.sh) passed on exactly one commit. The ci.yml job go-test is satisfied
# without running when the pull request head has a green one.
#
#   hack/go-test-lab.sh state <sha>             prints the newest go-test-lab state on the commit
#                                               (success, failure, error, pending), or nothing when it
#                                               has none. Exit 1 when GitHub cannot be read.
#   hack/go-test-lab.sh touches-go <base> <head>  exit 0 when the change base..head is one the go-*
#                                               jobs run for (hack/ci-changes.sh go=true), 1 when it is
#                                               not (documentation, web), 2 when that cannot be told.
#
# `state` reads the repository from REPO or GITHUB_REPOSITORY, else from the current directory. The
# status is not a required context of the ruleset (not every contributor can produce it) and
# anyone with write access can post it: the same trust limit as `e2e-lab` (hack/e2e-gate.sh, old issue 568), a
# guardrail against mistakes and not against a malicious member. Needs gh, jq (and git for touches-go).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

root=$(cd "$(dirname "$0")/.." && pwd)

case ${1:-} in
state)
	sha=${2:?usage: hack/go-test-lab.sh state <sha>}
	repo=${REPO:-${GITHUB_REPOSITORY:-'{owner}/{repo}'}}
	resp=$(gh api "repos/$repo/commits/$sha/statuses?per_page=100") || exit 1
	# The newest status of the context is first. Exactly the commit asked for: a status on another
	# commit is not in this list.
	jq -r '[.[] | select(.context == "go-test-lab")][0].state // ""' <<<"$resp" || exit 1
	;;
touches-go)
	base=${2:?usage: hack/go-test-lab.sh touches-go <base> <head>} head=${3:?usage: hack/go-test-lab.sh touches-go <base> <head>}
	changed=$(git diff --name-only "$base" "$head" 2>/dev/null) || exit 2
	[[ -n $changed ]] || exit 2
	# Only go= is read: the directories of the flavors suite (a `go list`) are not needed.
	line=$(CI_CHANGES_FLAVORS_DIRS=${CI_CHANGES_FLAVORS_DIRS:-none} "$root/hack/ci-changes.sh" --stdin <<<"$changed" 2>/dev/null | grep -x 'go=.*') || exit 2
	case $line in
	go=true) exit 0 ;;
	go=false) exit 1 ;;
	*) exit 2 ;;
	esac
	;;
*)
	echo "usage: hack/go-test-lab.sh state <sha> | touches-go <base> <head>" >&2
	exit 2
	;;
esac
