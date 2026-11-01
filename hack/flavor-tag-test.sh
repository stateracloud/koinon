#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/flavor-tag.sh: the tag must not depend on the caller's locale (#335). A
# fixture repo has file names whose order differs between C and a UTF-8 collation; the script is run
# under both and the tags must be equal.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/hack" "$tmp/flavors/demo" "$tmp/flavors/common"
cp "$here/flavor-tag.sh" "$tmp/hack/flavor-tag.sh"
cd "$tmp"
git init -q .
# Names that collate differently: case, punctuation, a non-ASCII letter.
for f in flavors/demo/Dockerfile flavors/demo/a_b flavors/demo/a-b flavors/demo/aB flavors/demo/Ab \
  flavors/demo/zeta flavors/demo/Zeta flavors/demo/éa flavors/common/_x flavors/common/X; do
  printf 'content of %s\n' "$f" >"$f"
done
git add -A

fail=0
tags=()
tried=()
have=$(locale -a 2>/dev/null || true)
for loc in C POSIX C.UTF-8 en_US.UTF-8 en_ZA.UTF-8 nl_NL.UTF-8 sv_SE.UTF-8; do
  # Only locales this machine has; an absent one falls back to C and proves nothing.
  if [[ $loc != C && $loc != POSIX ]] && ! grep -qixF -e "${loc//UTF-8/utf8}" -e "$loc" <<<"$have"; then
    continue
  fi
  tag=$(LC_ALL=$loc LANG=$loc hack/flavor-tag.sh demo)
  tried+=("$loc")
  tags+=("$tag")
  echo "$loc -> $tag"
done

for t in "${tags[@]}"; do
  if [[ $t != "${tags[0]}" ]]; then
    echo "FAIL: the flavor tag depends on the locale: ${tried[*]} -> ${tags[*]}" >&2
    fail=1
    break
  fi
done

# A UTF-8 locale with its own collation must have run, or the test proves nothing. CI generates
# en_US.UTF-8 before this runs; elsewhere, install any non-C UTF-8 locale.
other=0
for l in "${tried[@]}"; do [[ $l == C || $l == POSIX || $l == C.UTF-8 ]] || other=1; done
if [[ $other -eq 0 ]]; then
  echo "FAIL: no locale with its own collation (e.g. en_US.UTF-8) is installed, so the test cannot detect a locale dependency" >&2
  fail=1
fi

[[ $fail -eq 0 ]] && echo "flavor-tag-test: ok"
exit "$fail"
