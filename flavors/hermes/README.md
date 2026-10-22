# Hermes flavor

[Hermes Agent](https://github.com/NousResearch/hermes-agent) 0.22.3 driven over ACP
(`hermes acp`) by `koinon-agentd` through OpenShell's non-TTY exec (ADR 0035, 0048, 0049). The
image is the official `nousresearch/hermes-agent:v0.22.3`, pinned by digest, with Koinon's
entry script and read-only config added. It holds no Koinon binary.

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
| Permission requests | Hermes asks over ACP before every `write_file` and `patch` (`acp_adapter/edit_approval.py`), offering `allow_once` and `reject_once`; the tool kind is `edit`, which koinon-agentd's default mapping allows. Dangerous terminal commands also ask |

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

`hermes acp` is a thin ACP front end: it starts Hermes's gateway daemon in the sandbox
(`hermes gateway run --idle-exit`, `hermes_cli/gateway_runtime_start.py`) and talks to it over a
loopback WebSocket and a Unix socket in `$HERMES_HOME`. The daemon inherits the entry script's
environment and exits when idle. It runs no messaging platform: none is configured and no
platform token is in the environment (the log says `No messaging platforms enabled`).

## Switches

Sources are Hermes Agent `v0.22.3` (commit `5b147ab036d6d19131bff8d20701b5001eab844a`, from the
image's `/opt/hermes/install-stamp.json`); paths are relative to `/opt/hermes` in the image and
the repository root upstream.

| What | Switch | Source |
| --- | --- | --- |
| Messaging gateway (Telegram, Discord, Slack, …) | none configured; `koinon-hermes` unsets the API server and bot token variables. A platform connects only when enabled and given a token | `gateway/config.py` (`get_connected_platforms`, `_is_platform_connected`) |
| Memory and user profile written by the agent | `memory.memory_enabled: false`, `memory.user_profile_enabled: false`, `memory.provider: ""`, toolset `memory` disabled | `hermes_cli/config_defaults.py` (`memory`), `agent/agent_init.py` |
| Memory nudges | `memory.nudge_interval: 0` | `hermes_cli/config_defaults.py`, `agent/agent_init.py` |
| Skill creation and editing (`skill_manage`) and nudges | toolset `skills` disabled, `skills.creation_nudge_interval: 0`; `.no-bundled-skills` keeps the copied bundled skills to the essential set | `toolsets.py` (`skills`), `agent/agent_init.py`, `tools/skills_sync.py` |
| Skill curator | `curator.enabled: false` | `hermes_cli/config_defaults.py` (`curator`) |
| Background self-review of finished turns | `auxiliary.background_review.enabled: false` | `hermes_cli/config_defaults.py`, `agent/background_review.py` |
| Session search and recall | toolset `session_search` disabled | `toolsets.py` |
| Web, browser, subagents, code execution, cron jobs | not in `platform_toolsets.acp`, and in `agent.disabled_toolsets` | `gateway/session_policy.py`, `hermes_cli/tools_config.py` |
| MCP resource and prompt helper tools | `mcp_servers.koinon.tools: {resources: false, prompts: false}` | `tools/mcp_tool_registration.py` |
| Update check | `updates.check: false` | `hermes_cli/source_check.py` |
| Plugin update check | `plugins.auto_update_check_hours: 0` | `hermes_cli/config_defaults.py` |
| Shared metrics (telemetry) | `telemetry.shared_metrics.enabled/send: false` (also the default) | `hermes_cli/config_defaults.py` |
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
