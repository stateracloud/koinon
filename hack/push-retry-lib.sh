#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): sourced by hack/rt1-fixtures-build.sh and hack/release-test-images.sh (#950). Not run on its own.
#
# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

# push_retry CMD [ARG...] runs a registry push (ko build, docker buildx --push, docker push, crane copy,
# crane tag) and repeats it ONLY when it failed with GHCR's "DENIED" answer, with a short bounded backoff.
# Any other failure returns at once with the command's own status and no second attempt.
#
# Why: the first manifest PUT to a GHCR package that does not exist yet is refused with DENIED, although the
# blobs and the SBOM went up and the package was created in the same moment. GHCR creates the package and links
# it to the repository (which is what gives the workflow's GITHUB_TOKEN write access to it) asynchronously, so
# that first PUT races the link. A second PUT a few seconds later succeeds. A real permission failure stays
# DENIED for every attempt, so the bound turns it into a failure after about a minute.
#
#   PUSH_RETRY_ATTEMPTS  total attempts, default 5
#   PUSH_RETRY_DELAY     seconds before the second attempt, default 3; it doubles each time (3+6+12+24 = 45 s)
#
# stdout of the command passes through; stderr is shown as it comes and also inspected.

push_retry() {
	local attempts=${PUSH_RETRY_ATTEMPTS:-5} delay=${PUSH_RETRY_DELAY:-3} n=1 rc errf
	errf=$(mktemp "${TMPDIR:-/tmp}/push-retry.XXXXXX") || return 1
	while :; do
		rc=0
		"$@" 2> >(tee "$errf" >&2) || rc=$?
		wait # the tee process substitution finished, errf is complete
		((rc == 0)) && { rm -f "$errf"; return 0; }
		if ! grep -q 'DENIED' "$errf"; then
			rm -f "$errf"
			return "$rc"
		fi
		if ((n >= attempts)); then
			echo "push retry: still DENIED after $n attempts: $*" >&2
			rm -f "$errf"
			return "$rc"
		fi
		echo "push retry: DENIED (attempt $n of $attempts; a new GHCR package is linked to the repository asynchronously, #950), again in ${delay}s: $*" >&2
		sleep "$delay"
		delay=$((delay * 2))
		n=$((n + 1))
	done
}
