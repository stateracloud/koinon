#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Workflow trigger lint (#463): no workflow under .github/workflows uses the `pull_request_target`
# trigger. GitHub blocks it by default for public repositories (brownouts from 2026-11-09, permanent
# 2026-12-02), and it runs with the base branch's secrets and a write token next to pull request
# content. Use `pull_request` with a read-only token where no secret is needed, `push` or `schedule`
# for the write part. Adding it back needs an ADR that amends ADR 0082 and an admin Actions event
# policy; then exempt the workflow here by name. Comments are ignored.
#
# Usage: hack/workflow-trigger-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}

fail=0
n=0
while IFS= read -r f; do
	n=$((n + 1))
	# strip comments (a # at the start or after a space), then look for the trigger name anywhere
	hits=$(sed -E 's/(^|[[:space:]])#.*$//' "$f" | grep -nE '(^|[^A-Za-z0-9_])pull_request_target([^A-Za-z0-9_]|$)' || true)
	if [[ -n $hits ]]; then
		while IFS= read -r h; do echo "${f#"$root"/}:$h" >&2; done <<<"$hits"
		fail=1
	fi
done < <(find "$root/.github/workflows" -maxdepth 1 \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

if ((fail)); then
	echo "workflow-trigger-lint: pull_request_target is blocked by default on public repositories (#463, ADR 0082); use pull_request with a read-only token, or push/schedule for writes" >&2
	exit 1
fi
echo "workflow-trigger-lint: $n workflows, none uses pull_request_target"
