#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): the main session's relay hook calls it. It only reads, so it works from a fork.
#
# Relay classifier (issue 1488): says in one line whether a GitHub event of a pull request is news for the
# main session, so the GitHub-to-Slack relay hook starts a model turn only on news. The logic is Go:
# hack/cmd/relayclass (ADR 0118), a main-module tool that `go tool` runs, so its exit code passes
# through (0 a verdict on stdout, 2 usage or read error). How the hook calls it:
# docs/maintainers/hooks.md, "The relay classifier".
#
# Usage: hack/relay-classify.sh [--repo OWNER/REPO] [--since RFC3339] [--required FILE] <PR number | comment URL>
# Prints one of: STOP, question, red required check, READY at <sha>, draft, nothing new.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
# The one path argument: resolve it against the caller's directory before the cd (ADR 0118).
args=()
while [[ $# -gt 0 ]]; do
  case $1 in
  --required)
    [[ $# -ge 2 ]] || { echo "relay-classify: --required needs a file" >&2; exit 2; }
    f=$2
    [[ $f == /* ]] || f=$PWD/$f
    args+=(--required "$f")
    shift 2
    ;;
  --required=*)
    f=${1#--required=}
    [[ $f == /* ]] || f=$PWD/$f
    args+=(--required "$f")
    shift
    ;;
  *) args+=("$1"); shift ;;
  esac
done
# `go tool` finds the tool through the module of the working directory.
cd "$repo"
exec go tool relayclass "${args[@]}"
