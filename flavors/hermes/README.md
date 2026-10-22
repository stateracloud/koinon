# Hermes flavor

[Hermes Agent](https://github.com/NousResearch/hermes-agent) 0.22.3 driven over ACP
(`hermes acp`) by `koinon-agentd` through OpenShell's non-TTY exec (ADR 0035, 0048, 0049). The
image is the official `nousresearch/hermes-agent:v0.22.3`, pinned by digest, with Koinon's
entry script, read-only config and managed scope added. It holds no Koinon binary.

Build: `make flavor-hermes` (loads `koinon/flavor-hermes:dev`; nothing is pushed).
Contract test: `make test-flavors` (`test/flavors`, needs Docker).

## Contract

| Item (ADR 0035) | Hermes flavor |
| --- | --- |
| ACP command | `/usr/local/bin/koinon-hermes --model-base-url URL --model NAME --mcp-url URL`, run as UID 10001 in a session working directory under `/sandbox` |
| Resume mode | `session/load` (replays history). Hermes also advertises `session/resume`, `fork` and `list`; Koinon does not rely on them |
| Unknown session ID | `session/load` fails with JSON-RPC `-32603`, `data: {"details": "not_found"}`; no new session is made |
| Model endpoint | `--model-base-url` and `--model` as a `custom` provider; key from `$OPENAI_API_KEY` (`model.key_env`) |
| MCP | `--mcp-url`, server `koinon` in Hermes's own config with `Authorization: Bearer ${OPENAI_API_KEY}` (Hermes takes no client MCP servers); tools reach the model as `mcp__koinon__<tool>` |
| Tools | `file` (`read_file`, `write_file`, `patch`, `search_files`), `terminal`, `todo` and the `koinon` MCP server; nothing else |
| State | `/sandbox/.hermes` (`HERMES_HOME`): sessions in `state.db`, logs in `logs/`, the gateway daemon's lock and socket |
| Home exclude paths | `/sandbox/.hermes/logs`, `/sandbox/.hermes/cache`, `/sandbox/.hermes/backups`, `/sandbox/.cache` |
| Instructions | `/sandbox/.koinon/AGENTS.md`, written by koinon-agentd before every harness start, reaches the system prompt as `$HERMES_HOME/SOUL.md`, a link to it (see [Instructions](#instructions)). The entry script refuses to start (exit 64) when the file is missing, empty or a symbolic link |
| Tool switches | Hermes's managed scope, [`/etc/hermes/config.yaml`](etc/hermes/config.yaml), read-only and over the user config (see [Managed scope](#managed-scope)). The entry script refuses to start (exit 64) unless Hermes reads it |
| Permission requests | Hermes asks over ACP before every `write_file` and `patch` (`acp_adapter/edit_approval.py`), offering `allow_once` and `reject_once`; the tool kind is `edit`, which koinon-agentd's default mapping allows. Dangerous terminal commands also ask (`approvals.mode: manual`) |

## How per-agent values reach the config

As for OpenCode (see `../opencode/README.md`): agentd execs the entry script with the agent's
values as arguments; [`koinon-hermes`](koinon-hermes) checks them (no quotes, backslashes, `$`,
braces, whitespace or control characters; URLs must be `http(s)://`), exports them as
`KOINON_MODEL_BASE_URL`, `KOINON_MODEL` and `KOINON_MCP_URL`, sets the environment the
official image's `ENV` would have set (OpenShell drops it) with `HERMES_HOME=/sandbox/.hermes`,
and execs `/opt/hermes/.venv/bin/hermes acp`.

Hermes reads its configuration only from `$HERMES_HOME/config.yaml` (`hermes_constants.py`,
`get_hermes_home`; there is no config-path override), so the entry script links that path to
the read-only [`/etc/koinon/hermes/config.yaml`](etc/koinon/hermes/config.yaml). Hermes expands
`${VAR}` in every string value after parsing the YAML (`hermes_cli/config.py`,
`_expand_env_vars`, applied in `load_config`), and again in MCP `url` and `headers`
(`tools/mcp_tool_config.py`, failing closed if a variable is unset), which is how the
`KOINON_*` values and the key placeholder get in. `_config_version: 51` marks the file as
current, so Hermes does not try to migrate (and rewrite) it.

## Managed scope

Hermes overlays a *managed scope* on every config load: `/etc/hermes/config.yaml`, outside
the home and read-only in the image (`hermes_cli/managed_scope.py`; `hermes_cli/config.py`,
`_merge_managed_overlay`). Each value there wins at its leaf over the user config, and a list
replaces the user's list instead of merging (`_deep_merge`); the managed values are applied
after the user's `${VAR}` expansion and expand against the process environment only. Every
switch that decides the agent's tools is there: `platform_toolsets.acp`,
`agent.disabled_toolsets`, memory, skills and their nudges, the curator, background review,
approvals and the command allowlist, the terminal backend, Python plugins and every shell hook
event. The user config the agent can repoint keeps only the model, the MCP server and the
phone-home switches.

The agent runs as the same user as Hermes and owns its home, so what Hermes reads from there
is the agent's to change:

- **The config link.** Hermes reloads `$HERMES_HOME/config.yaml` whenever its signature changes,
  and falls back to defaults or to a last-known-good copy in `backups/config/` when it is
  missing or broken (`hermes_cli/config.py`, `_load_config_impl`, `_last_known_good_fallback`).
  Every one of those paths applies the managed overlay. The entry script also re-links the
  file on every start.
- **The environment.** `HERMES_MANAGED_DIR` moves the managed scope and `PYTEST_CURRENT_TEST`
  turns `/etc/hermes` off (`get_managed_dir`, `_under_pytest`). The entry script unsets every
  `HERMES_*` variable it does not set itself, and `PYTEST_CURRENT_TEST`.
- **Files Hermes loads at start.** `.env` and `.op.env` override the environment
  (`hermes_cli/env_loader.py`, `load_hermes_dotenv`), `active_profile` moves `HERMES_HOME` to a
  profile (`hermes_cli/main.py`, `_apply_profile_override`), `distribution.yaml` makes Hermes
  treat SOUL.md as untrusted, and `shell-hooks-allowlist.json` is consent for shell hooks. The
  entry script deletes them on every start.
- **A broken managed file.** Hermes logs a warning and ignores a managed file that does not
  parse (fail-open, `_cached_read`). The entry script reads it with Hermes's own Python and
  loader from where Hermes looks, and refuses to start unless it holds the toolset allowlist;
  the image build also checks that it pins every shell hook event this Hermes knows.

The contract suite (`test/flavors/hermes_test.go`) plants a permissive config, repoints the link
through the terminal tool mid-run, plants `.env`, `.op.env` and `active_profile`, and sets
`HERMES_MANAGED_DIR` and `PYTEST_CURRENT_TEST`, and checks that the tools on offer stay the
same; a control with the entry script's guards taken out shows the same plants do widen them.

**A process from an earlier run.** A `.env` written *after* the entry script has run applies to
the next Hermes process that loads it, and `hermes acp` attaches to a gateway daemon that
outlives the harness while idle for less than `gateway.unmanaged_idle_exit_seconds` (600 s),
discovered through `gateway.lock` and the control socket in `$HERMES_HOME`
(`hermes_cli/gateway_client.py`, `connect_gateway`; `hermes_cli/gateway_runtime.py`,
`ensure_gateway_runtime`). A background job left from an earlier run could write `.env` between
the entry script and the harness, or a stale daemon serve the next run. koinon-agentd ends every
process of the sandbox user but the sandbox's main process before it starts the harness and
after the harness has ended (`../README.md`, `internal/agentd/sweep.sh`), so neither survives
into a run. The contract suite leaves a job that keeps writing a `.env` moving the managed scope
to the agent's permissive config, runs the next ticket through koinon-agentd, and checks that
the tools stay the same; without the sweep they do not (the switched-off memory, skills, web,
browser and delegation tools are offered). A run's own turn cannot do it, since `.env` is read
before the turn starts and each run sends one prompt.

## Instructions

Hermes builds the system prompt from `$HERMES_HOME/SOUL.md` (identity slot,
`agent/system_prompt.py`, `_identity_parts`; `agent/prompt_builder.py`, `load_soul_md`). The
entry script links it to `/sandbox/.koinon/AGENTS.md`, which koinon-agentd writes from the
run's agent version, and koinon-agentd checks and restores the link before every prompt (event
`run.instructions_restored`). Hermes skips a missing or empty SOUL.md silently and seeds its
template (through the link) only when the target is missing (`hermes_cli/config.py`,
`ensure_hermes_home`), so the entry script refuses to start without real content.

- **A loaded session keeps its instructions.** Hermes stores a session's system prompt and
  reuses it on `session/load` (`agent/conversation_loop.py`), so a new agent version's
  instructions apply from the ticket's next new session, not to a resumed one.
- Hermes scans SOUL.md for prompt-injection patterns and, for a file in the user's own home,
  only logs a hit.
- **Repository context files stay out.** Hermes would open the system prompt with the first
  of `.hermes.md`/`HERMES.md` (up to the git root), the `AGENTS.md` chain, `CLAUDE.md` or
  `.cursorrules` and `.cursor/rules/*.mdc` it finds from the session directory
  (`agent/prompt_builder.py`, `build_context_files_prompt`), and add those of a subdirectory
  to tool results once a tool touches it (`agent/subdirectory_hints.py`). Koinon ignores
  repository harness configuration; its instructions tell the agent to read `AGENTS.md`,
  `CONTRIBUTING.md` and the README as content. See [Context-file patch](#context-file-patch).

## Context-file patch

Hermes skips context files only for an agent built with `skip_context_files`, and keeps
SOUL.md then only with `load_soul_identity` (`agent/system_prompt.py`, `_identity_parts` and
`_context_files_part`). No setting gives an ACP session that pair: the gateway builds it with
`skip_context_files=ignore_rules` and `load_soul_identity=not ignore_rules`
(`gateway/session_execution.py`, `_build_fresh_agent`), and `ignore_rules` (`--ignore-rules`,
`HERMES_IGNORE_RULES`) would drop SOUL.md with the context files. There is no managed-scope
key; `gateway.platforms.<platform>.skip_context_files` is read only for a session without a
frozen policy, which an ACP session always has.

So the image patches Hermes at build time ([`no-context-files.py`](no-context-files.py)): two
lines in `agent/agent_init.py`, `init_agent`, after the safe-worker block, make every agent
skip context files and load SOUL.md wherever it would have before:

```python
load_soul_identity = load_soul_identity or not skip_context_files
skip_context_files = True
```

That also turns off the subdirectory hints and `skills.auto_load` (already empty). The build
fails if the code to patch is not there exactly once (a new Hermes version), and then builds an
agent the way the gateway builds an ACP session and checks its system prompt: SOUL.md in, a
repository's `.hermes.md`, `AGENTS.md`, `CLAUDE.md` and `.cursorrules` out, hints off. The
contract suite checks the same over ACP for each context file, alone in a session directory.

`hermes acp` is a thin ACP front end: it starts Hermes's gateway daemon in the sandbox
(`hermes gateway run --idle-exit`, `hermes_cli/gateway_runtime_start.py`) and talks to it over a
loopback WebSocket and a Unix socket in `$HERMES_HOME`. The daemon inherits the entry script's
environment and exits when idle. It runs no messaging platform: none is configured and no
platform token is in the environment (the log says `No messaging platforms enabled`).

## Switches

Every switch below except the phone-home ones and the context-file patch is in the managed
scope.

Sources are Hermes Agent `v0.22.3` (commit `5b147ab036d6d19131bff8d20701b5001eab844a`, from the
image's `/opt/hermes/install-stamp.json`); paths are relative to `/opt/hermes` in the image and
the repository root upstream.

| What | Switch | Source |
| --- | --- | --- |
| Messaging gateway (Telegram, Discord, Slack, …) | none configured; `koinon-hermes` unsets the API server and bot token variables. A platform connects only when enabled and given a token | `gateway/config.py` (`get_connected_platforms`, `_is_platform_connected`) |
| Memory and user profile written by the agent | `memory.memory_enabled: false`, `memory.user_profile_enabled: false`, `memory.provider: ""`, toolset `memory` disabled | `hermes_cli/config_defaults.py` (`memory`), `agent/agent_init.py` |
| Memory nudges | `memory.nudge_interval: 0` | `hermes_cli/config_defaults.py`, `agent/agent_init.py` |
| Skill creation and editing (`skill_manage`) and nudges | toolset `skills` disabled, `skills.creation_nudge_interval: 0`, no `external_dirs`, `auto_load` or project skills; `.no-bundled-skills` keeps the copied bundled skills to the essential set | `toolsets.py` (`skills`), `agent/agent_init.py`, `tools/skills_sync.py` |
| Skill curator | `curator.enabled: false` | `hermes_cli/config_defaults.py` (`curator`) |
| Background self-review of finished turns | `auxiliary.background_review.enabled: false` | `hermes_cli/config_defaults.py`, `agent/background_review.py` |
| Session search and recall | toolset `session_search` disabled | `toolsets.py` |
| Web, browser, subagents, code execution, cron jobs | not in `platform_toolsets.acp`, and in `agent.disabled_toolsets` | `gateway/session_policy.py`, `hermes_cli/tools_config.py` |
| MCP resource and prompt helper tools | `mcp_servers.koinon.tools: {resources: false, prompts: false}` | `tools/mcp_tool_registration.py` |
| Dangerous terminal commands | `approvals.mode: manual` (always ask over ACP; no guardian model approves on its own), `approvals.deny: []`, `command_allowlist: []` | `tools/approval_context.py`, `tools/approval.py` |
| Python plugins and shell hooks | `plugins.enabled: []`; every shell hook event pinned to `[]`, `hooks_auto_accept: false` | `hermes_cli/plugins.py`, `agent/shell_hooks.py` |
| Remote terminal backends | `terminal.backend: local` | `hermes_cli/config_defaults.py` |
| Update check | `updates.check: false` | `hermes_cli/source_check.py` |
| Plugin update check | `plugins.auto_update_check_hours: 0` | `hermes_cli/config_defaults.py` |
| Shared metrics (telemetry) | `telemetry.shared_metrics.enabled/send: false` (also the default) | `hermes_cli/config_defaults.py` |
| Repository context files (`AGENTS.md`, `.hermes.md`, `CLAUDE.md`, `.cursorrules`) in the prompt or tool results | build-time patch: every agent `skip_context_files`, SOUL.md kept through `load_soul_identity` | `agent/agent_init.py` (`init_agent`), `agent/system_prompt.py`, `agent/subdirectory_hints.py` |
| Lazy package installs | `security.allow_lazy_installs: false` | `hermes_cli/config_defaults.py` |
| Browser and Chromium download | only on `hermes acp --setup-browser`; the browser toolset is off | `acp_adapter/entry.py` |
| Writes outside the home | `HERMES_WRITE_SAFE_ROOT=/sandbox`; `TMPDIR=/tmp`, `XDG_RUNTIME_DIR=/tmp/hermes-runtime`, `PYTHONDONTWRITEBYTECODE=1` | `agent/file_safety.py`, `hermes_constants.py`, `libexec/hermes-acp` |

Run with `--network none`, the daemon's logs show no request other than to the model and MCP
endpoints.

## Notes

- **models.dev**: Hermes may read the models.dev catalog for model metadata
  (`agent/models_dev.py`) and has no switch for it; it falls back to its cache and backs off
  when the sandbox policy refuses the request. Not seen in the contract run.
- **Unauthenticated probes**: to size the context window Hermes sends `GET /v1/props` and
  `GET /v1/models` to the model endpoint, partly without the key, when the host looks local
  (`agent/model_metadata.py`, `detect_local_server_type`). They carry no credential;
  agentgateway answers 401. Setting `model.context_length` would stop them but fixes the
  window for every model, so it is left unset.
- The image's s6 supervisor, entrypoint and `hermes` user (UID 10000) are unused; the sandbox
  runs as 10001. `/opt/data`, the image's declared volume, is unused.
- Hermes is pre-installed in the official image; Koinon verifies it by the image digest and
  `hermes --version` at build time rather than by downloading it.
