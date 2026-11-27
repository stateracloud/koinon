#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the Playwright image of hack/playwright-image.env (#1399). The one reader: ci.yml, e2e-kind.yml and
# the tests call it, and nothing sources the file. Fails closed: the file must hold exactly one line that is
# not empty and not a comment, in the form
#   PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v<ver>-<codename>@sha256:<64 hex>
# Usage: hack/playwright-image.sh [file]

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
f=${1:-$(dirname "${BASH_SOURCE[0]}")/playwright-image.env}
[[ -f $f ]] || { echo "playwright-image: no file $f" >&2; exit 1; }
n=$(grep -cvE '^[[:space:]]*(#.*)?$' "$f" || true)
[[ $n -eq 1 ]] || { echo "playwright-image: $f must hold exactly one setting line, found $n" >&2; exit 1; }
line=$(grep -vE '^[[:space:]]*(#.*)?$' "$f")
re='^PLAYWRIGHT_IMAGE=(mcr\.microsoft\.com/playwright:v[0-9]+\.[0-9]+\.[0-9]+-[a-z]+@sha256:[0-9a-f]{64})$'
[[ $line =~ $re ]] || { echo "playwright-image: $f line is not PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v<ver>-<codename>@sha256:<digest>" >&2; exit 1; }
printf '%s\n' "${BASH_REMATCH[1]}"
