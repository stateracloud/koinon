#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The agentgateway image Koinon pins (#238). The chart's gateway.standalone.image in
# charts/koinon/values.yaml is the one place it is named: repository, tag and digest. The mirror
# workflow (.github/workflows/mirror-agentgateway-test-image.yml) copies exactly that digest to
# GHCR, and the Go tests use the mirrored copy, so a test pulls what an install runs.
#
#   hack/agentgateway-pin.sh            print "<repository> <tag> <digest>"
#   hack/agentgateway-pin.sh --check    fail unless the pin is a full sha256 digest, the mirror
#                                       workflow takes it from here (no literal digest of its own),
#                                       the Go tests' image (internal/testing/agentgatewayimage) is
#                                       the same repository and digest, and no other Go file in the
#                                       repository names an agentgateway image itself
#
# Reads values.yaml with awk, not yq, so it runs wherever bash does. VALUES=<file>, WORKFLOW=<file>,
# GOCONST=<file> and GO_TREE=<dir> (searched instead of the repository's tracked Go files) point it
# elsewhere (the tests do).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."
values=${VALUES:-charts/koinon/values.yaml}
workflow=${WORKFLOW:-.github/workflows/mirror-agentgateway-test-image.yml}
goconst=${GOCONST:-internal/testing/agentgatewayimage/image.go}
gotree=${GO_TREE:-}

# The scalars under gateway: > standalone: > image:, by indentation; comments and quotes stripped.
read_pin() {
	awk '
		function val(s) { sub(/^[^:]*:[[:space:]]*/, "", s); sub(/[[:space:]]+#.*$/, "", s); gsub(/["'\'']/, "", s); return s }
		/^[^[:space:]#]/ { top = ($0 ~ /^gateway:/); sa = 0; img = 0; next }
		top && /^  [^[:space:]#]/ { sa = ($0 ~ /^  standalone:/); img = 0; next }
		top && sa && /^    [^[:space:]#]/ { img = ($0 ~ /^    image:/); next }
		top && sa && img && /^      repository:/ { r = val($0) }
		top && sa && img && /^      tag:/ { t = val($0) }
		top && sa && img && /^      digest:/ { d = val($0) }
		END { print r, t, d }
	' "$values"
}

read -r repo tag digest < <(read_pin)

if [[ ${1:-} != --check ]]; then
	[[ $# -eq 0 ]] || { echo "agentgateway-pin: unknown argument $1" >&2; exit 2; }
	[[ -n $repo && -n $tag && -n $digest ]] || { echo "agentgateway-pin: gateway.standalone.image in $values lacks repository, tag or digest" >&2; exit 1; }
	echo "$repo $tag $digest"
	exit 0
fi

fail=0
err() { echo "agentgateway-pin: $*" >&2; fail=1; }
[[ -n $repo ]] || err "gateway.standalone.image.repository is empty in $values"
[[ -n $tag ]] || err "gateway.standalone.image.tag is empty in $values"
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || err "gateway.standalone.image.digest in $values is not a sha256 digest: '${digest}'"
if [[ -f $workflow ]]; then
	grep -q 'hack/agentgateway-pin.sh' "$workflow" || err "$workflow does not read the pin from hack/agentgateway-pin.sh"
	! grep -Eq 'sha256:[0-9a-f]{64}' "$workflow" || err "$workflow carries a literal digest; it must take the chart's"
else
	err "$workflow is missing"
fi
if [[ -f $goconst ]]; then
	godigest=$(sed -n 's/^[[:space:]]*Digest[[:space:]]*=[[:space:]]*"\(sha256:[0-9a-f]*\)".*/\1/p' "$goconst")
	[[ $godigest == "$digest" ]] || err "$goconst has Digest '${godigest}', the chart pins '${digest}'"
	gorepo=$(sed -n 's/^[[:space:]]*Upstream[[:space:]]*=[[:space:]]*"\([^"@]*\)@".*/\1/p' "$goconst")
	[[ $gorepo == "$repo" ]] || err "$goconst has Upstream repository '${gorepo}', the chart pins '${repo}'"
	# Every other Go file takes the image from that package: no agentgateway image reference of its own.
	if [[ -n $gotree ]]; then
		others=$(grep -rlE --include='*.go' 'agentgateway(:v[0-9]|@sha256:)' "$gotree" | grep -vxF "$goconst" || true)
	else
		others=$(git ls-files -z '*.go' | xargs -0 grep -lE 'agentgateway(:v[0-9]|@sha256:)' | grep -vxF "$goconst" || true)
	fi
	[[ -z $others ]] || err "Go files name an agentgateway image themselves; use agentgatewayimage.Ref(): ${others//$'\n'/ }"
else
	err "$goconst is missing"
fi
[[ $fail == 0 ]] || exit 1
echo "agentgateway-pin: $repo:$tag@$digest"
