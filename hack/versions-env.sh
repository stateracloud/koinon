#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prints the NAME=value lines of hack/versions.env that a workflow may load into $GITHUB_ENV:
#   hack/versions-env.sh >> "$GITHUB_ENV"
# Only <TOOL>_VERSION and <TOOL>_SHA256_AMD64/ARM64 names pass, so a line cannot set BASH_ENV, PATH or
# any other variable the runner acts on. hack/versions-check.sh fails on any line this filter drops.
# The one filter for every workflow loader (old issue 544).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

grep -E '^[A-Z][A-Z0-9_]*_(VERSION|SHA256_(AMD64|ARM64))=[A-Za-z0-9._+-]+$' "$(dirname "$0")/versions.env"
