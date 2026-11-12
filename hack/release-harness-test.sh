#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests the three modes of hack/release-harness.sh against a stub docker: the arguments they pass to
# buildx, the digest and ref they print, and that a bad platform or digest is refused.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d "${TMPDIR:-/tmp}/release-harness-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
d=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
# The stub logs its arguments and writes the digest to the --metadata-file the script passes.
cat >"$work/bin/docker" <<STUB
#!/usr/bin/env bash
echo "\$*" >>"$work/calls"
while (( \$# )); do
	if [[ \$1 == --metadata-file ]]; then echo '{"containerimage.digest":"$d"}' >"\$2"; fi
	shift
done
STUB
chmod +x "$work/bin/docker"
export PATH="$work/bin:$PATH"

R=localhost:5000
commit=$(git rev-parse HEAD)
fail() { echo "FAIL: $1" >&2; cat "$work/calls" >&2 || true; exit 1; }
has() { grep -qF -- "$2" "$work/calls" || fail "$1: no '$2' in the docker call"; }
hasnt() { ! grep -qF -- "$2" "$work/calls" || fail "$1: unexpected '$2' in the docker call"; }

: >"$work/calls"
out=$(hack/release-harness.sh --platform linux/arm64 0.1.0-rc.1 $R "$commit" 2>/dev/null)
[[ $out == "digest=$d" ]] || fail "platform mode printed '$out'"
has platform "--platform linux/arm64"
has platform "push-by-digest=true"
has platform "rewrite-timestamp=true"
has platform "--cache-from type=registry,ref=$R/sandbox:buildcache-arm64"
has platform "--cache-to type=registry,ref=$R/sandbox:buildcache-arm64,mode=max,ignore-error=true"
has platform "SOURCE_DATE_EPOCH="
has platform "--sbom=true --provenance=mode=min"
hasnt platform "--tag"
echo "ok: --platform pushes by digest with a per-platform cache"

: >"$work/calls"
out=$(hack/release-harness.sh --merge 0.1.0 $R "$commit" "$d" "$d" 2>/dev/null)
[[ $out == "ref=$R/sandbox@$d" ]] || fail "merge mode printed '$out'"
has merge "imagetools create --tag $R/sandbox:0.1.0 --tag $R/sandbox:latest"
has merge "$R/sandbox@$d $R/sandbox@$d"
echo "ok: --merge joins the digests and tags a final release latest"

: >"$work/calls"
hack/release-harness.sh --merge 0.1.0-rc.2 $R "$commit" "$d" "$d" >/dev/null 2>&1
hasnt merge-rc ":latest"
echo "ok: --merge does not move latest for an rc"

: >"$work/calls"
out=$(hack/release-harness.sh 0.1.0-rc.1 $R "$commit" 2>/dev/null)
[[ $out == "ref=$R/sandbox@$d" ]] || fail "default mode printed '$out'"
has default "--platform linux/amd64,linux/arm64"
has default "--tag $R/sandbox:0.1.0-rc.1"
hasnt default "cache-from"
echo "ok: the one-call form is unchanged"

if hack/release-harness.sh --platform linux/s390x 0.1.0 $R "$commit" >/dev/null 2>&1; then fail "bad platform accepted"; fi
if hack/release-harness.sh --merge 0.1.0 $R "$commit" nonsense nonsense >/dev/null 2>&1; then fail "bad digest accepted"; fi
echo "ok: bad platform and bad digest are refused"
