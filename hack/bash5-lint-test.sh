#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/bash5-lint.sh on fixture trees, and proves the guard line itself: under an old bash (the
# macOS /bin/bash 3.2, when present) a guarded script exits 2 with the message before any bash 4+
# syntax is parsed; under bash 5 it runs on.
# shellcheck disable=SC2016 # fixtures and sed patterns hold literal ${...}

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
lint=$here/bash5-lint.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/bash5-lint-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

guard=$(sed -n '/^\[ "\${BASH_VERSINFO/p' "$lint" | head -1)
pass=0
failed=0
check() { # <name> <got> <want>
	if [[ $2 == "$3" ]]; then
		pass=$((pass + 1))
	else
		echo "FAIL $1: got '$2', want '$3'" >&2
		failed=$((failed + 1))
	fi
}
run() { # <root>: lint exit code
	local rc=0
	"$lint" "$1" >"$tmp/out" 2>"$tmp/err" || rc=$?
	echo "$rc"
}

# A tree that passes: guarded script (guard after header comments), a bash-free sh script, a sourced
# library without a shebang, and a golden file.
ok=$tmp/ok
mkdir -p "$ok/hack" "$ok/img"
printf '#!/usr/bin/env bash\n# SPDX-License-Identifier: Apache-2.0\n# header\n\n%s\nset -e\n' "$guard" >"$ok/hack/a.sh"
printf '#!/bin/sh\necho hi\n' >"$ok/img/entry.sh"
printf '# SPDX-License-Identifier: Apache-2.0\nx=1\n' >"$ok/hack/lib.sh"
printf 'echo golden\n' >"$ok/hack/q.golden.sh"
check "a guarded tree passes" "$(run "$ok")" 0

# A script without the guard fails and is named.
bad=$tmp/bad
cp -R "$ok" "$bad"
printf '#!/usr/bin/env bash\nset -euo pipefail\n' >"$bad/hack/b.sh"
check "a script without the guard fails" "$(run "$bad")" 1
check "the failing script is named" "$(grep -c 'hack/b.sh' "$tmp/err")" 1
check "the guarded script is not named" "$(grep -c 'hack/a.sh' "$tmp/err")" 0

# The guard must be the first command: a command above it fails.
late=$tmp/late
mkdir -p "$late"
printf '#!/usr/bin/env bash\ndeclare -A m\n%s\n' "$guard" >"$late/c.sh"
check "a command above the guard fails" "$(run "$late")" 1

# A guard with a different message fails.
drift=$tmp/drift
mkdir -p "$drift"
printf '#!/usr/bin/env bash\n[ "${BASH_VERSINFO:-0}" -ge 4 ] || exit 2\n' >"$drift/d.sh"
check "a drifted guard fails" "$(run "$drift")" 1

# The fail-open hook variant: accepted in hack/hooks/*-check.sh, rejected elsewhere.
# A hook starts with the KOINON_MAINTAINER_HOOKS opt-in line (issue 843), then the hook guard.
hookg=$(sed -n "s/^hook_guard='\(.*\)'\$/\1/p" "$lint" | sed "s/'\"'\"'/'/g")
gate=$(sed -n "s/^hook_gate='\(.*\)'\$/\1/p" "$lint")
hk=$tmp/hk
mkdir -p "$hk/hack/hooks"
printf '#!/usr/bin/env bash\n%s\n' "$hookg" >"$hk/hack/hooks/bash-read-check.sh"
check "a registered hook without the opt-in line fails" "$(run "$hk")" 1
printf '#!/usr/bin/env bash\n%s\n%s\n' "$hookg" "$gate" >"$hk/hack/hooks/bash-read-check.sh"
check "a registered hook with the opt-in line second fails" "$(run "$hk")" 1
printf '#!/usr/bin/env bash\n%s\n%s\n' "$gate" "$guard" >"$hk/hack/hooks/bash-read-check.sh"
check "a registered hook with the exit-2 guard fails" "$(run "$hk")" 1
printf '#!/usr/bin/env bash\n# c\n%s\n\n# c\n%s\n' "$gate" "$hookg" >"$hk/hack/hooks/bash-read-check.sh"
check "the hook variant passes in a registered hook" "$(run "$hk")" 0
for other in hack/hooks/sub/evil-check.sh test/hack/hooks/x-check.sh hack/hooks/notahook-check.sh; do
	rm -rf "$tmp/hk2"
	mkdir -p "$tmp/hk2/$(dirname "$other")"
	printf '#!/usr/bin/env bash\n%s\n%s\n' "$gate" "$hookg" >"$tmp/hk2/$other"
	check "the hook variant is rejected in $other" "$(run "$tmp/hk2")" 1
done
printf '#!/usr/bin/env bash\n%s\n%s\n' "$gate" "$hookg" >"$hk/hack/y.sh"
check "the hook variant is rejected elsewhere" "$(run "$hk")" 1
rm "$hk/hack/y.sh"
printf '#!/usr/bin/env bash\n%s\n%s\n' "$gate" "$hookg" >"$hk/hack/hooks/z_test.sh"
check "the hook variant is rejected in a hook test" "$(run "$hk")" 1

# Any other bash shebang fails.
for sb in '#!/bin/bash' '#!/usr/bin/env -S bash -e' '#!/usr/local/bin/bash'; do
	sbd=$tmp/sb
	rm -rf "$sbd"
	mkdir -p "$sbd"
	printf '%s\n%s\n' "$sb" "$guard" >"$sbd/s.sh"
	check "shebang $sb fails" "$(run "$sbd")" 1
done

# In a git checkout the files come from git ls-files.
gd=$tmp/gitroot
mkdir -p "$gd"
git -C "$gd" init -q
printf '#!/bin/bash\n' >"$gd/g.sh"
check "git root: a bad shebang fails" "$(run "$gd")" 1

# The guard itself, under the system bash 3.2 when this machine has it (macOS), else skipped.
printf '#!/usr/bin/env bash\n%s\ndeclare -A m=([a]=1)\necho ran\n' "$guard" >"$tmp/g.sh"
if [[ -x /bin/bash ]] && [[ $(/bin/bash -c 'echo ${BASH_VERSINFO[0]}') -lt 5 ]]; then
	rc=0
	/bin/bash "$tmp/g.sh" >"$tmp/gout" 2>"$tmp/gerr" || rc=$?
	check "old bash: exit 2" "$rc" 2
	oldv=$(/bin/bash -c 'echo "$BASH_VERSION"')
	check "old bash: the message" "$(cat "$tmp/gerr")" "Koinon's scripts need bash 5 or newer (this is $oldv); on macOS: brew install bash"
	check "old bash: nothing ran" "$(cat "$tmp/gout")" ""
else
	echo "bash5-lint-test: no old /bin/bash here, skipping the old-bash guard check"
fi
rc=0
bash "$tmp/g.sh" >"$tmp/gout" 2>/dev/null || rc=$?
check "bash 5: the script runs on" "$(cat "$tmp/gout")" "ran"

# The repository itself is clean.
check "this repository passes" "$(run "$here/..")" 0

echo "bash5-lint-test: $pass passed, $failed failed"
((failed == 0))
