#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The file_mode helper of test/e2e/backup-restore.sh (#1474): it reads 600 from a 0600 file and 644 from a 0644
# file, with GNU stat and with BSD stat (macOS). The BSD run uses a stub "stat" that refuses -c and answers -f
# like BSD stat does, so it runs on Linux too. The real BSD stat is not run here: only a macOS host does that.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=$here/../test/e2e/backup-restore.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/backup-restore-mode-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

fail() { echo "backup-restore-mode-test: $*" >&2; exit 1; }

line=$(grep -E '^file_mode\(\) \{' "$script") || fail "file_mode is not defined in $script"
eval "$line"

printf 'x\n' >"$tmp/private" && chmod 0600 "$tmp/private"
printf 'x\n' >"$tmp/open" && chmod 0644 "$tmp/open"

# check <label> <file> <want mode>
check() {
	local got
	got=$(file_mode "$2")
	[[ $got == "$3" ]] || fail "$1: $2 has mode '$got', want '$3'"
}

# The host's own stat (GNU on Linux, BSD on macOS).
check "host stat" "$tmp/private" 600
check "host stat" "$tmp/open" 644

# A BSD stat: -c is an error, -f %Lp prints the octal mode (the real stat of the host does the work).
mkdir "$tmp/bin"
real_stat=$(command -v stat)
cat >"$tmp/bin/stat" <<EOF
#!/usr/bin/env bash
[[ \$1 == -c ]] && { echo "stat: illegal option -- c" >&2; exit 1; }
[[ \$1 == -f && \$2 == %Lp ]] || exit 1
"$real_stat" -c %a "\$3" 2>/dev/null || "$real_stat" -f %Lp "\$3"
EOF
chmod +x "$tmp/bin/stat"
(
	PATH="$tmp/bin:$PATH"
	check "BSD stat stub" "$tmp/private" 600
	check "BSD stat stub" "$tmp/open" 644
	# The drill's test is `[[ mode == 600 ]]`: it must fail for 0644.
	[[ $(file_mode "$tmp/open") != 600 ]] || fail "a 0644 file passes the 600 check"
)

echo "backup-restore-mode-test: ok"
