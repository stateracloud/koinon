# OpenCode flavor

[OpenCode](https://github.com/anomalyco/opencode) 1.19.3 driven over ACP (`opencode acp`) by
`koinon-agentd` through OpenShell's non-TTY exec (ADR 0035, 0048, 0049). The image holds no
Koinon binary.

Build: `make flavor-opencode` (loads `koinon/flavor-opencode:dev`; nothing is pushed).
Contract test: `make test-flavors` (`test/flavors`, needs Docker).

## Contract

| Item (ADR 0035) | OpenCode flavor |
| --- | --- |
| ACP command | `/usr/local/bin/koinon-opencode --model-base-url URL --model NAME --mcp-url URL`, run as UID 10001 in a session working directory under `/sandbox` |
| Resume mode | `session/load` (replays history). OpenCode also advertises `session/resume`; Koinon does not rely on it |
| Model endpoint | `--model-base-url` (agentgateway's OpenAI-compatible base, ending in `/v1`) and `--model` (the gateway's model name); key from `$OPENAI_API_KEY` |
| MCP | `--mcp-url` (agentgateway's MCP route), server `koinon` in OpenCode's own config, `Authorization: Bearer $OPENAI_API_KEY`; tools reach the model as `koinon_<tool>` |
| Instruction files | `/sandbox/.koinon/AGENTS.md` (Koinon's rendered template instructions, via `instructions`), plus the repository's `AGENTS.md` found from the session directory |
| State | `/sandbox/.local/share/opencode` (sessions in `opencode.db`), `/sandbox/.local/state/opencode` |
| Home exclude paths | `/sandbox/.cache/opencode`, `/sandbox/.local/share/opencode/log` |
| Permission requests | OpenCode's defaults: it asks over ACP for paths outside the session directory (`external_directory`), `.env` reads and repeated identical tool calls (`doom_loop`); everything else is allowed inside the sandbox |

## How per-agent values reach the config

OpenShell passes neither the image's `ENV` nor, through Koinon's `ExecutionBackend`, any
environment to exec'd processes (ADR 0048 finding 1; the backend has no environment field by
design). So the values travel as **exec arguments**, which `koinon-agentd` already controls:

1. agentd execs `koinon-opencode --model-base-url … --model … --mcp-url …` with the agent's
   values from Postgres.
2. The entry script checks each value (no quotes, backslashes, braces, whitespace or control
   characters; URLs must be `http(s)://`), exports it as `KOINON_MODEL_BASE_URL`,
   `KOINON_MODEL` and `KOINON_MCP_URL`, sets `PATH`, `HOME` and the switches below, and execs
   `/usr/local/bin/opencode acp`.
3. [`etc/koinon/opencode/opencode.jsonc`](etc/koinon/opencode/opencode.jsonc), read-only at
   `/etc/koinon/opencode/`, reads them through OpenCode's `{env:NAME}` substitution, which is
   textual and happens before the JSON is parsed (`packages/opencode/src/config/variable.ts`);
   hence the character check.

The values are not secret. The key is never an argument: in a sandbox `$OPENAI_API_KEY` is the
OpenShell provider placeholder that the proxy swaps for the agent's agentgateway key.

`XDG_CONFIG_HOME=/etc/koinon` makes `/etc/koinon/opencode` OpenCode's global config directory
(`packages/core/src/global.ts`), so no `OPENCODE_CONFIG` is needed.

## Phone-home, updates, telemetry and plugins

Spike 7 saw OpenCode reach for `opencode.ai`, `models.opencode.ai` and `registry.npmjs.org`
(refused by policy). Each is switched off; sources are OpenCode `v1.19.3`
(commit `e3c4eb242585dcd77be1a867391b7c25fd47ce52`) and its
[CLI docs](https://github.com/anomalyco/opencode/blob/v1.19.3/packages/web/src/content/docs/cli.mdx#environment-variables).

| What | Switch | Source |
| --- | --- | --- |
| Update check and self-upgrade (`opencode.ai/install`) | `OPENCODE_DISABLE_AUTOUPDATE=1`, `"autoupdate": false` | `packages/opencode/src/cli/upgrade.ts`, `packages/opencode/src/installation/index.ts` |
| Model catalog fetch (`models.opencode.ai`) | `OPENCODE_DISABLE_MODELS_FETCH=1` | `packages/core/src/models-dev.ts` |
| npm install of `@opencode-ai/plugin` into each config directory (`registry.npmjs.org`) | global config directory is read-only; the installer skips unwritable directories | `packages/opencode/src/config/config.ts` (`npmSvc.install`), `packages/core/src/npm.ts` (`canWrite`) |
| External plugins from config | `OPENCODE_PURE=1` | `packages/opencode/src/plugin/index.ts` |
| Built-in provider auth plugins (Copilot, Codex, GitLab, …) | `OPENCODE_DISABLE_DEFAULT_PLUGINS=1` | `packages/opencode/src/plugin/index.ts` |
| LSP server downloads | `OPENCODE_DISABLE_LSP_DOWNLOAD=1` | `packages/opencode/src/effect/runtime-flags.ts` |
| ripgrep download from GitHub | `ripgrep` installed; OpenCode uses `rg` from `PATH` | `packages/core/src/ripgrep/binary.ts` |
| Session sharing | `"share": "disabled"`; `OPENCODE_AUTO_SHARE` unset | `packages/core/src/v1/config/config.ts` |
| OpenCode's own providers (Zen, title model) | `"enabled_providers": ["koinon"]`, `small_model` = the agent's model | `packages/core/src/v1/config/config.ts` |
| `~/.claude` prompts and skills | `OPENCODE_DISABLE_CLAUDE_CODE=1` | `packages/opencode/src/effect/runtime-flags.ts` |
| Telemetry | OpenCode has no telemetry switch; it exports OpenTelemetry only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, which the entry script unsets | `packages/core/src/flag/flag.ts` |

With these, the contract run's OpenCode log shows no outbound request other than to the model
and MCP endpoints.

## Notes

- Project configuration stays on (`OPENCODE_DISABLE_PROJECT_CONFIG` would also hide the
  repository's `AGENTS.md`), so a repository's `opencode.json` or `.opencode/` can change the
  harness's settings. The sandbox policy and agentgateway, not OpenCode, are the boundary.
- OpenCode is the glibc binary from npm (`opencode-linux-<arch>`), verified by sha256 at build
  time (`ADD --checksum`); the sha512 npm integrity values are in the Dockerfile.
- Ubuntu packages (`ca-certificates`, `git`, `ripgrep`) follow the archive at build time; the base
  image is pinned by digest.
