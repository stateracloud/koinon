#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runner lint (#58, ADR 0047): Koinon CI runs on GitHub-hosted runners only. A self-hosted or Blacksmith
# runner on a public repository would run anyone's pull request on infrastructure we do not control, so
# this is a fixed rule, not a case-by-case decision. The script fails when a workflow (or the actionlint
# config) names the label `self-hosted` or a `blacksmith-*` label, or when a `runs-on:` value is an
# expression that reads `vars.`, `inputs.`, `secrets.`, `needs.`, `github.event.`, `github.head_ref`,
# `github.ref_name`, `github.actor` or `github.triggering_actor` (a label chosen at run
# time cannot be checked here; `github.event_name` and `matrix.` are fine). It checks the scalar form and the
# block form (`runs-on:` followed by `labels:`, `group:` or list lines). Limit: an expression that reaches a
# label some other way (a `fromJSON` of a matrix value, a reusable workflow's input passed as `runs-on`
# through another name) is not followed. Comment lines and trailing comments are ignored. Scans
# .github/workflows/*.yml, *.yaml and .github/actionlint.y*ml.
#
# On purpose, the bare words `blacksmith` and `self-hosted` are banned on any non-comment line of a workflow
# (a step name, an env value, a string), not only in `runs-on`: no legitimate use exists, and a narrower
# match would let a label slip through another key.
#
# Prior art: actionlint's `runner-label` rule accepts `self-hosted` as a known label and checks only
# for unknown ones, so it does not cover this; GitHub's own advice is "never use self-hosted runners with
# public repositories" (docs.github.com, "Self-hosted runners", security hardening).
#
# Usage: hack/runner-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}

fail=0
n=0
shopt -s nullglob
for f in "$root"/.github/workflows/*.yml "$root"/.github/workflows/*.yaml "$root"/.github/actionlint.yml "$root"/.github/actionlint.yaml; do
	[[ -f $f ]] || continue
	rel=${f#"$root"/}
	n=$((n + 1))
	hits=$(awk -v file="$rel" '
		function dyn(t) { return t ~ /\$\{\{[^}]*(vars\.|inputs\.|secrets\.|needs\.|github\.event\.|github\.head_ref|github\.ref_name|github\.actor|github\.triggering_actor)/ }
		{
			line = $0
			if (line ~ /^[ \t]*#/) next
			sub(/[ \t]+#.*$/, "", line) # trailing comment
			l = tolower(line)
			if (l ~ /^[ \t]*self-hosted-runner:/ || l ~ /(^|[^a-z0-9_-])self-hosted([^a-z0-9_-]|$)/ || l ~ /(^|[^a-z0-9_-])blacksmith(-[a-z0-9.-]*)?([^a-z0-9_-]|$)/)
				printf "%s:%d: self-hosted or Blacksmith runner: %s\n", file, NR, $0
			else if (l ~ /^[ \t-]*runs-on:/ && dyn(l))
				printf "%s:%d: runs-on label chosen at run time: %s\n", file, NR, $0
			else if (inblock && l !~ /^[ \t]*$/) {
				match(line, /^[ \t]*/)
				if (RLENGTH < blockind || (RLENGTH == blockind && line !~ /^[ \t]*-/)) inblock = 0
				else if (dyn(l))
					printf "%s:%d: runs-on label chosen at run time: %s\n", file, NR, $0
			}
			if (l ~ /^[ \t-]*runs-on:[ \t]*$/) {
				inblock = 1
				match(line, /^[ \t]*/)
				blockind = RLENGTH
			}
		}' "$f")
	if [[ -n $hits ]]; then
		printf '%s\n' "$hits"
		fail=1
	fi
done

if [[ $fail -ne 0 ]]; then
	echo "runner-lint: Koinon CI uses GitHub-hosted runners only (ADR 0047, docs/dev/ci.md#workflow-security-review)" >&2
	exit 1
fi
echo "runner-lint: $n files, no self-hosted or Blacksmith runner"
