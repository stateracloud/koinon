#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs one web/e2e-drill spec in the pinned Playwright image (#1039), so the drill job installs no browser and no OS
# package from a live mirror (hack/ci-playwright-image-test.sh, #569). The drill host (the CI runner) needs kind and
# the cluster port-forwards, so the job itself cannot run in the image; only the spec does. Usage:
#   KOINON_DRILL_PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v<ver>-noble@sha256:... hack/drill-web-run.sh <spec>
# test/drill runs it when KOINON_DRILL_WEB_RUNNER names it. web/node_modules comes from the host's
# `pnpm --dir web install --frozen-lockfile`; the image brings Chromium and its libraries (PLAYWRIGHT_BROWSERS_PATH).
# --network=host: the spec reaches the port-forwards on the host's 127.0.0.1. Variable values pass by name, never on
# the command line, so the passwords stay out of the process list.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail
spec=${1:?usage: drill-web-run.sh <spec>}
image=${KOINON_DRILL_PLAYWRIGHT_IMAGE:?KOINON_DRILL_PLAYWRIGHT_IMAGE is not set (the pinned Playwright image, by digest)}
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[[ $image == *@sha256:* ]] || { echo "drill-web-run: the image must be pinned by digest: $image" >&2; exit 2; }

envs=()
while IFS= read -r name; do envs+=(-e "$name"); done < <(
	env | cut -d= -f1 | grep -E '^(KOINON_DRILL_|PLAYWRIGHT_OUTPUT_DIR$|HTTPS?_PROXY$|NO_PROXY$|https?_proxy$|no_proxy$)' || true)

mounts=(-v "$repo:$repo")
if [[ -n ${PLAYWRIGHT_OUTPUT_DIR:-} ]]; then
	mkdir -p "$PLAYWRIGHT_OUTPUT_DIR"
	mounts+=(-v "$PLAYWRIGHT_OUTPUT_DIR:$PLAYWRIGHT_OUTPUT_DIR")
fi

exec docker run --rm --init --ipc=host --network=host \
	--user "$(id -u):$(id -g)" -e HOME=/tmp "${envs[@]}" "${mounts[@]}" -w "$repo/web" \
	"$image" node_modules/.bin/playwright test -c playwright.drill.config.ts "$spec"
