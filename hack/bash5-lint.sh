#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# bash 5 guard lint (ADR 0090, old issue 553): every `#!/usr/bin/env bash` script carries the guard line
#
#   [ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 ..." >&2; exit 2; }
#
# as its first command: the first line that is neither blank nor a comment. It must come first because
# an old bash (macOS ships 3.2) parses the file line by line and has to reach the guard before any
# bash 4+ syntax, and the guard itself uses only POSIX syntax. This lint runs under bash 5 only, like
# every other script; it checks the exact text, so the message cannot drift between scripts.
#
# Hooks: the registered hack/hooks/*-check.sh start with the maintainer opt-in line (gate below, issue 843;
# POSIX, so bash 3.2 reaches it) and carry the fail-open variant of the guard as their second command.
# Any other bash shebang (#!/bin/bash, env -S bash, ...) is rejected.
#
# Scope: every *.sh file under the root whose first line is `#!/usr/bin/env bash`. Not in scope, by
# design (see ADR 0090): files without a bash shebang (libraries that are only sourced, such as
# hack/chart-check.d/*.sh, which a guarded script
# loads) and scripts with a #!/bin/sh shebang (they run in container images with no bash).
#
# Usage: hack/bash5-lint.sh [root]    (default: the repository root)
# shellcheck disable=SC2016 # the guard text holds a literal ${BASH_VERSINFO...}

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

guard='[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon'"'"'s scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }'

# PreToolUse hooks (hack/hooks/*-check.sh) skip with a message instead: exit 2 would block the tool call.
hook_guard='[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '"'"'{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'"'"'; exit 0; }'
# Maintainer hooks do nothing unless KOINON_MAINTAINER_HOOKS=1 (docs/maintainers/hooks.md); they drain stdin first.
hook_gate='[ "${KOINON_MAINTAINER_HOOKS:-}" = 1 ] || { cat >/dev/null; exit 0; }'

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}

fail=0
n=0
while IFS= read -r f; do
	shebang=$(head -n 1 "$f")
	if [[ $shebang == '#!'*bash* && $shebang != '#!/usr/bin/env bash' ]]; then
		echo "bash5-lint: ${f#"$root"/}: bash shebang '$shebang' (use #!/usr/bin/env bash)" >&2
		fail=1
		continue
	fi
	[[ $shebang == '#!/usr/bin/env bash' ]] || continue
	n=$((n + 1))
	first=$(awk 'NR > 1 && NF && $0 !~ /^[[:space:]]*#/ { print; exit }' "$f")
	second=$(awk 'NR > 1 && NF && $0 !~ /^[[:space:]]*#/ && ++k == 2 { print; exit }' "$f")
	rel=${f#"$root"/}
	# Exactly the registered PreToolUse hooks. Add a new hook here when .claude/settings.json registers it.
	case $rel in
	hack/hooks/agent-spawn-check.sh | hack/hooks/bash-read-check.sh | hack/hooks/read-result-check.sh | hack/hooks/sleep-chain-check.sh) is_hook=1 ;;
	*) is_hook=0 ;;
	esac
	if [[ $is_hook == 1 ]]; then
		# hooks fail open: exit 2 would block the tool call (ADR 0090, point 6)
		if [[ $first != "$hook_gate" || $second != "$hook_guard" ]]; then
			echo "bash5-lint: $rel: a hook starts with the KOINON_MAINTAINER_HOOKS line, then the fail-open bash 5 guard" >&2
			fail=1
		fi
		continue
	fi
	if [[ $first != "$guard" ]]; then
		echo "bash5-lint: ${f#"$root"/}: the first command is not the bash 5 guard" >&2
		fail=1
	fi
done < <(
	{
		if [[ -e $root/.git ]]; then
			git -C "$root" ls-files --cached --others --exclude-standard -z -- '*.sh' | xargs -0 -I{} printf '%s/%s\n' "$root" {}
		else
			find "$root" \( -name .git -o -name node_modules \) -prune -o -name '*.sh' -type f -print
		fi
	} | sort
)

if ((fail)); then
	echo "bash5-lint: put this line before the first command of each script listed (ADR 0090):" >&2
	echo "$guard" >&2
	exit 1
fi
echo "bash5-lint: $n scripts, every one starts with the bash 5 guard"
