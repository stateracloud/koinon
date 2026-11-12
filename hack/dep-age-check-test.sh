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
	'/^image_location() {/,/^}/p; /^expand_vars() {/,/^}/p; /^pm_pin() {/,/^}/p; /^snapshot_pin() {/,/^}/p; /^check_snapshot() {/,/^}/p; /^age_days() {/,/^}/p; /^report() {/,/^}/p; /^dockerfile_specs=/p; /^oci_annotation_created() {/,/^}/p' \
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

# The Ubuntu snapshot ARG (issue 33): the line is recognised, and its own timestamp is its age.
snap() { # snap <name> <line> <expected ISO time or empty>
	local got
	got=$(snapshot_pin "$2")
	if [[ $got == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$got', want '$3'"; fail=1; fi
}
snap "snapshot ARG" 'ARG UBUNTU_SNAPSHOT=20261027T000000Z' "2026-10-27T00:00:00Z"
snap "snapshot ARG, indented" '  ARG UBUNTU_SNAPSHOT=20261108T123456Z  ' "2026-11-08T12:34:56Z"
snap "not a snapshot ARG" 'ARG OTHER_SNAPSHOT=20261027T000000Z' ""
snap "malformed snapshot ID" 'ARG UBUNTU_SNAPSHOT=2026-10-27' ""
snap "snapshot ID without the Z" 'ARG UBUNTU_SNAPSHOT=20261027T000000' ""

# report/age_days end to end: an old snapshot passes, one younger than 3 days (or from the future) fails.
fails_is() { # fails_is <n> <ok message> <FAIL message>
	if [[ $fails -eq $1 ]]; then echo "$2"; else echo "$3"; fail=1; fi
}
# shellcheck disable=SC2034 # globals read by the eval-ed report()
fails=0 checked=0 WAIVER='' MIN_AGE_DAYS=3
check_snapshot "2026-10-27T00:00:00Z" 20261027T000000Z >/dev/null
fails_is 0 "ok   an old snapshot passes" "FAIL an old snapshot was rejected"
young=$(date -u -d '1 day ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1d +%Y-%m-%dT%H:%M:%SZ)
check_snapshot "$young" young >/dev/null
fails_is 1 "ok   a snapshot 1 day old fails" "FAIL a snapshot 1 day old passed"
check_snapshot "2099-01-01T00:00:00Z" future >/dev/null
fails_is 2 "ok   a snapshot from the future fails" "FAIL a future snapshot passed"
# shellcheck disable=SC2034
WAIVER=someone
check_snapshot "$young" young >/dev/null
fails_is 2 "ok   the waiver turns a young snapshot into WAIVED" "FAIL the waiver did not apply"

# The Dockerfile pathspecs must select every Dockerfile, named ones included.
# shellcheck disable=SC2154 # defined by the eval above
selected=$(git -C "$repo" ls-files -- "${dockerfile_specs[@]}")
for want_file in hack/e2e-toolchain/Dockerfile hack/e2e-toolchain/web.Dockerfile; do
	if grep -qxF "$want_file" <<<"$selected"; then echo "ok   dockerfile specs select $want_file"; else echo "FAIL dockerfile specs miss $want_file"; fail=1; fi
done

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
		'/^image_location() {/,/^}/p; /^oci_annotation_created() {/,/^}/p; /^consider_created() {/,/^}/p; /^check_image() {/,/^}/p' \
		"$repo/hack/dep-age-check.sh" >>"$frag"
	{
		printf 'check_image %q\n' "$ref"
		# Literal $checked/$fails for the frag; expand when the frag runs.
		cat <<'FRAGTAIL'
echo "checked=$checked"
echo "fails=$fails"
FRAGTAIL
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

# An oci:// chart reference is read like an image reference, and docker.io names reach the Hub API.
out=$(run_check_image '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"org.opencontainers.image.created":"2026-01-01T00:00:00Z"},"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:dead","size":1},"layers":[]}' "oci://docker.io/alpine@$fake_digest") || true
if grep -q '^OK ' <<<"$out" && grep -qF 'host=registry-1.docker.io repo=library/alpine' <<<"$out"; then
	echo "ok   oci:// prefix is stripped and docker.io/alpine resolves through image_location"
else
	echo "FAIL oci:// prefix: $out"
	fail=1
fi

# image_location: Docker Hub names reach registry-1.docker.io whatever their prefix (docker.io serves a web page).
loc() { # loc <name> <expected "host repo">
	local got
	got=$(image_location "$1")
	if [[ $got == "$2" ]]; then echo "ok   image_location $1"; else echo "FAIL image_location $1: got '$got', want '$2'"; fail=1; fi
}
loc registry "registry-1.docker.io library/registry"
loc docker.io/library/registry "registry-1.docker.io library/registry"
loc docker.io/registry "registry-1.docker.io library/registry"
loc index.docker.io/bitnami/redis "registry-1.docker.io bitnami/redis"
loc bitnami/redis "registry-1.docker.io bitnami/redis"
loc ghcr.io/o/r "ghcr.io o/r"
loc localhost:5001/x "localhost:5001 x"
loc docker.io/alpine "registry-1.docker.io library/alpine"
loc index.docker.io/alpine "registry-1.docker.io library/alpine"
loc alpine "registry-1.docker.io library/alpine"
loc docker.io/myorg/app "registry-1.docker.io myorg/app"

[[ $fail -eq 0 ]] && echo "dep-age-check tests passed"
exit "$fail"
