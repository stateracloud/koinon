# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=sh
#
# savework reads the uncommitted work of the git repositories in a workspace so that
# koinon-agentd can keep it when the sandbox has to be recreated (savework.go, ADR 0054).
#
#   sh -c SCRIPT savework DIR MAXBYTES [HOST]
#
# It looks in DIR and up to two levels below it. For each repository whose origin is a repository
# on HOST (the connection's host, github.com when omitted) it takes the newest commit the host
# has (the first commit of the current history that no remote branch contains, or HEAD when there
# is none) as the base, and lists what the
# working tree changes against it: modified, added and untracked-but-not-ignored, and deleted
# files. It uses a temporary index, so the repository's own index, branches and working tree are
# not touched (git does write the new file contents as objects). Symbolic links, submodules and
# paths git has to quote are left out and reported.
#
# The repositories are the agent's, so git runs with a config the agent cannot use to run code (old issue 260):
# no system, global or home config (GIT_CONFIG_NOSYSTEM, GIT_CONFIG_GLOBAL=/dev/null, which also
# ignores the home include file /etc/gitconfig names), core.fsmonitor and core.hooksPath forced off
# on the command line, no external diff or textconv, and every filter driver the repository defines
# emptied and not required (git runs no filter whose command is empty) so files are hashed as they are on disk,
# and no transport, so a partial clone cannot fetch from the promisor remote its config names. A
# repository whose own config includes another file (include, includeIf) is skipped: git cannot be
# told to ignore includes in the repository's config, and the files they name are not Koinon's to read.
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
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
# No transport at all: a partial clone fetches missing objects from its promisor remote, which
# the repository's config names together with its uploadpack, sshCommand or ext:: URL.
# GIT_ALLOW_PROTOCOL overrides every protocol.*.allow; GIT_NO_LAZY_FETCH is git 2.45 and later.
export GIT_ALLOW_PROTOCOL=none GIT_NO_LAZY_FETCH=1
unset GIT_CONFIG GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_CONFIG_SYSTEM GIT_EXTERNAL_DIFF GIT_DIR GIT_WORK_TREE \
  GIT_COMMON_DIR GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_ATTR_SOURCE GIT_SSH GIT_SSH_COMMAND GIT_ASKPASS
dir=$1
max=$2
host=${3:-github.com}
tmp=$(mktemp -d 2>/dev/null) || { echo "savework: cannot create a temporary directory" >&2; exit 2; }
trap 'rm -rf "$tmp"' EXIT
command -v git >/dev/null 2>&1 || { echo "savework: git is not installed in this sandbox" >&2; exit 2; }
tab=$(printf '\t')

# gitr REPO ARGS... runs git in REPO with the isolated config; $gitcfg holds the repository's
# filter overrides (set per repository below).
gitcfg=
gitr() {
  _repo=$1
  shift
  # shellcheck disable=SC2086
  git -C "$_repo" -c core.fsmonitor=false -c core.hooksPath=/dev/null -c core.excludesFile=/dev/null -c core.attributesFile=/dev/null $gitcfg "$@"
}

for g in "$dir/.git" "$dir"/*/.git "$dir"/*/*/.git; do
  [ -e "$g" ] || continue
  printf '%s\n' "${g%/.git}"
done >"$tmp/repos"

