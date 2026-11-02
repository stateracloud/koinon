#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# mktemp lint (#535): every mktemp call carries a template, `mktemp -d "${TMPDIR:-/tmp}/name.XXXXXX"`.
# A bare `mktemp`, `mktemp -d`, `mktemp -q` or `mktemp -t x` writes to /var/folders on macOS (it ignores
# TMPDIR), which the Claude Code sandbox blocks. Scans hack/**/*.sh, test/**/*.sh, the Makefile and
# .github/workflows/*.yml; comment lines are ignored and a call is fine when the rest of its line has an
# XXX... template.
#
# Usage: hack/mktemp-lint.sh [root]    (default: the repository root)
set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}

# Files still carrying bare calls because #512 is changing them. Empty this list after #512 merges (#535).
excluded=(
	hack/e2e-remote.sh      # #535: bare mktemp in the remote command; fix after #512 merges
	hack/e2e-remote-test.sh # #535: bare mktemp -d; fix after #512 merges
)

is_excluded() {
	local e
	for e in ${excluded[@]+"${excluded[@]}"}; do [[ $1 == "$e" ]] && return 0; done
	return 1
}

fail=0
n=0
while IFS= read -r f; do
	rel=${f#"$root"/}
	is_excluded "$rel" && continue
	[[ $rel == hack/mktemp-lint-test.sh ]] && continue # its fixtures are bare calls on purpose
	n=$((n + 1))
	# A command position: start of line, after $( or a backtick, after ; | & ( or =, or after a space
	# that follows one of those words. Words like "rm mktemp tr" or a path ending in /mktemp do not match.
	hits=$(awk -v file="$rel" '
		/^[[:space:]]*#/ { next }
		{
			line = $0
			while (match(line, /(^|[;|&(=`"[:space:]])mktemp([[:space:]]|\)|`|;|$)/)) {
				lead = (RSTART == 1 && substr(line, 1, 1) == "m") ? "" : substr(line, RSTART, 1)
				before = substr(line, 1, RSTART - 1)
				rest = substr(line, RSTART + RLENGTH - 1)
				cmd = 0
				if (lead == "" || lead ~ /[(`;|&=]/) cmd = 1
				else if (lead ~ /[[:space:]]/) {
					sub(/[[:space:]]+$/, "", before)
					if (before == "" || before ~ /(;|\||&|\(|`|then|do|else)$/) cmd = 1
				}
				if (lead == "(" && before ~ /\\\$$/) cmd = 0 # an escaped \$( in a string
				if (cmd && rest !~ /XXX/) printf "%s:%d: %s\n", file, NR, $0
				line = substr(line, RSTART + RLENGTH)
			}
		}' "$f")
	if [[ -n $hits ]]; then
		echo "$hits" >&2
		fail=1
	fi
done < <(
	{
		[[ -d $root/hack ]] && find "$root/hack" -name '*.sh'
		[[ -d $root/test ]] && find "$root/test" -name '*.sh'
		[[ -f $root/Makefile ]] && echo "$root/Makefile"
		[[ -d $root/.github/workflows ]] && find "$root/.github/workflows" -name '*.yml'
		true
	} | sort
)

if ((fail)); then
	# shellcheck disable=SC2016 # the hint shows a literal ${TMPDIR}
	echo 'mktemp-lint: give every mktemp a template, e.g. mktemp -d "${TMPDIR:-/tmp}/<script>.XXXXXX" (macOS mktemp ignores TMPDIR without one; #535)' >&2
	exit 1
fi
echo "mktemp-lint: $n files, every mktemp has a template"
