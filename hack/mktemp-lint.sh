#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# mktemp lint (old issue 535): every mktemp call carries a template, `mktemp -d "${TMPDIR:-/tmp}/name.XXXXXX"`.
# A bare `mktemp`, `mktemp -d`, `mktemp -q` or `mktemp -t x` writes to /var/folders on macOS (it ignores
# TMPDIR), which the Claude Code sandbox blocks. Scans hack/**/*.sh, test/**/*.sh, the Makefile and
# .github/workflows/*.yml; comment lines are ignored. A call is fine when its own arguments (up to the end
# of the call: ) ` ; | & or a trailing comment; a backslash continuation is joined) carry a template of at
# least six X that is not a hard-coded absolute path. -t, -p and --tmpdir are flagged unless the template is
# a variable path ($tmp/..., ${TMPDIR...}/...): macOS ignores TMPDIR with -t and --tmpdir is GNU-only.
# Also scans internal/**/*.sh and internal/**/*.go (scripts embedded in Go raw strings), because
# internal/agentd/savework.sh runs under the macOS sandbox in agent lanes' specs (issue 358). Out of scope:
# snippets in docs.
#
# Usage: hack/mktemp-lint.sh [root]    (default: the repository root)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}

fail=0
n=0
while IFS= read -r f; do
	rel=${f#"$root"/}
	[[ $rel == hack/mktemp-lint-test.sh ]] && continue # its fixtures are bare calls on purpose
	n=$((n + 1))
	# A command position: start of line, after $( or a backtick, after ; | & ( or =, or after a space
	# that follows one of those words. Words like "rm mktemp tr" or a path ending in /mktemp do not match.
	hits=$(awk -v file="$rel" '
		# 1 when the call whose text follows "mktemp" has no usable template
		function bad(rest,   n, i, t, tok, tmpl, c, q, len) {
			# cut at the end of the call, but not inside double quotes
			q = 0
			len = length(rest)
			for (i = 1; i <= len; i++) {
				c = substr(rest, i, 1)
				if (c == "\"") q = !q
				else if (!q && (c == ")" || c == ";" || c == "|" || c == "&" || c == "`")) break
				else if (!q && (c == " " || c == "\t") && substr(rest, i + 1, 1) == "#") break
			}
			rest = substr(rest, 1, i - 1)
			n = split(rest, tok, /[[:space:]]+/)
			tmpl = ""
			for (i = 1; i <= n; i++) {
				t = tok[i]
				# -t fails or ignores TMPDIR on macOS, --tmpdir is GNU-only and -p is needless with a template
				if (t ~ /^-[a-zA-Z]*[tp][a-zA-Z]*$/ || t ~ /^--tmpdir/) return 1
				if (tmpl == "" && t ~ /XXXXXX/) tmpl = t
			}
			if (tmpl == "") return 1
			sub(/^["\047]+/, "", tmpl)
			sub(/["\047]+$/, "", tmpl)
			if (tmpl !~ /XXXXXX$/) return 1 # BSD mktemp only replaces trailing X
			if (tmpl ~ /^\//) return 1 # a hard-coded absolute path, not under ${TMPDIR...}
			return 0
		}
		function scan(line,   rs, rl, lead, before, rest, cmd) {
			while (match(line, /(^|[;|&(=`"[:space:]])mktemp([[:space:]]|\)|`|;|\||&|$)/)) {
				rs = RSTART
				rl = RLENGTH # bad() calls match(), which resets RSTART and RLENGTH
				lead = (RSTART == 1 && substr(line, 1, 1) == "m") ? "" : substr(line, RSTART, 1)
				before = substr(line, 1, RSTART - 1)
				rest = substr(line, RSTART + RLENGTH - 1)
				cmd = 0
				if (lead == "" || lead ~ /[(`;|&=]/) cmd = 1
				else if (lead ~ /[[:space:]]/) {
					sub(/[[:space:]]+$/, "", before)
					if (before == "" || before ~ /(;|\||&|\(|`|then|do|else|command|env|xargs)$/) cmd = 1
				}
				if (before == "" && substr(rest, 1, 1) == ")") cmd = 0 # a case label: mktemp) ...
				if (lead == "(" && before ~ /\\\$$/) cmd = 0 # an escaped \$( in a string
				if (cmd && bad(rest)) printf "%s:%d: %s\n", file, start, first
				line = substr(line, rs + rl)
			}
		}
		buf == "" && /^[[:space:]]*#/ { next }
		{
			sub("\015$", "")
			if (buf == "") { start = NR; first = $0 }
			if ($0 ~ /\\$/) { buf = buf substr($0, 1, length($0) - 1) " "; next }
			scan(buf $0)
			buf = ""
		}
		END { if (buf != "") scan(buf) }
' "$f")
	if [[ -n $hits ]]; then
		echo "$hits" >&2
		fail=1
	fi
done < <(
	{
		[[ -d $root/hack ]] && find "$root/hack" -name '*.sh'
		[[ -d $root/test ]] && find "$root/test" -name '*.sh'
		[[ -d $root/internal ]] && grep -rl --include='*.sh' --include='*.go' mktemp "$root/internal" || true
		[[ -f $root/Makefile ]] && echo "$root/Makefile"
		[[ -d $root/.github/workflows ]] && find "$root/.github/workflows" -name '*.yml'
		true
	} | sort
)

if ((fail)); then
	# shellcheck disable=SC2016 # the hint shows a literal ${TMPDIR}
	echo 'mktemp-lint: give every mktemp a template, e.g. mktemp -d "${TMPDIR:-/tmp}/<script>.XXXXXX" (macOS mktemp ignores TMPDIR without one; old issue 535)' >&2
	exit 1
fi
echo "mktemp-lint: $n files, every mktemp has a template"
