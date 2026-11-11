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

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }

job_body() { awk -v j="  $2:" '$0 == j { f = 1; next } f && /^  [a-zA-Z0-9_-]+:/ { exit } f' "$1"; }

ver=$(jq -r '.devDependencies["@playwright/test"] // .dependencies["@playwright/test"]' "$pkg")
body=$(job_body "$wfdir/ci.yml" web-e2e)
image=$(sed -n 's/^ *image: *\(mcr.microsoft.com\/playwright:.*\)$/\1/p' <<<"$body")

check "web-e2e runs in the Playwright image" test -n "$image"
# shellcheck disable=SC2329 # called through check
tag_matches() { [[ $image == mcr.microsoft.com/playwright:v"$ver"-*@sha256:* ]]; }
check "image tag is v$ver-<codename> (the exact @playwright/test version)" tag_matches
# shellcheck disable=SC2329
digest_pinned() { [[ $image =~ @sha256:[0-9a-f]{64}$ ]]; }
check "image is pinned by digest" digest_pinned
# shellcheck disable=SC2016
check "web-e2e installs no browser or OS packages" bash -c '! grep -Eq "playwright install|install-deps|--with-deps|apt-get" <<<"$1"' _ "$body"

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
fx=$(mktemp -d "${TMPDIR:-/tmp}/ci-playwright-scan.XXXXXX")
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
