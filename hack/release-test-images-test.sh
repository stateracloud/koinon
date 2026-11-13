#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/release-test-images.sh with stubbed ko, crane and docker: it names nine images under the
# registry (koinon-fakemodel and the eight RT1 fixtures), tags without the "v", writes a digest ref per
# image, and fails when the fixtures output is short or the tag is not a release tag.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=$here/release-test-images.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/release-test-images-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# ko, docker, crane: record the call; crane digest answers a digest made from the reference.
cat >"$tmp/bin/ko" <<'STUB'
#!/usr/bin/env bash
echo "ko KO_DOCKER_REPO=$KO_DOCKER_REPO $*" >>"$TMP/calls"
STUB
cat >"$tmp/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >>"$TMP/calls"
STUB
cat >"$tmp/bin/crane" <<'STUB'
#!/usr/bin/env bash
echo "crane $*" >>"$TMP/calls"
if [[ $1 == digest ]]; then
	[[ -n ${CRANE_SHORT:-} && $2 == */rt1-promgen:* ]] && { echo "sha256:abc"; exit 0; }
	printf 'sha256:%s\n' "$(printf '%s' "$2" | shasum -a 256 | cut -c1-64)"
fi
STUB
chmod +x "$tmp"/bin/*

fails=0
check() { # name got want
	if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fails=$((fails + 1)); fi
}
run() { # args: exit code; stderr in $tmp/err
	local rc=0
	: >"$tmp/calls"
	TMP=$tmp PATH=$tmp/bin:$PATH bash "$script" "$@" >"$tmp/stdout" 2>"$tmp/err" || rc=$?
	echo "$rc"
}

check "a release candidate tag publishes" "$(run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs")" 0
check "...nine refs" "$(wc -l <"$tmp/refs" | tr -d ' ')" 9
check "...all digest refs" "$(grep -cE '^[a-z0-9-]+ ghcr\.io/o/r/[a-z0-9-]+@sha256:[0-9a-f]{64}$' "$tmp/refs")" 9
check "...the fake model server is koinon-fakemodel" "$(awk '$1 == "koinon-fakemodel" { print $2 ~ /^ghcr.io\/o\/r\/koinon-fakemodel@/ }' "$tmp/refs")" 1
check "...the fixtures are rt1-*" "$(grep -c '^rt1-' "$tmp/refs")" 8
check "...the four ko builds use the tag without the v" "$(grep -c -- '--tags 0.1.0-rc.2 ' "$tmp/calls")" 4
check "...every image also gets the release version" "$(grep -c '^crane tag .* 0.1.0-rc.2$' "$tmp/calls")" 8
check "...no v tag is pushed" "$(grep -c 'v0.1.0' "$tmp/calls")" 0

check "a final tag publishes too" "$(run ghcr.io/o/r v0.1.0 "$tmp/refs2")" 0
check "a tag that is not a release tag fails" "$(run ghcr.io/o/r main "$tmp/refs3")" 1
check "...and writes nothing" "$([[ -e $tmp/refs3 ]] && echo yes || echo no)" no
check "a short fixtures output fails" "$(CRANE_SHORT=1 run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs4")" 1
check "...and writes nothing" "$([[ -e $tmp/refs4 ]] && echo yes || echo no)" no

((fails == 0)) || { echo "$fails failed" >&2; exit 1; }
echo "all passed"