n=0
total=0
: >"$tmp/skipped"
while IFS= read -r r; do
  gitcfg=
  # The config keys git would act on, without reading any include.
  git -C "$r" -c core.fsmonitor=false -c core.hooksPath=/dev/null config --no-includes --name-only --list >"$tmp/keys" 2>"$tmp/err" ||
    { echo "S $r: git could not read the repository configuration" >>"$tmp/skipped"; continue; }
  if grep -qiE '^(include\.|includeif\.)' "$tmp/keys"; then
    echo "S $r: the repository configuration includes other files, which Koinon does not read" >>"$tmp/skipped"
    continue
  fi
  bad=
  # One name per line, never split or globbed: a driver named * must not expand to file names.
  grep -iE '^filter\.' "$tmp/keys" | sed -E 's/^filter\.(.*)\.[^.]*$/\1/' | sort -u >"$tmp/filters"
  while IFS= read -r k; do
    case $k in
      *[!A-Za-z0-9_-]* | '') bad=1 ;;
      *) gitcfg="$gitcfg -c filter.$k.clean= -c filter.$k.smudge= -c filter.$k.process= -c filter.$k.required=false" ;;
    esac
  done <"$tmp/filters"
  if [ -n "$bad" ]; then
    echo "S $r: the repository configuration has a filter Koinon cannot neutralise" >>"$tmp/skipped"
    continue
  fi
  url=$(gitr "$r" remote get-url origin 2>/dev/null) || { echo "S $r: no origin remote" >>"$tmp/skipped"; continue; }
  case $url in
    "https://$host/"*) slug=${url#"https://$host/"} ;;
    "git@$host:"*) slug=${url#"git@$host:"} ;;
    "ssh://git@$host/"*) slug=${url#"ssh://git@$host/"} ;;
    *) echo "S $r: origin is not a repository on $host" >>"$tmp/skipped"; continue ;;
  esac
  slug=${slug%/}
  slug=${slug%.git}
  case $slug in
    */*/* | */ | /* | *[[:space:]]* | '') echo "S $r: cannot read the repository from its origin" >>"$tmp/skipped"; continue ;;
    */*) ;;
    *) echo "S $r: cannot read the repository from its origin" >>"$tmp/skipped"; continue ;;
  esac
  head=$(gitr "$r" rev-parse --verify -q HEAD 2>/dev/null) || head=
  if [ -z "$head" ]; then echo "S $slug: the repository has no commit" >>"$tmp/skipped"; continue; fi
  oldest=$(gitr "$r" rev-list HEAD --not --remotes 2>/dev/null | tail -n 1)
  if [ -n "$oldest" ]; then
    base=$(gitr "$r" rev-parse --verify -q "$oldest^" 2>/dev/null) || base=
  else
    base=$head
  fi
  if [ -z "$base" ]; then echo "S $slug: no commit of its history is on GitHub" >>"$tmp/skipped"; continue; fi

  idx="$tmp/index.$n"
  if ! GIT_INDEX_FILE=$idx gitr "$r" read-tree "$base" 2>"$tmp/err" ||
     ! GIT_INDEX_FILE=$idx gitr "$r" add -A 2>>"$tmp/err"; then
    echo "S $slug: git could not read the working tree: $(head -c 200 "$tmp/err" | tr '\n' ' ')" >>"$tmp/skipped"
    continue
  fi
  GIT_INDEX_FILE=$idx gitr "$r" -c core.quotepath=off diff --no-ext-diff --no-textconv --cached --raw --no-renames --no-abbrev "$base" >"$tmp/raw" 2>/dev/null || { echo "S $slug: git diff failed" >>"$tmp/skipped"; continue; }
  GIT_INDEX_FILE=$idx gitr "$r" -c core.quotepath=off diff --no-ext-diff --no-textconv --cached --numstat --no-renames "$base" >"$tmp/num" 2>/dev/null || { echo "S $slug: git diff failed" >>"$tmp/skipped"; continue; }

  : >"$tmp/list.$n"
  exec 3<"$tmp/num"
  while IFS=$tab read -r meta path; do
    IFS=$tab read -r add del _npath <&3 || { add=0; del=0; }
    case $path in
      '"'* | ' '* | *' ') echo "S $slug: $path: the path has characters Koinon does not save" >>"$tmp/skipped"; continue ;;
    esac
    # shellcheck disable=SC2086 # meta is a space-separated field list
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
    size=$(GIT_INDEX_FILE=$idx gitr "$r" cat-file -s "$nsha" 2>/dev/null) || { echo "S $slug: $path: unreadable" >>"$tmp/skipped"; continue; }
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
      gitr "$r" cat-file blob "$sha" || exit 2
      printf '\n'
    fi
  done <"$tmp/list.$i"
  echo E
  i=$((i + 1))
done
cat "$tmp/skipped"
exit 0
