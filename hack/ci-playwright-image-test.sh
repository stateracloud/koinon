#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# #569: the required `web-e2e` job runs in the official Playwright image, by digest, and no
# workflow installs OS packages from a live apt mirror on every run. A run of PR #568 failed at
# `playwright install --with-deps` when security.ubuntu.com did not resolve.
#   - the image tag carries exactly the @playwright/test version of web/package.json
#   - the image is pinned by sha256 digest
#   - no workflow runs `--with-deps`, `playwright install-deps` or `apt-get install` in a step
#     (workflow files are searched; a Dockerfile's apt is out of scope here: it is built only on change,
#     and apt-retries-test.sh checks its retry settings)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wfdir=${1:-$repo/.github/workflows}
pkg=${2:-$repo/web/package.json}
pin=${3:-$repo/hack/playwright-image.env}
wdf=${4:-$repo/hack/e2e-toolchain/web.Dockerfile}

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }

job_body() { awk -v j="  $2:" '$0 == j { f = 1; next } f && /^  [a-zA-Z0-9_-]+:/ { exit } f' "$1"; }

ver=$(jq -r '.devDependencies["@playwright/test"] // .dependencies["@playwright/test"]' "$pkg")
body=$(job_body "$wfdir/ci.yml" web-e2e)
# The one pin (hack/playwright-image.env): both workflows read it, so a bump touches no .github path.
reader=$repo/hack/playwright-image.sh
image=$("$reader" "$pin" 2>/dev/null || true)

check "the pin file sets PLAYWRIGHT_IMAGE to the Playwright image" test -n "$image"
# The reader fails closed (nothing sources the pin file): negative cases.
# Built at run time: a full image@sha256:<64 hex> literal in a file would be read as a new pin by hack/dep-age-check.sh.
zero_digest=$(printf '%064d' 0)
good_line="PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v1.2.3-noble@sha256:$zero_digest"
pinfx=$(mktemp -d "${TMPDIR:-/tmp}/ci-playwright-pin.XXXXXX") || exit 1
# shellcheck disable=SC2329 # called through check
reads() { printf '%s\n' "$2" >"$pinfx/$1.env"; "$reader" "$pinfx/$1.env"; }
# shellcheck disable=SC2329
rejects() { ! reads "$@"; }
check "reader accepts a comment, an empty line and one good line" reads good $'# c\n\n'"$good_line"
check "reader rejects an extra export line" rejects export "$good_line"$'\nexport PLAYWRIGHT_IMAGE=docker.io/evil/x:latest'
check "reader rejects a second PLAYWRIGHT_IMAGE line" rejects second "$good_line"$'\n'"$good_line"
check "reader rejects an unpinned image" rejects tag 'PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v1.2.3-noble'
check "reader rejects another registry" rejects reg "${good_line/mcr.microsoft.com/docker.io\/x}"
# shellcheck disable=SC2016 # literal on purpose
check "reader rejects a command substitution in the line" rejects sub "$good_line"'$(id)'
check "reader rejects a file with no setting line" rejects empty '# only a comment'
rm -rf "$pinfx"

# shellcheck disable=SC2329 # called through check
tag_matches() { [[ $image == mcr.microsoft.com/playwright:v"$ver"-*@sha256:* ]]; }
check "image tag is v$ver-<codename> (the exact @playwright/test version)" tag_matches
# shellcheck disable=SC2329
digest_pinned() { [[ $image =~ @sha256:[0-9a-f]{64}$ ]]; }
check "image is pinned by digest" digest_pinned
# shellcheck disable=SC2329
dockerfile_matches() { [[ $(sed -n 's/^ARG PLAYWRIGHT_VERSION=//p' "$wdf") == "$ver" ]]; }
check "web.Dockerfile ARG PLAYWRIGHT_VERSION is $ver" dockerfile_matches
# shellcheck disable=SC2329
web_e2e_uses_pin() { grep -Eq '^ *image: *\$\{\{ *needs\.changes\.outputs\.playwright_image *\}\}( +#.*)?$' <<<"$body" && ! grep -Eq 'mcr\.microsoft\.com/playwright' <<<"$body"; }
# shellcheck disable=SC2329
no_source() { ! grep -Eq '^[^#]*(\.|source) +(\./)?hack/playwright-image\.env' "$wfdir"/*.yml; }
check "no workflow sources the pin file (hack/playwright-image.sh is the one reader)" no_source
check "web-e2e takes its image from the changes job output (the pin file), not a literal" web_e2e_uses_pin
# shellcheck disable=SC2016
check "web-e2e installs no browser or OS packages" bash -c '! grep -Eq "playwright install|install-deps|--with-deps|apt-get" <<<"$1"' _ "$body"

# The restore drill job (e2e-kind.yml) needs kind on the host, so only its browser spec runs in the image
# (go tool drillwebrun): the job must read the same pin file, not carry a literal image.
if [ -e "$wfdir/e2e-kind.yml" ]; then
  # shellcheck disable=SC2329
  # shellcheck disable=SC2016 # the pattern is literal
  drill_reads_pin() {
    grep -q 'hack/playwright-image.sh' "$wfdir/e2e-kind.yml" && grep -q 'KOINON_DRILL_PLAYWRIGHT_IMAGE=\$image' "$wfdir/e2e-kind.yml" \
      && ! grep -Eq '^ *[A-Z_]+: *mcr\.microsoft\.com/playwright' "$wfdir/e2e-kind.yml"
  }
  check "the drill job's Playwright image comes from the pin file (same as web-e2e)" drill_reads_pin
fi

# Every other place: a workflow step that installs OS packages from a live mirror. Options may take
# a separate value (`apt-get -o Acquire::Retries=5 install`); comment lines are not hits.
scan() {
  local d=$1 f=() g
  for g in "$d"/*.yml "$d"/*.yaml; do [ -e "$g" ] && f+=("$g"); done
  [ "${#f[@]}" -gt 0 ] || return 0
  grep -HnE '(--with-deps|playwright install-deps|apt(-get)?( +-[^ ]+( +[^- ][^ ]*)?)* +install)' "${f[@]}" | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' || true
}
hits=$(scan "$wfdir")
check "no workflow step installs OS packages from a live apt mirror ($hits)" test -z "$hits"

# Self-test of the scan on a fixture directory.
fx=$(mktemp -d "${TMPDIR:-/tmp}/ci-playwright-scan.XXXXXX") || exit 1
trap 'rm -rf "$fx"' EXIT
printf 'jobs:\n  a:\n    steps:\n      - run: sudo apt-get install -y xz-utils\n' >"$fx/one.yaml"
check "scan catches apt-get install in a .yaml workflow" test -n "$(scan "$fx")"
rm "$fx/one.yaml"
printf 'jobs:\n  a:\n    steps:\n      - run: sudo apt-get -o Acquire::Retries=5 install -y xz-utils\n' >"$fx/two.yml"
check "scan catches apt-get -o K=V install" test -n "$(scan "$fx")"
printf '      - run: sudo apt -o Acquire::Retries=5 install -y jq\n' >"$fx/two.yml"
check "scan catches apt -o K=V install" test -n "$(scan "$fx")"
printf '      # sudo apt-get install -y xz-utils\n      - run: apt-get update\n' >"$fx/two.yml"
check "scan ignores a comment line" test -z "$(scan "$fx")"
rm "$fx/two.yml"
check "scan of a directory with no workflow files is not an error" test -z "$(scan "$fx")"

echo "ci-playwright-image-test: $pass ok, $fail failed"
[ "$fail" -eq 0 ]
