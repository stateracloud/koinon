#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Renumbers the migrations this branch added so they follow the base branch's highest migration, then
# runs `make generate` (sqlc reads the migrations). Use it when `hack/migrate-lint.sh` reports a
# duplicate or a gap after merging origin/main: pick the migration number late, renumber here.
#
#   hack/migrate-renumber.sh [--no-generate] [base-ref]     (default origin/main)
#
# "Added by this branch" means: a migration file in the working tree that the base ref does not have
# under the same name. Merged migrations are never renamed or edited (forward-only, ADR 0027). Files
# are renamed with `git mv`, in the order of their current numbers; the new names are printed, and any
# file that still mentions an old name is listed so you can fix the reference.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

generate=1
if [[ ${1:-} == --no-generate ]]; then generate=0; shift; fi
base=${1:-origin/main}
dir=${KOINON_MIGRATIONS_DIR:-internal/db/migrations}

git rev-parse --verify -q "$base^{commit}" >/dev/null || { echo "migrate-renumber: unknown base ref $base" >&2; exit 1; }
base_files=$(git ls-tree --name-only "$base" "$dir/" | sed 's#.*/##' | grep -E '^[0-9]{5}_.*\.sql$' | sort || true)
max=0
for f in $base_files; do n=$((10#${f:0:5})); ((n > max)) && max=$n; done

added=()
while IFS= read -r f; do
  grep -qxF "$f" <<<"$base_files" || added+=("$f")
done < <(cd "$dir" && /bin/ls | grep -E '^[0-9]{5}_.*\.sql$' | sort)

if ((${#added[@]} == 0)); then echo "migrate-renumber: this branch adds no migrations"; exit 0; fi

next=$((max + 1))
renames=()
for f in "${added[@]}"; do
  new=$(printf '%05d' "$next")${f:5}
  [[ $new == "$f" ]] || renames+=("$f $new")
  next=$((next + 1))
done

if ((${#renames[@]} == 0)); then echo "migrate-renumber: already in order after $(printf '%05d' "$max")"; else
  # Two passes through temporary names, so a new name never lands on a not-yet-moved old one.
  for r in "${renames[@]}"; do old=${r% *}; git mv "$dir/$old" "$dir/.renumber-$old"; done
  for r in "${renames[@]}"; do old=${r% *}; new=${r#* }; git mv "$dir/.renumber-$old" "$dir/$new"; echo "renamed $old -> $new"; done
  for r in "${renames[@]}"; do
    old=${r% *}
    refs=$(git grep -l -F -- "$old" -- . ":!$dir" 2>/dev/null || true)
    [[ -z $refs ]] || { echo "migrate-renumber: $old is still named in:"; echo "$refs"; }
  done
fi

if ((generate)); then make generate; fi
