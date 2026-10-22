# OpenCode flavor

[OpenCode](https://github.com/anomalyco/opencode) 2.0.24 driven over ACP (`opencode acp`) by
`koinon-agentd` through OpenShell's non-TTY exec (ADR 0035, 0048, 0049, 0052). The image holds no
Koinon binary.

Build: `make flavor-opencode` (loads `koinon/flavor-opencode:dev`; nothing is pushed).
Contract test: `make test-flavors` (`test/flavors`, needs Docker).

Source references below are to OpenCode
[`v2.0.24`](https://github.com/anomalyco/opencode/tree/v2.0.24) (commit
`e7a34f09bfd9134dfade5a8ddb843f7030bc9a69`). The pin and its workarounds are in ADR 0052; moving
to a later 2.0.x is issue [#85](https://github.com/procoduck/koinon/issues/85).

## Contract

| Item (ADR 0035) | OpenCode flavor |
| --- | --- |
| ACP command | `/usr/local/bin/koinon-opencode --model-base-url URL --model NAME --mcp-url URL [--plugin-installs]`, run as UID 10001 in a session working directory under `/sandbox`. agentd adds `--plugin-installs` only for an agent whose template sets `harness.pluginInstalls: true` (see [Plugins](#plugins)) |
| Instructions | `/sandbox/.koinon/AGENTS.md`, written by koinon-agentd before every harness start from the run's agent version, and checked before every prompt. The entry script refuses to start (exit 64) when it is missing, empty or a symbolic link |
| Session start | After `session/new` or `session/load`, agentd waits for the command `koinon:koinon-ready`, then selects the model with `session/set_config_option` (`configId` `model`, value `koinon/<model>`) |
| Resume mode | `session/load` (replays history). OpenCode also advertises `session/resume`; Koinon does not rely on it |
| Model endpoint | `--model-base-url` (agentgateway's OpenAI-compatible base, ending in `/v1`) and `--model` (the gateway's model name); key from `$OPENAI_API_KEY` |
| MCP | `--mcp-url` (agentgateway's MCP route), server `koinon` in OpenCode's own config, `Authorization: Bearer <key>`; tools reach the model one by one as `koinon_<tool>` (`codemode: false`) |
| Project configuration | Off (`OPENCODE_DISABLE_PROJECT_CONFIG=1`): a repository's `opencode.json(c)`, `.opencode/`, `.claude/`, `.agents/` and its top-level `AGENTS.md` have no effect |
| State | `/sandbox/.local/share/opencode` (sessions in `opencode.db`), `/sandbox/.local/state/opencode` |
| Home exclude paths | `/sandbox/.cache/opencode`, `/sandbox/.local/share/opencode/log` |
| Permission requests | OpenCode's defaults: it asks over ACP for paths outside the session directory (permission `external_directory`, except its own global config directory), `.env` reads and repeated identical tool calls (`doom_loop`); everything else is allowed inside the sandbox. A read outside the workspace arrives with tool kind `read` and the path in `locations`; koinon-agentd refuses requests for paths outside the workspace in every preset |

## How per-agent values reach the config

OpenShell passes neither the image's `ENV` nor, through Koinon's `ExecutionBackend`, any
environment to exec'd processes (ADR 0048 finding 1; the backend has no environment field by
design). So the values travel as **exec arguments**, which `koinon-agentd` already controls:

1. agentd execs `koinon-opencode --model-base-url … --model … --mcp-url …` with the agent's
   values from Postgres.
2. The entry script checks each value (no quotes, backslashes, braces, whitespace or control
   characters; URLs must be `http(s)://`), exports it as `KOINON_MODEL_BASE_URL`,
   `KOINON_MODEL` and `KOINON_MCP_URL`, sets `PATH`, `HOME`, the XDG directories and the
   switches below, and execs `/usr/local/bin/opencode acp`.
3. [`etc/koinon/opencode/opencode.jsonc`](etc/koinon/opencode/opencode.jsonc), read-only at
   `/etc/koinon/opencode/`, reads them through OpenCode's `{env:NAME}` substitution, which is
   textual and happens before the JSON is parsed (`packages/core/src/config/variable.ts`,
   `packages/core/src/config.ts`); hence the character check.

`OPENCODE_CONFIG_DIR=/etc/koinon/opencode` makes that directory OpenCode's global config
(`packages/util/src/global.ts`). `XDG_CONFIG_HOME` points into the home
(`packages/util/src/global-roots.ts`), so `~/.config/opencode` is not a config directory.

The values are not secret. The key is never an argument: in a sandbox `$OPENAI_API_KEY` is the
OpenShell provider placeholder that the proxy swaps for the agent's agentgateway key. The entry
script moves it to `KOINON_MODEL_KEY` and unsets `OPENAI_API_KEY`, because OpenCode activates
any built-in provider whose environment variable is set (`packages/core/src/integration.ts`), and
the built-in `openai` provider reads that one.

## Instructions

OpenCode 2.0.24 does not read the `instructions` config key (it is in the schema,
`packages/schema/src/config.ts`, but nothing consumes it). It does load `AGENTS.md` from the
global config directory into the system prompt as `Instructions from: <path>`
(`packages/core/src/config/plugin/instruction.ts`, `packages/core/src/instruction-discovery.ts`).
The image links `/etc/koinon/opencode/AGENTS.md` to `/sandbox/.koinon/AGENTS.md`; OpenCode
resolves the link and reports the target's path.

- **Per run.** koinon-agentd writes the file before each harness start from the run's agent
  version (`agentconfig.Parse(version.Config).Instructions` with a short Koinon preamble), so an
  agent version bump takes effect at the next run without recreating the sandbox.
- **Edits.** OpenCode watches the file and sends a change with the next turn as a
  `<system-update>` ("The instructions changed: …"; a deleted file becomes "Previously loaded
  instructions no longer apply."). The agent can write the file, so agentd checks its hash before
  every prompt and restores it (event `run.instructions_restored`). It checks the link the same
  way; in the read-only image it cannot change.
- **Missing or empty.** OpenCode skips a missing file silently and loads an empty one as empty
  instructions, so the entry script refuses to start without real content.
- **Repository guidance** is content, not configuration: with project configuration off, the
  repository's top-level `AGENTS.md` is not loaded (`packages/server/src/routes.ts`), and Koinon's
  template instructions tell the agent to read it, `CONTRIBUTING.md` and the `README` before
  changing code (`templates/repo-helper.yaml`). OpenCode still attaches an `AGENTS.md` from a
  subdirectory when the agent reads a file there (`packages/core/src/tool/plugin/read.ts`).

## MCP readiness

OpenCode connects configured MCP servers after `session/new` returns and does not hold the first
turn for them (`packages/core/src/mcp/index.ts`: "Initial connections stay asynchronous"), and ACP
has no MCP status notification. It does turn MCP prompts into commands named
`<server>:<prompt>` (`packages/core/src/plugin/command.ts`) and announces commands with
`available_commands_update` (`packages/cli/src/acp/sessions.ts`). Koinon's MCP server publishes
one prompt, `koinon-ready`, which agentgateway admits for every key, so `koinon:koinon-ready`
appears once the server is connected. agentd waits for it (bounded; a timeout fails the run),
then waits one more second: OpenCode registers the server's tools after a 100 ms debounce
(`packages/core/src/tool/mcp.ts`), separately from the prompt path.

## Model selection

A new session reports a model OpenCode picked from its own catalog (for example
`opencode/fledge-alpha-free`) until one is selected. agentd always selects `koinon/<model>` with
`session/set_config_option` (`packages/cli/src/acp/config-option.ts`; OpenCode has no
`session/set_model`) and fails the run unless the answer shows that model.

## Phone-home, updates, telemetry and plugins

| What | Switch | Source |
| --- | --- | --- |
| Update check and self-upgrade (`opencode.ai`) | `OPENCODE_DISABLE_AUTOUPDATE=1`, `"update": "disable"` | `packages/cli/src/services/updater.ts` |
| Model catalog refresh (`models.opencode.ai`) | `OPENCODE_DISABLE_MODELS_FETCH=1`; the bundled snapshot is used | `packages/cli/src/server-process.ts`, `packages/core/src/models-dev.ts` |
| Project configuration from the session directory upward | `OPENCODE_DISABLE_PROJECT_CONFIG=1` | `packages/cli/src/server-process.ts`, `packages/core/src/config/discovery.ts` |
| Skills from `~/.claude` and `~/.agents` | plugin `-opencode.config.compatibility` | `packages/core/src/config/plugin/compatibility.ts` |
| Built-in provider plugins (Copilot, Bedrock, OpenRouter, …) | plugin `-opencode.provider.*`; `opencode.provider.opencode` is guarded and stays (see below) | `packages/core/src/plugin/provider/`, `packages/core/src/plugin/internal.ts` |
| Web search | plugins `-opencode.websearch.*`, `-opencode.tool.websearch` | `packages/core/src/tool/plugin/websearch.ts` |
| Logins for well-known integrations | plugin `-opencode.wellknown` | `packages/core/src/wellknown/plugin.ts` |
| ripgrep download from GitHub | `ripgrep` installed; OpenCode uses `rg` from `PATH` | `packages/core/src/ripgrep/binary.ts` |
| Telemetry | OpenTelemetry export only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, which the entry script unsets | `packages/util/src/observability/otlp.ts` |
| Session sharing | `"share": "disabled"` (2.0.24 has no sharing runtime) | `packages/schema/src/config.ts` |

Plugin negations (`-id`, `-prefix.*`) are parsed in `packages/core/src/config/plugin/source.ts`
and matched in `packages/core/src/plugin/supervisor.ts`. The entry script also unsets every
variable that would move OpenCode's config, database or server credentials (`OPENCODE_CONFIG*`,
`OPENCODE_DB`, `OPENCODE_PASSWORD`, `OPENCODE_SERVER_PASSWORD`, …; `packages/cli/src/env.ts`,
`packages/cli/src/server-process.ts`, `packages/cli/src/database-path.ts`).

`opencode acp` talks to its own server (`opencode serve --stdio --port 0` on 127.0.0.1,
`packages/cli/src/services/standalone.ts`) over HTTP with a client that honours `HTTPS_PROXY`, so
the entry script adds `127.0.0.1,localhost` to `NO_PROXY`; without it `acp` fails with "OpenCode
server is unavailable" (`packages/cli/src/acp/error.ts`).

OpenCode's built-in `opencode` provider cannot be removed and lists free models (`*-free`,
`big-pickle`) from the bundled snapshot (`packages/core/src/plugin/provider/opencode.ts`,
`packages/core/src/models-dev/snapshot.txt`). They are unreachable: the sandbox policy denies
`opencode.ai`, and agentd selects the koinon model explicitly (ADR 0052, accepted risk).

With these, the contract run shows no outbound request other than to the model and MCP
endpoints.

## Plugins

Plugin installs are a template option, off by default (`spec.harness.pluginInstalls`,
`templates/README.md`).

| | Off (default) | On |
| --- | --- | --- |
| Entry script | no plugin list | lists each directory under `/sandbox/.config/opencode/plugins/` in `OPENCODE_CONFIG_CONTENT` (`{"plugins": [...]}`) |
| Plugins that load | none besides OpenCode's built-ins, minus those turned off above | those directories (an OpenCode v2 plugin is a directory whose module default-exports `{id, setup}` or `{id, effect}`); nothing from `~/.opencode` or a repository |

The global config directory is read-only and holds no plugins, project configuration is off and
`~/.config/opencode` is no config directory, so the list is the only way in
(`packages/core/src/config.ts` merges `OPENCODE_CONFIG_CONTENT` after the global config;
`packages/core/src/config/plugin/source.ts` takes absolute directories). Plugins run inside
OpenCode with its full privileges (issue #84): a broken one can keep OpenCode from ever getting
ready, which agentd's readiness timeout bounds; the sandbox policy and agentgateway, not
OpenCode, remain the boundary.

## Notes

- OpenCode is the glibc binary from npm (`@opencode/cli-linux-<arch>`), verified by sha256 at
  build time (`ADD --checksum`); the sha512 npm integrity values are in the Dockerfile.
- Ubuntu packages (`ca-certificates`, `git`, `ripgrep`) follow the archive at build time; the base
  image is pinned by digest.
