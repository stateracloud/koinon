#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Print the docker build arguments that set a harness's base image from HARNESS_BASE (#200), one per
# line: nothing when HARNESS_BASE is unset or empty (the Dockerfile's own `ARG BASE=` default is the
# one source of the public ref and its digest pin), else `--build-arg` and `BASE=<HARNESS_BASE>`.
# Used by the Makefile's harness targets and test/e2e/run.sh.
#
# HARNESS_BASE names the same image elsewhere, for example through a registry mirror on a network
# that cannot reach the public registry. It must carry the digest the Dockerfile pins (a mirror
# serves the identical digest), so the base never changes without a change to the Dockerfile:
# a ref with another digest, or with none, exits 1. No mirror address is in this repository; the
# machine that needs one sets HARNESS_BASE from its own configuration.
#
# Usage: HARNESS_BASE=<ref>@sha256:<digest> hack/harness-base.sh <harness>
# Exit: 0 printed (or nothing to print), 1 HARNESS_BASE refused, 2 usage or no pinned default.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

harness=${1:-}
[ -n "$harness" ] || { echo "usage: harness-base.sh <harness>" >&2; exit 2; }
cd "$(dirname "$0")/.."
dockerfile=harnesses/$harness/Dockerfile
[ -f "$dockerfile" ] || { echo "harness-base.sh: no such harness: $harness" >&2; exit 2; }

base=${HARNESS_BASE:-${FLAVOR_BASE:-}} # FLAVOR_BASE: the earlier name, read until #610 is done
[ -n "$base" ] || exit 0

default=$(sed -n 's/^ARG BASE=//p' "$dockerfile" | head -n 1)
pin=""
[[ $default =~ @(sha256:[0-9a-f]{64})$ ]] && pin=${BASH_REMATCH[1]}
[ -n "$pin" ] || { echo "harness-base.sh: $dockerfile has no digest-pinned ARG BASE= default" >&2; exit 2; }

# The image reference grammar (github.com/distribution/reference, IPv6 hosts left out): an optional
# registry host[:port]/, lowercase path components, an optional :tag, then @sha256:<64 hex>. Only
# those characters pass, so the printed arguments are safe to word-split unquoted (the Makefile
# does): no glob character, space or shell metacharacter can reach a shell (#203 review).
dc='[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?'
pc='[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*'
ref_re="^(${dc}(\.${dc})*(:[0-9]+)?/)?${pc}(/${pc})*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}\$"
if ! [[ $base =~ $ref_re ]]; then
  echo "harness-base.sh: HARNESS_BASE is no image reference with a digest ([host[:port]/]path[:tag]@sha256:<64 hex>): $base" >&2
  exit 1
fi
if [[ $base != *"@$pin" ]]; then
  echo "harness-base.sh: HARNESS_BASE must end with the digest $dockerfile pins (@$pin): $base" >&2
  exit 1
fi
printf '%s\n' --build-arg "BASE=$base"
