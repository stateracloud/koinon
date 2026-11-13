#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/release-test-images.sh with stubbed ko, crane and docker: it names ten images under the
# registry (koinon-fakemodel, koinon-fakemcp and the eight RT1 fixtures), tags without the "v", writes a digest ref per
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
# KO_FAIL_N: the first N ko calls fail with KO_FAIL_MSG on stderr (the first push to a new GHCR package, #950)
if [[ -n ${KO_FAIL_N:-} ]]; then
	n=$(($(cat "$TMP/ko-n" 2>/dev/null || echo 0) + 1))
	echo "$n" >"$TMP/ko-n"
	if ((n <= KO_FAIL_N)); then echo "$KO_FAIL_MSG" >&2; exit 1; fi
fi
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
	rm -f "$tmp/ko-n"
	PUSH_RETRY_DELAY=0 TMP=$tmp PATH=$tmp/bin:$PATH bash "$script" "$@" >"$tmp/stdout" 2>"$tmp/err" || rc=$?
	echo "$rc"
}

check "a release candidate tag publishes" "$(run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs")" 0
check "...ten refs" "$(wc -l <"$tmp/refs" | tr -d ' ')" 10
check "...all digest refs" "$(grep -cE '^[a-z0-9-]+ ghcr\.io/o/r/[a-z0-9-]+@sha256:[0-9a-f]{64}$' "$tmp/refs")" 10
check "...the fake model server is koinon-fakemodel" "$(awk '$1 == "koinon-fakemodel" { print $2 ~ /^ghcr.io\/o\/r\/koinon-fakemodel@/ }' "$tmp/refs")" 1
check "...the fake MCP server is koinon-fakemcp" "$(awk '$1 == "koinon-fakemcp" { print $2 ~ /^ghcr.io\/o\/r\/koinon-fakemcp@/ }' "$tmp/refs")" 1
check "...the fixtures are rt1-*" "$(grep -c '^rt1-' "$tmp/refs")" 8
check "...the five ko builds use the tag without the v" "$(grep -c -- '--tags 0.1.0-rc.2 ' "$tmp/calls")" 5
check "...every image also gets the release version" "$(grep -c '^crane tag .* 0.1.0-rc.2$' "$tmp/calls")" 8
check "...no v tag is pushed" "$(grep -c 'v0.1.0' "$tmp/calls")" 0

# The drill topology must not require an image the release does not publish (#952): every image field that
# Validate() rejects as empty (test/drill/topology.go) and that is a koinon-fake* server must be in the list.
required=$(grep -oE '"(fake[A-Za-z]*Image|fakeMcpImage)": t\.' "$here/../test/drill/topology.go" | grep -oE 'fake[A-Za-z]+' | sort -u | tr '\n' ' ')
check "the drill requires the fake model and MCP images (the grep still finds them)" "$required" "fakeMcpImage fakeModelImage "
for f in koinon-fakemodel koinon-fakemcp; do
	check "...$f is published" "$(grep -c "^fakes=(.*\b$f\b" "$script")" 1
done

check "a final tag publishes too" "$(run ghcr.io/o/r v0.1.0 "$tmp/refs2")" 0
check "a tag that is not a release tag fails" "$(run ghcr.io/o/r main "$tmp/refs3")" 1
check "...and writes nothing" "$([[ -e $tmp/refs3 ]] && echo yes || echo no)" no
check "a short fixtures output fails" "$(CRANE_SHORT=1 run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs4")" 1
check "...and writes nothing" "$([[ -e $tmp/refs4 ]] && echo yes || echo no)" no

# A DENIED manifest PUT on a new package is retried (#950); any other error fails at once; DENIED for good fails
# once the attempts are spent.
denied='PUT https://ghcr.io/v2/o/r/rt1-x/manifests/1: DENIED: requested access to the resource is denied'
check "DENIED twice, then success, publishes" "$(KO_FAIL_N=2 KO_FAIL_MSG=$denied run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs5")" 0
check "...two failed attempts plus the five ko builds" "$(cat "$tmp/ko-n")" 7
check "...and said why it retried" "$(grep -c 'push retry: DENIED' "$tmp/err")" 2
check "DENIED on every attempt fails" "$(KO_FAIL_N=99 KO_FAIL_MSG=$denied run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs6")" 1
check "...after exactly five attempts" "$(cat "$tmp/ko-n")" 5
check "...and says so" "$(grep -c 'still DENIED after 5 attempts' "$tmp/err")" 1
check "...and writes nothing" "$([[ -e $tmp/refs6 ]] && echo yes || echo no)" no
check "another error fails at once" "$(KO_FAIL_N=99 KO_FAIL_MSG='PUT: UNAUTHORIZED: authentication required' run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs7")" 1
check "...with no retry" "$(cat "$tmp/ko-n")" 1
check "...and no retry message" "$(grep -c 'push retry' "$tmp/err")" 0

# Lower-case docker wording, the rt1 path (run -> push_retry), and stdout passing through a retry.
cat >"$tmp/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "docker $*" >>"$TMP/calls"
if [[ $1 == buildx && $2 == build ]]; then
	n=$(($(cat "$TMP/dk-n" 2>/dev/null || echo 0) + 1))
	echo "$n" >"$TMP/dk-n"
	if ((n <= ${DK_FAIL_N:-0})); then echo "${DK_MSG:-denied: requested access to the resource is denied}" >&2; exit 1; fi
fi
STUB
rm -f "$tmp/dk-n"
check "docker's lower-case denied is retried" "$(DK_FAIL_N=2 run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs8")" 0
check "...three buildx attempts for the first image, one for the second" "$(cat "$tmp/dk-n")" 4
check "...and the fixtures output still came through" "$(grep -c '^refs\|rt1-' "$tmp/refs8")" 8
rm -f "$tmp/dk-n"
check "a local permission denied is not retried" "$(DK_FAIL_N=9 DK_MSG="permission denied while trying to connect to the docker socket" run ghcr.io/o/r v0.1.0-rc.2 "$tmp/refs9")" 1
check "...one attempt only" "$(cat "$tmp/dk-n")" 1

((fails == 0)) || { echo "$fails failed" >&2; exit 1; }
echo "all passed"
