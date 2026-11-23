#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): sourced by hack/rt1-fixtures-build.sh, hack/release-test-images.sh (#950) and
# test/e2e/kind/images.sh (#1224). Not run on its own.
#
# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

# push_retry CMD [ARG...] runs a registry push or a ko build (ko build, docker buildx --push, docker push, crane
# copy, crane tag) and repeats it ONLY when it failed with GHCR's "DENIED" answer or with a short registry fault
# (see below), with a short bounded backoff. Any other failure returns at once with the command's own status and
# no second attempt.
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
# A short registry fault is HTTP 500, 502, 503, 504 (not 501; INTERNAL_SERVER_ERROR, "Too many connections"), 429, a reset
# connection, an EOF on a registry request (a Go syntax error "unexpected EOF" is not one) or a timeout. ko pulls its base image (cgr.dev/chainguard/static by default) on
# every build, and a fault of cgr.dev (HTTP 500, "Error 1040: Too many connections") failed the merge queue e2e three
# times before any spec ran (#1224). The compile error, UNAUTHORIZED or a missing image is no short fault: it fails
# at once, so a real fault is not hidden for a minute. The last attempt's own status is the return status.
#
# The DENIED match is "denied: requested access" in any case: ko and crane print DENIED, docker and buildx print denied.
# stdout of the command passes through; stderr is shown as it comes and also inspected.

_retry_denied='denied: requested access'
_retry_fault='INTERNAL_SERVER_ERROR|BAD_GATEWAY|SERVICE_UNAVAILABLE|GATEWAY_TIMEOUT|TOOMANYREQUESTS|TOO_MANY_REQUESTS|Too many connections|(status code|HTTP) (429|50[0234])([^0-9]|$)|(GET|PUT|HEAD|POST|PATCH) https?://[^ ]+: .*EOF|connection reset|i/o timeout|TLS handshake timeout|Client\.Timeout'

push_retry() {
	local attempts=${PUSH_RETRY_ATTEMPTS:-5} delay=${PUSH_RETRY_DELAY:-3} n=1 rc errf label reason
	errf=$(mktemp "${TMPDIR:-/tmp}/push-retry.XXXXXX") || return 1
	while :; do
		rc=0
		# stderr goes through tee in a pipeline, which waits for tee, so errf is complete below; stdout passes through
		# on fd 3 untouched. The command's own status is written to a file (PIPESTATUS is unreliable with set -e).
		{ { "$@" && echo 0 >"$errf.rc" || echo $? >"$errf.rc"; } 2>&1 >&3 3>&- | tee "$errf" >&2 3>&-; } 3>&1 || true
		rc=$(cat "$errf.rc") && rm -f "$errf.rc"
		((rc == 0)) && { rm -f "$errf" "$errf.rc"; return 0; }
		if grep -qiE "$_retry_denied" "$errf"; then
			label=DENIED reason="a new GHCR package is linked to the repository asynchronously, #950"
		elif grep -qiE "$_retry_fault" "$errf"; then
			label="a registry fault" reason="a short registry fault, #1224"
		else
			rm -f "$errf" "$errf.rc"
			return "$rc"
		fi
		if ((n >= attempts)); then
			echo "push retry: still $label after $n attempts: $*" >&2
			rm -f "$errf" "$errf.rc"
			return "$rc"
		fi
		echo "push retry: $label (attempt $n of $attempts; $reason), again in ${delay}s: $*" >&2
		sleep "$delay"
		delay=$((delay * 2))
		n=$((n + 1))
	done
}
