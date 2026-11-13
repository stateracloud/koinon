#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): runs in the upstream release workflow, before hack/release-gate.sh.
#
# Waits for the restore drill that the push of an rc tag starts (the `drill` job of e2e-kind.yml, ADR 0109,
# #518; it takes up to two hours), so the release `gate` job does not fail only because the drill is still
# running. It only waits: it never reads a result and never makes anything pass. hack/release-gate.sh stays
# the single judge and fails closed on a missing, red or unreadable drill result, so a timeout here, a run
# that never appears, or an API error all end in the gate's own failure and never in a release. A final tag
# (no -rc.) needs no wait: its evidence is the lab result attached to the rc release.
#
# Environment: TAG, SHA, GITHUB_REPOSITORY, GH_TOKEN (actions:read). Optional, in seconds:
#   DRILL_WAIT_APPEAR (600)  how long to wait for the tag's e2e-kind run to exist at all
#   DRILL_WAIT_MAX (9000)    how long to wait for that run to finish (the job's own limit is 120 minutes)
#   DRILL_WAIT_POLL (60)     seconds between two looks

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

: "${TAG:?}" "${SHA:?}" "${GITHUB_REPOSITORY:?}" "${GH_TOKEN:?}"
appear=${DRILL_WAIT_APPEAR:-600}
max=${DRILL_WAIT_MAX:-9000}
poll=${DRILL_WAIT_POLL:-60}
for v in "$appear" "$max" "$poll"; do
	[[ $v =~ ^[0-9]+$ ]] || { echo "release drill wait: '$v' is not a number of seconds" >&2; exit 1; }
done

if [[ $TAG != *-rc.* ]]; then
	echo "release drill wait: $TAG is not an rc tag; nothing to wait for"
	exit 0
fi

# The newest e2e-kind run that this tag push started, as "<id> <status>"; empty when there is none.
run_of_tag() {
	gh api "repos/$GITHUB_REPOSITORY/actions/workflows/e2e-kind.yml/runs?head_sha=$SHA&event=push&per_page=30" \
		--jq "[.workflow_runs[] | select(.head_branch == \"$TAG\")] | sort_by(.created_at) | last | select(. != null) | \"\\(.id) \\(.status)\""
}

start=$SECONDS
seen=
while :; do
	row=$(run_of_tag) || { [[ -n ${warned:-} ]] || echo "release drill wait: the API call failed (will keep trying until the limits)" >&2; warned=1; row=; }
	if [[ -n $row ]]; then
		[[ -n $seen ]] || { echo "release drill wait: the drill run of $TAG is ${row% *}; waiting for it to finish"; seen=1; }
		if [[ ${row#* } == completed ]]; then
			echo "release drill wait: run ${row% *} finished after $((SECONDS - start)) s"
			exit 0
		fi
		if ((SECONDS - start >= max)); then
			echo "release drill wait: run ${row% *} is still going after $max s; the gate decides on what exists" >&2
			exit 0
		fi
	elif [[ -n $seen ]]; then
		# the run was seen and one look failed: keep waiting, up to the same limit
		if ((SECONDS - start >= max)); then
			echo "release drill wait: no answer from the API after $max s; the gate decides on what exists" >&2
			exit 0
		fi
	elif ((SECONDS - start >= appear)); then
		echo "release drill wait: no e2e-kind run for $TAG after $appear s; the gate decides on what exists" >&2
		exit 0
	fi
	sleep "$poll"
done
