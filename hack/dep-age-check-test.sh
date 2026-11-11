#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests (no network) of expand_vars in hack/dep-age-check.sh: the version a file keeps in a
# variable is what gets dated. Covers the shell, Dockerfile and YAML `NAME: value` forms (plain,
# quoted, trailing comment, list item), and that a name defined with two different values or not at
# all is left unexpanded (so dating it fails closed) instead of the first definition winning (old issue 549).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dep-age-check-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

# Only the pure helpers: the script itself needs git and the network for a full check.
eval "$(sed -n \
	'/^expand_vars() {/,/^}/p; /^pm_pin() {/,/^}/p; /^dockerfile_specs=/p; /^image_registry_host() {/,/^}/p; /^oci_annotation_created() {/,/^}/p' \
	"$repo/hack/dep-age-check.sh")"

fail=0
# The URLs are assembled from parts so that no release-asset URL literal appears in this file:
# dep-age-check scans every changed file, this one included, and would try to date a fixture.
scheme="https:/" host="github.com" path="o/r/releases/download"
# shellcheck disable=SC2016 # a literal ${NAME} for expand_vars
url="$scheme/$host/$path/"'v${TOOL_VERSION}/x'
want() { # want <name> <file content> <expected url>
	local f="$tmp/f" got
	printf '%s\n' "$2" >"$f"
	got=$(expand_vars "$f" "$url")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
expanded="$scheme/$host/$path/v1.2.3/x"

want "shell NAME=value" 'TOOL_VERSION=1.2.3' "$expanded"
want "Dockerfile ARG" 'ARG TOOL_VERSION=1.2.3' "$expanded"
want "YAML plain" '  TOOL_VERSION: 1.2.3' "$expanded"
want "YAML double quoted" '  TOOL_VERSION: "1.2.3"' "$expanded"
want "YAML single quoted" "  TOOL_VERSION: '1.2.3'" "$expanded"
want "YAML trailing comment" '  TOOL_VERSION: 1.2.3 # newest at least 3 days old' "$expanded"
want "YAML list item" '      - TOOL_VERSION: 1.2.3' "$expanded"
want "the same value twice is fine" 'TOOL_VERSION=1.2.3
  TOOL_VERSION: 1.2.3' "$expanded"
want "two different values stay unexpanded" 'TOOL_VERSION=1.2.3
  TOOL_VERSION: 9.9.9' "$url"
want "an undefined name stays unexpanded" 'OTHER_VERSION=1.2.3' "$url"
want "a longer name is not the name" 'MY_TOOL_VERSION: 9.9.9' "$url"

pins() { # pins <name> <line> <expected "package version" or empty>
	local got
	got=$(pm_pin "$2")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
pins "packageManager pin" '  "packageManager": "pnpm@12.18.0",' "pnpm 12.18.0"
pins "packageManager pin with hash" '  "packageManager": "pnpm@12.18.0+sha512.abc"' "pnpm 12.18.0"
pins "Dockerfile PNPM_VERSION" 'ARG PNPM_VERSION=12.18.0' "pnpm 12.18.0"
pins "other ARG is not a pin" 'ARG NODE_VERSION=24.1.0' ""
pins "unrelated JSON line" '  "version": "0.0.0",' ""

# The Dockerfile pathspecs must select every Dockerfile, named ones included.
# shellcheck disable=SC2154 # defined by the eval above
selected=$(git -C "$repo" ls-files -- "${dockerfile_specs[@]}")
for want_file in hack/e2e-toolchain/Dockerfile hack/e2e-toolchain/web.Dockerfile; do
	if grep -qxF "$want_file" <<<"$selected"; then echo "ok   dockerfile specs select $want_file"; else echo "FAIL dockerfile specs miss $want_file"; fail=1; fi
done

# #537: docker.io/ resolves to registry-1.docker.io (the Hub registry API host).
host() { # host <name> <input> <want>
	local got
	got=$(image_registry_host "$2")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
host "docker.io host" docker.io registry-1.docker.io
host "index.docker.io host" index.docker.io registry-1.docker.io
host "registry-1.docker.io unchanged" registry-1.docker.io registry-1.docker.io
host "ghcr.io unchanged" ghcr.io ghcr.io

# #537: OCI chart artifacts date from org.opencontainers.image.created; nothing dated fails closed.
ann() { # ann <name> <json> <want first line or empty>
	local got
	got=$(oci_annotation_created "$2" | head -1)
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
ann "helm chart annotation" \
	'{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"org.opencontainers.image.created":"2026-11-03T19:22:02Z"},"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:abc","size":1}}' \
	"2026-11-03T19:22:02Z"
ann "index descriptor annotation" \
	'{"schemaVersion":2,"manifests":[{"digest":"sha256:abc","annotations":{"org.opencontainers.image.created":"2026-01-02T03:04:05Z"}}]}' \
	"2026-01-02T03:04:05Z"
ann "no created annotation" \
	'{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:abc","size":1}}' \
	""

# A found image reference that cannot be dated must FAIL (report unknown), never exit without a line.
# Stub the network helpers and exercise check_image offline (#537). Digests are assembled so this
# file never contains a literal name@sha256:<64 hex> that the live dep-age scan would try to date.
fake_digest=sha256:$(printf 'a%.0s' {1..64})
fake_chart_ref="ghcr.io/example/helm-chart@$fake_digest"

run_check_image() { # run_check_image <manifest-json> [ref] -> stdout of check_image + counters
	local manifest=$1
	local ref=${2:-$fake_chart_ref}
	local frag=$tmp/check_image_frag.sh
	cat >"$frag" <<EOF
#!/usr/bin/env bash
set -euo pipefail
MIN_AGE_DAYS=3
WAIVER=
fails=0
checked=0
report() {
	local what=\$1 age=\$2 src=\$3 note=\${4:-} status=OK
	checked=\$((checked + 1))
	[[ \$age == unknown ]] && status=FAIL
	[[ \$status == FAIL ]] && fails=\$((fails + 1))
	printf '%s %s age=%s %s\\n' "\$status" "\$what" "\$age" "\$src"
	return 0
}
age_days() { echo 9.9; }
image_manifest() { printf 'host=%s repo=%s\\n' "\$1" "\$2" >&2; printf '%s' $(printf '%q' "$manifest"); }
image_blob() { printf '%s' '{"name":"chart","version":"0.0.0"}'; }
image_registry_published() { return 1; }
EOF
	sed -n \
		'/^image_registry_host() {/,/^}/p; /^oci_annotation_created() {/,/^}/p; /^consider_created() {/,/^}/p; /^check_image() {/,/^}/p' \
		"$repo/hack/dep-age-check.sh" >>"$frag"
	{
		printf 'check_image %q\n' "$ref"
		echo 'echo "checked=$checked"'
		echo 'echo "fails=$fails"'
	} >>"$frag"
	bash "$frag" 2>&1
}

out=$(run_check_image '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:dead","size":1},"layers":[]}') || true
if grep -q '^FAIL ' <<<"$out" && grep -q 'checked=1' <<<"$out" && grep -q 'fails=1' <<<"$out"; then
	echo "ok   undatable image reference fails closed (checked=1)"
else
	echo "FAIL undatable image reference: $out"
	fail=1
fi

out=$(run_check_image '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"org.opencontainers.image.created":"2026-01-01T00:00:00Z"},"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"sha256:dead","size":1},"layers":[]}') || true
if grep -q '^OK ' <<<"$out" && grep -q 'annotation' <<<"$out" && grep -q 'checked=1' <<<"$out" && grep -q 'fails=0' <<<"$out"; then
	echo "ok   OCI chart annotation dates the reference"
else
	echo "FAIL OCI chart annotation: $out"
	fail=1
fi

# #537: docker.io/<official> and bare <official> both resolve to library/<name> on the Hub API host.
hub_manifest='{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"org.opencontainers.image.created":"2026-01-01T00:00:00Z"},"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:dead","size":1},"layers":[]}'
hub_path() { # hub_path <name> <ref> <want host=… repo=…>
	local out
	out=$(run_check_image "$hub_manifest" "$2") || true
	if grep -qF "$3" <<<"$out" && grep -q '^OK ' <<<"$out"; then
		echo "ok   $1"
	else
		echo "FAIL $1: want '$3' in: $out"
		fail=1
	fi
}
hub_path "bare alpine → library/alpine" "alpine@$fake_digest" "host=registry-1.docker.io repo=library/alpine"
hub_path "docker.io/alpine → library/alpine" "docker.io/alpine@$fake_digest" "host=registry-1.docker.io repo=library/alpine"
hub_path "docker.io/library/alpine unchanged path" "docker.io/library/alpine@$fake_digest" "host=registry-1.docker.io repo=library/alpine"
hub_path "docker.io/myorg/app keeps org path" "docker.io/myorg/app@$fake_digest" "host=registry-1.docker.io repo=myorg/app"

[[ $fail -eq 0 ]] && echo "dep-age-check tests passed"
exit "$fail"
