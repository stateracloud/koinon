# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# Maintainer lab settings (sourced, not run). Nothing about any particular lab is tracked in this
# repository: a maintainer who has a lab (an Argo Workflows queue, its S3 buckets, a registry) keeps
# its settings in an untracked file and the lab scripts read them from there.
#
#   ${KOINON_LAB_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/koinon/lab.env}
#
# The file is plain shell that assigns KOINON_* variables (see hack/lab/lab.env.example). A variable
# that is already set in the environment wins over the file, so a one-off override needs no edit.
# Scripts that need a setting call lab_require, which exits 2 with a message naming the variable
# and the file when it is empty.
#
# Used by hack/e2e-argo.sh; a lab gate script should source this file too instead of growing its own.

lab_config_file() {
  printf '%s\n' "${KOINON_LAB_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/koinon/lab.env}"
}

# lab_config_load reads the lab config file, if there is one. Every KOINON_* variable it sets is
# exported unless the environment already had a value for it.
lab_config_load() {
  local f v pre="" post=""
  f=$(lab_config_file)
  [[ -r $f ]] || return 0
  # What the environment had before the file: re-applied after it, so the environment wins.
  for v in $(compgen -v KOINON_); do
    pre+=$(printf 'export %s=%q\n' "$v" "${!v}")$'\n'
  done
  # The file runs in a subshell: it can do anything shell does, and only its KOINON_* results count.
  post=$(
    set +eu
    # shellcheck disable=SC1090
    . "$f" >/dev/null 2>&1 || exit 1
    for v in $(compgen -v KOINON_); do printf 'export %s=%q\n' "$v" "${!v}"; done
  ) || { echo "lab config: $f failed to load" >&2; return 1; }
  eval "$post"
  eval "$pre"
}

# lab_require <script name> <VAR>...: exit 2 naming every variable that is empty.
lab_require() {
  local who=$1 v missing=""
  shift
  for v in "$@"; do
    [[ -n ${!v:-} ]] || missing+=" $v"
  done
  [[ -z $missing ]] && return 0
  {
    echo "$who: not configured:$missing"
    echo "  These name a maintainer lab (Argo queue, buckets) and are not tracked in the repository."
    echo "  Set them in the environment or in $(lab_config_file) (template: hack/lab/lab.env.example)."
    echo "  Without a lab, run the e2e on your own Docker: hack/e2e-remote.sh --local"
  } >&2
  exit 2
}
