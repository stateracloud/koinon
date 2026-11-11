# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash
#
# koinon-entry-lib.sh is sourced by every flavor's entry script (koinon-opencode in v0.1,
# ADR 0059) from /usr/local/lib/koinon/. It holds what the flavors share: argument parsing, URL
# checks, the instruction-file check and the KOINON_* exports. What differs per flavor stays in the entry script: the value check (each
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
# mcp_url (and the optional --session-id into session_id and --model-format into model_format,
# default openai: the model's wire format, ADR 0069; the entry script maps it to a provider entry
# and refuses a format it has none for; and --thinking into thinking: the thinking form Koinon
# decided for the run, internal/thinking.Mode, ADR 0069 amendments 2026-10-29 and 2026-10-31, which
# the entry script renders so the harness never guesses it from the model's ID: none, default,
# adaptive-summarized, disabled or effort-none, the last three only on the format they belong to),
# checks them, and sets koinon_shift to the number of arguments consumed (a trailing
# -- included). The caller then runs `shift "$koinon_shift"`, which leaves the arguments for the
# harness.
#
# A flavor whose harness retries model requests (internal/harness RetriesModelRequests) also takes
# Koinon's retry policy, which koinon-agentd always passes (ADR 0056 amendment 2026-10-31, point 3):
# --model-tries (the first request included), --model-retry-backoff-ms (the first wait; it doubles
# per retry, up to a minute) and --model-max-retry-after-ms (the longest Retry-After waited out; a
# longer one ends the retries), into model_tries, model_retry_backoff_ms and
# model_max_retry_after_ms. Without them the script uses the same defaults as koinon-agentd (3,
# 1000, 60000; test/flavors checks they agree), so the harness never runs on its own.
koinon_parse_args() {
  base_url='' model='' mcp_url='' session_id='' model_format=openai thinking='' koinon_shift=0
  model_tries=3 model_retry_backoff_ms=1000 model_max_retry_after_ms=60000
  while [ $# -gt 0 ]; do
    case $1 in
      --model-base-url) [ $# -ge 2 ] || koinon_die "$1 needs a value"; base_url=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --model) [ $# -ge 2 ] || koinon_die "$1 needs a value"; model=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --mcp-url) [ $# -ge 2 ] || koinon_die "$1 needs a value"; mcp_url=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --model-format) [ $# -ge 2 ] || koinon_die "$1 needs a value"; model_format=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --thinking) [ $# -ge 2 ] || koinon_die "$1 needs a value"; thinking=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --session-id) [ $# -ge 2 ] || koinon_die "$1 needs a value"; session_id=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --model-tries) [ $# -ge 2 ] || koinon_die "$1 needs a value"; model_tries=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --model-retry-backoff-ms) [ $# -ge 2 ] || koinon_die "$1 needs a value"; model_retry_backoff_ms=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
      --model-max-retry-after-ms) [ $# -ge 2 ] || koinon_die "$1 needs a value"; model_max_retry_after_ms=$2; shift 2; koinon_shift=$((koinon_shift + 2)) ;;
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
  check --model-format "$model_format"
  case $thinking in
    '' | none | default) ;;
    adaptive-summarized | disabled)
      [ "$model_format" = anthropic ] || koinon_die "--thinking $thinking applies to --model-format anthropic only"
      ;;
    effort-none)
      [ "$model_format" = openai ] || koinon_die "--thinking $thinking applies to --model-format openai only"
      ;;
    *) koinon_die "--thinking must be none, default, adaptive-summarized, disabled or effort-none: '$thinking'" ;;
  esac
  if [ -n "$session_id" ]; then
    check --session-id "$session_id"
    [ ${#session_id} -le 256 ] || koinon_die "--session-id is longer than 256 characters"
  fi
  koinon_check_count --model-tries "$model_tries" 1
  koinon_check_count --model-retry-backoff-ms "$model_retry_backoff_ms" 0
  koinon_check_count --model-max-retry-after-ms "$model_max_retry_after_ms" 0
}

# koinon_check_count NAME VALUE MIN: a decimal integer of at most 9 digits, at least MIN.
koinon_check_count() {
  case $2 in
    '' | *[!0-9]* | ??????????*) koinon_die "$1 must be a whole number: '$2'" ;;
  esac
  [ "$2" -ge "$3" ] || koinon_die "$1 must be at least $3: '$2'"
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

