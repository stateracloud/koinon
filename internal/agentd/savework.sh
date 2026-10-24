# SPDX-License-Identifier: Apache-2.0
#
# savework reads the uncommitted work of the git repositories in a workspace so that
# koinon-agentd can keep it when the sandbox has to be recreated (savework.go, ADR 0054).
#
#   sh -c SCRIPT savework DIR MAXBYTES
#
# It looks in DIR and up to two levels below it. For each repository whose origin is a GitHub
# repository it takes the newest commit GitHub has (the first commit of the current history
# that no remote branch contains, or HEAD when there is none) as the base, and lists what the
# working tree changes against it: modified, added and untracked-but-not-ignored, and deleted
# files. It uses a temporary index, so the repository's own index, branches and working tree are
# not touched (git does write the new file contents as objects). Symbolic links, submodules and
# paths git has to quote are left out and reported.
#
# Output on stdout, line based, file contents inline:
#   R OWNER/NAME BASE_SHA
#   F STATUS MODE SIZE ADDED DELETED BINARY PATH      then SIZE bytes of content and a newline
#   E
#   S REASON                                           something was left out
# STATUS is A, M or D (a deleted file has no content); BINARY is 0 or 1.
# Exits 0 with no R line when there is nothing to save, 3 when the files total more than
# MAXBYTES (nothing is printed then), 2 when it cannot run.
set -u
export GIT_OPTIONAL_LOCKS=0
export GIT_TERMINAL_PROMPT=0
dir=$1
max=$2
tmp=$(mktemp -d 2>/dev/null) || { echo "savework: cannot create a temporary directory" >&2; exit 2; }
trap 'rm -rf "$tmp"' EXIT
command -v git >/dev/null 2>&1 || { echo "savework: git is not installed in this sandbox" >&2; exit 2; }
tab=$(printf '\t')

for g in "$dir/.git" "$dir"/*/.git "$dir"/*/*/.git; do
  [ -e "$g" ] || continue
  printf '%s\n' "${g%/.git}"
done >"$tmp/repos"

n=0
total=0
: >"$tmp/skipped"
while IFS= read -r r; do
  url=$(git -C "$r" remote get-url origin 2>/dev/null) || { echo "S $r: no origin remote" >>"$tmp/skipped"; continue; }
  case $url in
    https://github.com/*) slug=${url#https://github.com/} ;;
    git@github.com:*) slug=${url#git@github.com:} ;;
    ssh://git@github.com/*) slug=${url#ssh://git@github.com/} ;;
    *) echo "S $r: origin is not a GitHub repository" >>"$tmp/skipped"; continue ;;
  esac
  slug=${slug%/}
  slug=${slug%.git}
  case $slug in
    */*/* | */ | /* | *[[:space:]]* | '') echo "S $r: cannot read the repository from its origin" >>"$tmp/skipped"; continue ;;
    */*) ;;
    *) echo "S $r: cannot read the repository from its origin" >>"$tmp/skipped"; continue ;;
  esac
  head=$(git -C "$r" rev-parse --verify -q HEAD 2>/dev/null) || head=
  if [ -z "$head" ]; then echo "S $slug: the repository has no commit" >>"$tmp/skipped"; continue; fi
  oldest=$(git -C "$r" rev-list HEAD --not --remotes 2>/dev/null | tail -n 1)
  if [ -n "$oldest" ]; then
    base=$(git -C "$r" rev-parse --verify -q "$oldest^" 2>/dev/null) || base=
  else
    base=$head
  fi
  if [ -z "$base" ]; then echo "S $slug: no commit of its history is on GitHub" >>"$tmp/skipped"; continue; fi

  idx="$tmp/index.$n"
  if ! GIT_INDEX_FILE=$idx git -C "$r" read-tree "$base" 2>"$tmp/err" ||
     ! GIT_INDEX_FILE=$idx git -C "$r" add -A 2>>"$tmp/err"; then
    echo "S $slug: git could not read the working tree: $(head -c 200 "$tmp/err" | tr '\n' ' ')" >>"$tmp/skipped"
    continue
  fi
  GIT_INDEX_FILE=$idx git -C "$r" -c core.quotepath=off diff --cached --raw --no-renames --no-abbrev "$base" >"$tmp/raw" 2>/dev/null || { echo "S $slug: git diff failed" >>"$tmp/skipped"; continue; }
  GIT_INDEX_FILE=$idx git -C "$r" -c core.quotepath=off diff --cached --numstat --no-renames "$base" >"$tmp/num" 2>/dev/null || { echo "S $slug: git diff failed" >>"$tmp/skipped"; continue; }

  : >"$tmp/list.$n"
  exec 3<"$tmp/num"
  while IFS=$tab read -r meta path; do
    IFS=$tab read -r add del _npath <&3 || { add=0; del=0; }
    case $path in
      '"'* | ' '* | *' ') echo "S $slug: $path: the path has characters Koinon does not save" >>"$tmp/skipped"; continue ;;
    esac
    set -- $meta
    omode=${1#:}
    nmode=$2
    nsha=$4
    st=$5
    case $st in
      A | M | D) ;;
      *) echo "S $slug: $path: a type change is not saved" >>"$tmp/skipped"; continue ;;
    esac
    bin=0
    if [ "$add" = - ]; then add=0; del=0; bin=1; fi
    if [ "$st" = D ]; then
      case $omode in
        120000 | 160000) echo "S $slug: $path: a symbolic link or submodule is not saved" >>"$tmp/skipped"; continue ;;
      esac
      echo "D 000000 0 $add $del $bin $nsha $path" >>"$tmp/list.$n"
      continue
    fi
    case $nmode in
      100644 | 100755) ;;
      *) echo "S $slug: $path: a symbolic link or submodule is not saved" >>"$tmp/skipped"; continue ;;
    esac
    size=$(GIT_INDEX_FILE=$idx git -C "$r" cat-file -s "$nsha" 2>/dev/null) || { echo "S $slug: $path: unreadable" >>"$tmp/skipped"; continue; }
    total=$((total + size))
    echo "$st $nmode $size $add $del $bin $nsha $path" >>"$tmp/list.$n"
  done <"$tmp/raw"
  exec 3<&-
  if [ -s "$tmp/list.$n" ]; then
    printf '%s\n%s\n%s\n' "$r" "$slug" "$base" >"$tmp/meta.$n"
    n=$((n + 1))
  fi
done <"$tmp/repos"

if [ "$total" -gt "$max" ]; then
  echo "savework: the files total $total bytes, over the limit of $max" >&2
  exit 3
fi

i=0
while [ "$i" -lt "$n" ]; do
  { read -r r; read -r slug; read -r base; } <"$tmp/meta.$i"
  printf 'R %s %s\n' "$slug" "$base"
  while read -r st mode size add del bin sha path; do
    printf 'F %s %s %s %s %s %s %s\n' "$st" "$mode" "$size" "$add" "$del" "$bin" "$path"
    if [ "$st" != D ]; then
      git -C "$r" cat-file blob "$sha" || exit 2
      printf '\n'
    fi
  done <"$tmp/list.$i"
  echo E
  i=$((i + 1))
done
cat "$tmp/skipped"
exit 0
