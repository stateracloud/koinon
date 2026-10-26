# SPDX-License-Identifier: Apache-2.0
#
# koinon-entry-lib.sh is sourced by every flavor's entry script (koinon-opencode in v0.1,
# ADR 0059) from /usr/local/lib/koinon/. It holds what the flavors share: argument parsing, URL
# checks, the instruction-file check, the instruction link, the environment clean-up and the
# KOINON_* exports. What differs per flavor stays in the entry script: the value check (each
# harness substitutes values differently), PATH, the harness's own variables, home clean-up and
# the final exec.
#
# Before sourcing, the script sets KOINON_FLAVOR (opencode) and defines check(),
# which calls koinon_die for a value its harness cannot take. After sourcing it may define
# koinon_extra_arg() for flavor-specific switches. Every refusal exits 64 with
# "koinon-<flavor>: <message>" on stderr (the flavor contract suite asserts both).

# The instruction file koinon-agentd writes before it starts the harness (internal/agentd,
# instructionsPath).
koinon_instructions=/sandbox/.koinon/AGENTS.md

koinon_die() {
  echo "koinon-$KOINON_FLAVOR: $*" >&2
  exit 64
}

# koinon_check_url NAME VALUE: check() first, then http or https with a host.
koinon_check_url() {
  check "$1" "$2"
  case $2 in
    http://?* | https://?*) ;;
    *) koinon_die "$1 must be an http or https URL: '$2'" ;;
  esac
}

# koinon_extra_arg ARG: a switch only this flavor takes. Return 0 after setting what it needs
# when ARG is one (it counts as one argument), non-zero when it is not. Redefine it after
# sourcing this file.
koinon_extra_arg() { return 1; }

# koinon_parse_args "$@": reads --model-base-url, --model and --mcp-url into base_url, model and
# mcp_url, checks them, and sets koinon_shift to the number of arguments consumed (a trailing
# -- included). The caller then runs `shift "$koinon_shift"`, which leaves the arguments for the
# harness.
koinon_parse_args() {
  base_url='' model='' mcp_url='' koinon_shift=0
  while [ $# -gt 0 ]; do
    case $1 in
      --model-base-url) [ $# -ge 2 ] || koinon_die "$1 needs a value"; base_url=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --model) [ $# -ge 2 ] || koinon_die "$1 needs a value"; model=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --mcp-url) [ $# -ge 2 ] || koinon_die "$1 needs a value"; mcp_url=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --) koinon_shift=$((koinon_shift + 1)); break ;;
      *)
        if koinon_extra_arg "$1"; then
          shift
          koinon_shift=$((koinon_shift + 1))
        else
          koinon_die "unknown argument '$1'"
        fi
        ;;
    esac
  done
  koinon_check_url --model-base-url "$base_url"
  check --model "$model"
  koinon_check_url --mcp-url "$mcp_url"
}

# koinon_export_base_env: what every flavor sets for itself because OpenShell does not pass the
# image's ENV (ADR 0048). State lives in the sandbox home, the agent's volume (ADR 0048, 0049).
koinon_export_base_env() {
  export LANG=C.UTF-8
  export HOME=/sandbox
  export XDG_CACHE_HOME=/sandbox/.cache
}

# koinon_export_endpoints: the per-agent values the flavor's configuration reads.
koinon_export_endpoints() {
  export KOINON_MODEL_BASE_URL="$base_url"
  export KOINON_MODEL="$model"
  export KOINON_MCP_URL="$mcp_url"
}

# koinon_require_instructions: Koinon's instructions reach the model only through the file the
# flavor links to. Harnesses skip a missing file silently and load an empty one as nothing, so
# refuse to start without real content. Sets instructions.
koinon_require_instructions() {
  instructions=$koinon_instructions
  if [ ! -f "$instructions" ] || [ -L "$instructions" ] || [ ! -s "$instructions" ]; then
    koinon_die "missing instructions: $instructions (koinon-agentd writes it before the harness starts)"
  fi
}

# koinon_link_instructions LINK: puts LINK back as a symbolic link to the instruction file, replacing
# whatever is there. koinon-agentd also restores it before every prompt.
koinon_link_instructions() {
  rm -rf "$1"
  ln -s "$instructions" "$1"
}
