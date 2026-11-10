#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/flavor-tag.sh: the tag must not depend on the caller's locale (old issue 335). A
# fixture repo has file names whose order differs between C and a UTF-8 collation; the script is run
# under both and the tags must be equal. It also tests hack/flavor-base.sh (FLAVOR_BASE, #200).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)

tmp=$(mktemp -d "${TMPDIR:-/tmp}/flavor-tag-test.XXXXXX")
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

# hack/flavor-base.sh (#200): FLAVOR_BASE becomes `--build-arg BASE=...` only with the digest the
# Dockerfile pins; unset, nothing (the Dockerfile default builds).
cp "$here/flavor-base.sh" "$tmp/hack/flavor-base.sh"
mkdir -p flavors/pinned
pin=sha256:$(printf '%064d' 7)
printf 'ARG BASE=registry.example.invalid/base/ubuntu:24.04@%s\n' "$pin" >flavors/pinned/Dockerfile
fb() { # <want rc> <want stdout> <FLAVOR_BASE> <flavor> <description>
  local rc=0 out
  out=$(FLAVOR_BASE=$3 hack/flavor-base.sh "$4" 2>/dev/null) || rc=$?
  if [[ $rc != "$1" || $out != "$2" ]]; then
    printf 'FAIL: flavor-base: %s\n  want rc=%s out=%q\n  got  rc=%s out=%q\n' "$5" "$1" "$2" "$rc" "$out" >&2
    fail=1
  else
    echo "ok: flavor-base: $5"
  fi
}
fb 0 "" "" pinned "FLAVOR_BASE unset: no build argument"
fb 0 $'--build-arg\nBASE=mirror.example.invalid/nvcr.io/base/ubuntu:24.04@'"$pin" "mirror.example.invalid/nvcr.io/base/ubuntu:24.04@$pin" pinned "the pinned digest through a mirror"
fb 0 $'--build-arg\nBASE=registry.example.invalid/base/ubuntu:24.04@'"$pin" "registry.example.invalid/base/ubuntu:24.04@$pin" pinned "the default ref itself"
fb 1 "" "mirror.example.invalid/base/ubuntu:24.04" pinned "a ref without the digest is refused"
fb 1 "" "mirror.example.invalid/base/ubuntu:24.04@sha256:$(printf '%064d' 8)" pinned "another digest is refused"
fb 1 "" "@$pin" pinned "the digest alone is refused"
fb 1 "" "a.example.invalid/x@$pin b@$pin" pinned "two references are refused"
# The Makefile word-splits the output unquoted: only the reference grammar may pass (#203 review).
fb 1 "" "x*@$pin" pinned "a glob character is refused"
fb 1 "" "mirror.example.invalid/x?y@$pin" pinned "a ? glob is refused"
fb 1 "" "mirror.example.invalid/[ab]@$pin" pinned "a [..] glob is refused"
fb 1 "" "mirror.example.invalid/\$(id)@$pin" pinned "a command substitution is refused"
fb 1 "" "mirror.example.invalid/x;true@$pin" pinned "a ; is refused"
fb 1 "" "mirror.example.invalid/Upper/x@$pin" pinned "an uppercase path component is refused"
fb 1 "" "mirror.example.invalid/x:@$pin" pinned "an empty tag is refused"
fb 1 "" "mirror.example.invalid/x@sha256:$(printf '%063d' 7)Z" pinned "a malformed digest is refused"
fb 0 $'--build-arg\nBASE=mirror.example.invalid:5000/nvcr.io/base/ubuntu@'"$pin" "mirror.example.invalid:5000/nvcr.io/base/ubuntu@$pin" pinned "a registry port and no tag pass"
fb 0 $'--build-arg\nBASE=base_img/ubuntu-24.04:24.04_x@'"$pin" "base_img/ubuntu-24.04:24.04_x@$pin" pinned "no registry host, separators in path and tag pass"
fb 2 "" "mirror.example.invalid/x@$pin" demo "a Dockerfile without a pinned ARG BASE= default: exit 2"
fb 2 "" "mirror.example.invalid/x@$pin" nosuch "an unknown flavor: exit 2"
fb 2 "" "" "" "no flavor: usage, exit 2"

# hack/probe-tag.sh (#482): the probe image's tag is a pure function of hack/spike/probe and the flavor's
# ARG BASE= line, so the e2e can pull the image flavor-publish pushed and run no apt step.
cd "$tmp"
mkdir -p pt/hack/spike/probe pt/flavors/opencode
cp "$here/probe-tag.sh" pt/hack/probe-tag.sh
(
	cd pt
	git init -q .
	printf 'FROM x\n' >hack/spike/probe/Dockerfile
	printf 'print(1)\n' >hack/spike/probe/p.py
	printf 'ARG BASE=registry.example.invalid/base@%s\n' "$pin" >flavors/opencode/Dockerfile
	git add -A
)
pt() { (cd pt && LC_ALL=${1:-C} hack/probe-tag.sh); }
pt_check() { # <description> <ok if the two tags are equal: 1, differ: 0> <tag a> <tag b>
	if { [[ $2 == 1 ]] && [[ $3 == "$4" ]]; } || { [[ $2 == 0 ]] && [[ $3 != "$4" ]]; }; then
		echo "ok: probe-tag: $1"
	else
		printf 'FAIL: probe-tag: %s (%s, %s)\n' "$1" "$3" "$4" >&2
		fail=1
	fi
}
t0=$(pt)
[[ $t0 =~ ^probe-[0-9a-f]{12}$ ]] || { echo "FAIL: probe-tag: unexpected tag $t0" >&2; fail=1; }
pt_check "repeatable" 1 "$t0" "$(pt)"
pt_check "an unstaged edit of a probe file changes the tag" 0 "$t0" "$(printf 'print(2)\n' >pt/hack/spike/probe/p.py && pt)"
printf 'print(1)\n' >pt/hack/spike/probe/p.py
pt_check "restoring the file restores the tag" 1 "$t0" "$(pt)"
printf 'ARG BASE=registry.example.invalid/base@sha256:%s\n' "$(printf '%064d' 9)" >pt/flavors/opencode/Dockerfile
pt_check "another flavor base changes the tag" 0 "$t0" "$(pt)"
printf 'ARG BASE=registry.example.invalid/base@%s\n' "$pin" >pt/flavors/opencode/Dockerfile
printf '# unrelated flavor edit\n' >>pt/flavors/opencode/Dockerfile
pt_check "a flavor edit outside the BASE line keeps the tag" 1 "$t0" "$(pt)"
printf 'x\n' >pt/flavors/unrelated
pt_check "a file outside hack/spike/probe keeps the tag" 1 "$t0" "$(pt)"
rm pt/hack/probe-tag.sh
if (cd pt && cp "$here/probe-tag.sh" hack/probe-tag.sh && rm flavors/opencode/Dockerfile && hack/probe-tag.sh >/dev/null 2>&1); then
	echo "FAIL: probe-tag: a missing flavor Dockerfile must fail" >&2
	fail=1
else
	echo "ok: probe-tag: no flavor Dockerfile, exit non-zero"
fi

[[ $fail -eq 0 ]] && echo "flavor-tag-test: ok"
exit "$fail"
