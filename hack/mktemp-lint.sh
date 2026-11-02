#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# mktemp lint (#535): every mktemp call carries a template, `mktemp -d "${TMPDIR:-/tmp}/name.XXXXXX"`.
# A bare `mktemp`, `mktemp -d`, `mktemp -q` or `mktemp -t x` writes to /var/folders on macOS (it ignores
# TMPDIR), which the Claude Code sandbox blocks. Scans hack/**/*.sh, test/**/*.sh, the Makefile and
# .github/workflows/*.yml; comment lines are ignored. A call is fine when its own arguments (up to the end
# of the call: ) ` ; | & or a trailing comment; a backslash continuation is joined) carry a template of at
# least six X that is not a hard-coded absolute path. -t, -p and --tmpdir are flagged unless the template is
# a variable path ($tmp/..., ${TMPDIR...}/...): macOS ignores TMPDIR with -t and --tmpdir is GNU-only.
# Out of scope: internal/agentd/savework.sh (it runs inside the Linux sandbox, where a bare mktemp is right)
# and snippets in docs.
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
		# 1 when the call whose text follows "mktemp" has no usable template
		function bad(rest,   n, i, t, tok, tmpl, badopt) {
			if (match(rest, /[);|&`]|[[:space:]]#/)) rest = substr(rest, 1, RSTART - 1)
			n = split(rest, tok, /[[:space:]]+/)
			tmpl = ""
			badopt = 0
			for (i = 1; i <= n; i++) {
				t = tok[i]
				if (t ~ /^-[a-zA-Z]*[tp][a-zA-Z]*$/ || t ~ /^--tmpdir/) badopt = 1 # -t, -p, --tmpdir
				else if (tmpl == "" && t ~ /XXXXXX/) tmpl = t
			}
			if (tmpl == "") return 1
			sub(/^["\047]+/, "", tmpl)
			if (tmpl ~ /^\//) return 1 # a hard-coded absolute path, not under ${TMPDIR...}
			if (badopt && tmpl !~ /^\$/) return 1 # -t/-p/--tmpdir do not honour TMPDIR on every platform
			return 0
		}
		buf == "" && /^[[:space:]]*#/ { next }
		{
			if (buf == "") { start = NR; first = $0 }
			if ($0 ~ /\\$/) { buf = buf substr($0, 1, length($0) - 1) " "; next }
			line = buf $0
			buf = ""
			# A command position: start of line, after $( or a backtick, after ; | & ( or =, or after a space
			# that follows one of those words. Words like "rm mktemp tr" or a path ending in /mktemp do not match.
			while (match(line, /(^|[;|&(=`"[:space:]])mktemp([[:space:]]|\)|`|;|$)/)) {
				rs = RSTART
				rl = RLENGTH # bad() calls match(), which resets RSTART and RLENGTH
				lead = (RSTART == 1 && substr(line, 1, 1) == "m") ? "" : substr(line, RSTART, 1)
				before = substr(line, 1, RSTART - 1)
				rest = substr(line, RSTART + RLENGTH - 1)
				cmd = 0
				if (lead == "" || lead ~ /[(`;|&=]/) cmd = 1
				else if (lead ~ /[[:space:]]/) {
					sub(/[[:space:]]+$/, "", before)
					if (before == "" || before ~ /(;|\||&|\(|`|then|do|else|command)$/) cmd = 1
				}
				if (before == "" && substr(rest, 1, 1) == ")") cmd = 0 # a case label: mktemp) ...
				if (lead == "(" && before ~ /\\\$$/) cmd = 0 # an escaped \$( in a string
				if (cmd && bad(rest)) printf "%s:%d: %s\n", file, start, first
				line = substr(line, rs + rl)
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
