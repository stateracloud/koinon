#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# bash 5 guard lint (ADR 0090, #553): every `#!/usr/bin/env bash` script carries the guard line
#
#   [ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 ..." >&2; exit 2; }
#
# as its first command: the first line that is neither blank nor a comment. It must come first because
# an old bash (macOS ships 3.2) parses the file line by line and has to reach the guard before any
# bash 4+ syntax, and the guard itself uses only POSIX syntax. This lint runs under bash 5 only, like
# every other script; it checks the exact text, so the message cannot drift between scripts.
#
# Scope: every *.sh file under the root whose first line is `#!/usr/bin/env bash`. Not in scope, by
# design (see ADR 0090): files without a bash shebang (libraries that are only sourced, such as
# hack/lab/config.sh, hack/chart-check.d/*.sh and test/e2e/quickstart.golden.sh, which a guarded script
# loads) and scripts with a #!/bin/sh shebang (they run in container images with no bash).
#
# Usage: hack/bash5-lint.sh [root]    (default: the repository root)
# shellcheck disable=SC2016 # the guard text holds a literal ${BASH_VERSINFO...}

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

guard='[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon'"'"'s scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }'

root=${1:-$(cd "$(dirname "$0")/.." && pwd)}

fail=0
n=0
while IFS= read -r f; do
	[[ $(head -n 1 "$f") == '#!/usr/bin/env bash' ]] || continue
	n=$((n + 1))
	first=$(awk 'NR > 1 && NF && $0 !~ /^[[:space:]]*#/ { print; exit }' "$f")
	if [[ $first != "$guard" ]]; then
		echo "bash5-lint: ${f#"$root"/}: the first command is not the bash 5 guard" >&2
		fail=1
	fi
done < <(find "$root" \( -name .git -o -name node_modules \) -prune -o -name '*.sh' ! -name '*.golden.sh' -type f -print | sort)

if ((fail)); then
	echo "bash5-lint: put this line before the first command of each script listed (ADR 0090):" >&2
	echo "$guard" >&2
	exit 1
fi
echo "bash5-lint: $n scripts, every one starts with the bash 5 guard"
