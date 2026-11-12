# OpenCode harness

[OpenCode](https://github.com/anomalyco/opencode) 2.2.6 driven over ACP (`opencode acp`) by
`koinon-agentd` through OpenShell's non-TTY exec (ADR 0035, 0048, 0049, 0052). The image holds no
Koinon binary.

Build: `make harness-opencode` (loads `koinon/sandbox:dev-<hash>`, a hash of this tree's `harnesses/opencode` and `harnesses/common`, so other worktrees' builds cannot replace it; `hack/harness-tag.sh opencode` prints the tag; nothing is pushed).
Contract test: `make test-harnesses` (`test/harnesses`, needs Docker).

Source references below are to OpenCode
[`v2.2.0`](https://github.com/anomalyco/opencode/tree/v2.2.0) (commit
`1097e2668b1e08ad63bdf0b1c4860b9bc01e420c`). The pin, the waiver of the two-week minimum age and
the remaining workarounds are in ADR 0052 (amendment 2026-10-28). The bundled runtime is Bun 1.4.4.

## Contract

| Item (ADR 0035) | OpenCode harness |
| --- | --- |
| ACP command | `/usr/local/bin/koinon-opencode --model-base-url URL --model NAME --mcp-url URL [--plugin-installs] [--model-format openai|anthropic] [--cache-anthropic|--cache-alibaba] [--thinking none|default|adaptive-summarized|disabled|effort-none] [--session-id RUN_ID] [--no-web-tools] [--model-tries N] [--model-retry-backoff-ms MS] [--model-max-retry-after-ms MS]`, run as UID 10001 in a session working directory under `/sandbox`. agentd always adds the three retry switches, Koinon's retry policy (see [Model retries](#model-retries)). agentd adds `--plugin-installs` only for an agent whose template sets `harness.pluginInstalls: true` (see [Plugins](#plugins)). It always adds `--model-format` (the model's wire format, ADR 0069): `openai` (chat completions, provider entry `koinon`) or `anthropic` (Messages, entry `koinon-anthropic`, which posts to `<gateway>/v1/messages` with the key as `Authorization: Bearer`); `internal/harness` holds the format-to-entry table. It adds `--cache-anthropic` or `--cache-alibaba` for a chat-completions model whose provider needs explicit prompt-cache markers (ADR 0068); the entry script then sets `providers.koinon.compatibility.cacheFormat` through `OPENCODE_CONFIG_CONTENT`. An `anthropic`-format model gets no such switch: OpenCode places up to four `cache_control` breakpoints itself. It always adds `--thinking` (the thinking form Koinon decided for the run, see [Thinking](#thinking-koinon-decides-not-the-model-id-adr-0069-amendments-2026-10-29-and-2026-10-31)). For an OpenRouter model agentd also passes `--session-id` with the run ID, sent as the request body field `session_id` on whichever entry the run uses (OpenRouter sticky routing: a run stays on one upstream, so its cache stays warm). agentd adds `--no-web-tools` for every agent without web egress (all of them today, ADR 0080): the built-in `webfetch` is then not offered to the model (see the table of switched-off features below). The config also caps every tool result at 24576 bytes / 600 lines (`tool_output`); the full text goes to the tool-output directory |
| Instructions | `/sandbox/.koinon/AGENTS.md`, named by the `instructions` key of the read-only config, written by koinon-agentd before every harness start from the run's agent version, and checked before every prompt. The entry script refuses to start (exit 64) when it is missing, empty or a symbolic link |
| Session start | After `session/new` or `session/load`, agentd waits for the command `koinon:github_koinon-ready`, then selects the model with `session/set_config_option` (`configId` `model`, value `<entry>/<model>`: `koinon/<model>` or `koinon-anthropic/<model>`) |
| Resume mode | `session/load` (replays history). OpenCode also advertises `session/resume`; Koinon does not rely on it |
| Model endpoint | `--model-base-url` (agentgateway's LLM base, ending in `/v1`; the `koinon` entry posts to `/chat/completions` and `koinon-anthropic` to `/messages`, ADR 0069) and `--model` (the gateway's model name); key from `$OPENAI_API_KEY` |
| MCP | `--mcp-url` (agentgateway's MCP route), server `koinon` in OpenCode's own config, `Authorization: Bearer <key>`; tools reach the model one by one as `koinon_<tool>` (`codemode: false`) |
| Project configuration | Off (`OPENCODE_DISABLE_PROJECT_CONFIG=1`): a repository's `opencode.json(c)`, `.opencode/`, `.claude/`, `.agents/` and its top-level `AGENTS.md` have no effect |
| State | `/sandbox/.local/share/opencode` (sessions in `opencode.db`), `/sandbox/.local/state/opencode` |
| Home exclude paths | `/sandbox/.cache/opencode`, `/sandbox/.local/share/opencode/log` |
| Permission requests | It asks over ACP for paths outside the session directory (permission `external_directory`, set explicitly in the config, see [Permissions](#permissions); OpenCode's own output, temporary and config directories are exempt), `.env` reads; everything else is allowed inside the sandbox. A read outside the workspace arrives with tool kind `read` and the path in `locations`; koinon-agentd refuses requests for paths outside the workspace in every preset |

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
(`packages/util/src/global.ts:79`, `packages/cli/src/server-process.ts:39`). `XDG_CONFIG_HOME` points into the home
(`packages/util/src/global-roots.ts`), so `~/.config/opencode` is not a config directory.

The values are not secret. The key is never an argument: in a sandbox `$OPENAI_API_KEY` is the
OpenShell provider placeholder that the proxy swaps for the agent's agentgateway key. The entry
script moves it to `KOINON_MODEL_KEY` and unsets `OPENAI_API_KEY`, because OpenCode activates
any built-in provider whose environment variable is set (`packages/core/src/integration.ts`), and
the built-in `openai` provider reads that one.

## Instructions

The config's `instructions` key lists `/sandbox/.koinon/AGENTS.md` (an absolute path, never a URL:
OpenCode fetches those). OpenCode 2.2.0 resolves the entry, reads the file and puts it into the
system prompt as `Instructions from: <path>`
(`packages/core/src/config/plugin/instruction.ts:98-118` `match`, `:120-138` `configuredSource`,
`packages/schema/src/config.ts:94`). 2.0.24 ignored the key, which is why the harness used to link
`/etc/koinon/opencode/AGENTS.md` to the file; the link, `InstructionLink` in `internal/harness` and
its restore check in agentd are not needed for OpenCode any more (agentd keeps the generic
mechanism for a harness that needs one).

- **Per run.** koinon-agentd writes the file before each harness start from the run's agent
  version (`agentconfig.Parse(version.Config).Instructions` with a short Koinon preamble), so an
  agent version bump takes effect at the next run without recreating the sandbox.
- **Edits.** OpenCode watches the file and sends a change with the next turn as a
  `<system-update>` ("The instructions changed: …"; a deleted file becomes "Previously loaded
  instructions no longer apply."). The agent can write the file, so agentd checks its hash before
  every prompt and restores it (event `run.instructions_restored`).
- **Missing or empty.** OpenCode skips a missing file silently and loads an empty one as empty
  instructions, so the entry script refuses to start without real content.
- **Repository guidance** is content, not configuration: with project configuration off, the
  repository's top-level `AGENTS.md` is not loaded (`project` is false in
  `packages/core/src/config/plugin/instruction.ts:39`), and Koinon's template instructions tell the
  agent to read it, `CONTRIBUTING.md` and the `README` before changing code
  (`templates/repo-helper.yaml`). OpenCode still attaches an `AGENTS.md` from a subdirectory when
  the agent reads a file there (`packages/core/src/tool/plugin/read.ts:82-103`, issue #52).
  There is no switch in 2.2.0: the read tool walks up from the file to the project root, exclusive,
  and calls `SessionInstructions.load` unconditionally (`session/instructions.ts`), which publishes the
  file's text as a synthetic `Instructions from: <path>` message. Nothing in that path reads a flag,
  an environment variable or the configuration. A repository's subdirectory file is therefore repository text at instruction weight.
  Koinon does not patch OpenCode. It is a requirement on koinon-harness (#48): subdirectory
  `AGENTS.md` files are returned as data inside the read result, or not at all, never as instructions.

### Global config directory

The harness keeps `OPENCODE_CONFIG_DIR=/etc/koinon/opencode` instead of using `/etc/opencode`.
2.2.0 does read `/etc/opencode` as a *system* directory
(`packages/core/src/config/discovery.ts:27-32`, loaded last as managed settings in
`packages/core/src/config.ts:273-276`), but it does not replace the global directory: with the
variable unset, the global directory is `$XDG_CONFIG_HOME/opencode`, which is
`/sandbox/.config/opencode`, in the agent's home. Its `opencode.jsonc`, `AGENTS.md` and plugin
directories would then load (`packages/core/src/config.ts:298`,
`packages/core/src/config/plugin/instruction.ts:41`), an agent-writable configuration source. With
the variable set, the global directory is the read-only one and the home has no say. The image
holds no `/etc/opencode`, so the system directory is absent and has no effect.

## Permissions

2.0.24 gave every agent `external_directory: ask` (`packages/schema/src/agent.ts:48`) with
`allow` for the data directory's `shell/*/*` and `tool-output/*`, the temporary directory and the
global config directory (`packages/core/src/agent.ts:60-63`). **2.2.0 removed all of them**
(`packages/schema/src/agent.ts:46-51` has only `*` allow and the `.env` rules): the contract run
showed a `read` of `/etc/passwd` completing with no permission request. OpenCode's documentation
(`packages/web/src/content/docs/permissions.mdx:177`) still says it defaults to `ask`. The config
therefore restores the rule (`permissions`, last matching rule wins,
`packages/core/src/permission.ts:78`) and the four exemptions. The explore subagent allows every external directory
(`packages/core/src/plugin/agent.ts:92`), but the config plugin runs after it
(`plugin/internal.ts:224` before `:257`) and appends the top-level `permissions` to every existing
agent, explore included (`packages/core/src/config/plugin/agent.ts:83-91`), so they come last
there too. The config repeats them under `agents.explore.permissions` as defence in depth only. The contract suite's permission specs fail without this
block.

A rejected request ends the turn. ACP's permission response carries an option and no message, and
OpenCode answers a plain reject with `Permission.DeclinedError`, which `assert` turns into a defect
on purpose ("Deliberate defect tunnel", `packages/core/src/permission.ts:241-249`), so the step is
interrupted and `session/prompt` returns `cancelled` (a decline with feedback, or a `deny` rule, are
the only recoverable forms: `CorrectedError`, `BlockedError`; the ACP agent can send neither). A
`deny` rule, or the plugin hook `permission.evaluate` returning `{effect: "deny", message}`, gives a
`BlockedError` with that message, which the model sees as a tool error and the turn goes on. But it
is OpenCode-only, and agentd would no longer see or record the request (a bare `deny` rule shows the
model only "Permission denied"). koinon-agentd therefore keeps `ask` and uses the generic re-prompt: when the
turn ends cancelled after it refused a request it prompts the session again with a note naming the
refused action and the session directory (`internal/agentd/refusal.go`, run event `run.refusal`).
The contract suite's `answers the harness's permission requests` spec covers it.

**Subagents are off for every agent** (old issue 261). OpenCode 2.2.0's `subagent` tool is live by
default (depth 1; `explore` is the one configured subagent). A child session outlives a cancelled
turn (`packages/cli/src/acp/turn.ts:303-311`), so the wall clock (old issue 243) and the wrap-up notice (old issue 183)
would leave work running, and its messages reach agentd only through the private extension
`opencode/session/child_update` (gated on `_meta["opencode/child-session-updates"]`, which agentd
does not advertise), so ADR 0074 traces would miss it. The config therefore ends the top-level
`permissions` with `{action: subagent, resource: "*", effect: deny}` and repeats it under
`agents.explore.permissions`. The action name is the tool name (`packages/core/src/tool/plugin/subagent.ts:17`,
asserted at `:140`). `Tool.snapshot` drops a tool whose last matching rule has resource `*` and
effect deny (`packages/core/src/tool.ts:255`, `:326-329`), so the model is not offered the tool, the
same way it is not offered `webfetch`. Keep the rule last for its action. The contract suite checks
the tool list and that a scripted `subagent` call starts no child. Allowing `explore` per template
waits for agentd to advertise and record child updates and for cancel to cascade to children
(verified by a harness test).

OpenCode's permission gate is not a security boundary: rules saved by an "always" answer live in
the agent-writable `opencode.db` and can turn `ask` into `allow`, and the shell is unrestricted.
OpenShell and Kata are the boundary. koinon-agentd answers each request once and saves nothing.

## MCP readiness

OpenCode connects configured MCP servers after `session/new` returns and does not hold the first
turn for them (`packages/core/src/mcp/index.ts`: "Initial connections stay asynchronous"), and ACP
has no MCP status notification. It does turn MCP prompts into commands named
`<server>:<prompt>` (`packages/core/src/plugin/command.ts`) and announces commands with
`available_commands_update` (`packages/cli/src/acp/sessions.ts`). Koinon's MCP server publishes
one prompt, `koinon-ready`, which agentgateway admits for every key, so `koinon:github_koinon-ready`
appears once the server is connected. agentd waits for it (bounded; a timeout fails the run),
then waits one more second: OpenCode registers the server's tools after a 100 ms debounce
(`packages/core/src/tool/mcp.ts`), separately from the prompt path.

The announcement says nothing about the tools of the MCP servers the agent may use: agentgateway
runs `failOpen`, so a server it cannot reach is silently left out of the tool list. Before it starts
the harness, agentd therefore checks the control plane's health record of those servers (unhealthy,
or failing probes below the threshold; a disabled or removed server only gets the note at session
start). The reconciler's prober refreshes that record every 60 s, so a server that drops between two
probes is not caught by the record alone. So agentd also asks the gateway what it lists to the agent
right now, under the agent's own key (koinon-api holds it and lists the tools; agentd and the sandbox
never see it; ADR 0057 amendment 2026-11-01, old issue 488). A miss, from either, records the run event `run.mcp_server_missing`, waits `MCPRetryDelay`
(75 s by default, a probe interval plus slack) and looks once more. If the servers are back the run
records `run.mcp_server_recovered` and the first turn's root span carries `koinon.mcp.missing_servers`
and `koinon.mcp.missing_attempts=1`. A second miss fails the run before any turn starts: it has no
span, and `run.mcp_server_missing` with `attempt: 2` is its record (old issue 326).

## Model selection

A new session reports a model OpenCode picked from its own catalog (for example
`opencode/fledge-alpha-free`) until one is selected. agentd always selects `<entry>/<model>` with
`session/set_config_option` (`packages/cli/src/acp/config-option.ts`; OpenCode has no
`session/set_model`) and fails the run unless the answer shows that model.

### Two provider entries (ADR 0069)

`opencode.jsonc` has two static providers on the same gateway URL: `koinon`
(`aisdk:@ai-sdk/openai-compatible`, chat completions, `apiKey`) and `koinon-anthropic`
(`aisdk:@ai-sdk/anthropic`, Messages, `authToken`). Both send the agent's key as
`Authorization: Bearer <placeholder>`, the header the sandbox's gateway profile names
(`internal/reconciler`, `GatewayProfile`: `authStyle bearer`, `authorization`), so OpenShell
rewrites the placeholder (it does so in any header) and the gateway's key policy reads the same header. Never set
`apiKey` and `authToken` together (`packages/ai/src/providers/anthropic.ts` refuses it), and the
Anthropic provider's default `x-api-key` is not used. `--model-format anthropic` also sets the
config's default model to `koinon-anthropic/<model>` through `OPENCODE_CONFIG_CONTENT`.

### Thinking: Koinon decides, not the model ID (ADR 0069 amendments 2026-10-29 and 2026-10-31)

OpenCode 2.2.0 gives every Messages request a thinking block of its own accord. When the model ID
parses as Claude 5.1 or later (`supportsThinkingBlockBinding`, `packages/ai/src/protocols/utils/
claude-model.ts`, applied by `applyThinkingBindingDefault` in `anthropic-messages.ts`), the request
carries `thinking: {"type": "adaptive", "block_binding": {...}}` even though the config asks for no
thinking, and `requiredBetaHeaders` adds `thinking-binding-controls-2026-08-01` for it. It also
always adds `interleaved-thinking-2025-05-14`. The regex matches `claude-haiku-5-5` and
`anthropic/claude-haiku-5.5` alike. rc.7 assumed that "the model declares no reasoning, so no
`thinking` block is sent"; that was wrong, and with the beta removed by the gateway direct Anthropic
refused every request (`thinking.adaptive.block_binding: Extra inputs are not permitted`).

Koinon states the choice, so neither the ID nor a provider default decides. koinon-agentd passes
`--thinking` (`internal/harness` `ThinkingArg`) with the form from the model's row in
`internal/thinking` and the agent's setting. Thinking on and off are both sent explicitly (old issue 316):
provider defaults differ per route (direct Anthropic defaults Claude 5.x to `display: omitted`, which
bills the thinking and returns empty thinking blocks; OpenRouter summarizes). The entry script turns
OpenCode's own thinking block off with the model's `compatibility` in `OPENCODE_CONFIG_CONTENT`
(`supportsThinkingBlockBinding: false`, `supportsEffortUpdates: false`, which override the ID
heuristic, `packages/schema/src/model-compatibility.ts`) and puts Koinon's field in the
provider-level `body` overlay, the mechanism that already carries OpenRouter's `session_id`:

| `--thinking` | Format | Request body | Rows |
| --- | --- | --- | --- |
| `adaptive-summarized` | `anthropic` | `thinking: {"type": "adaptive", "display": "summarized"}` | Claude 5.x on, both routes |
| `disabled` | `anthropic` | `thinking: {"type": "disabled"}` | Claude Haiku 5.x off, both routes |
| `effort-none` | `openai` | `reasoning: {"effort": "none"}` | GPT-6 Luna and DeepSeek v4.1 flash off, OpenRouter |
| `default` | either | nothing: the model thinks by default | chat-completions models on |
| `none`, or no switch | either | nothing | models that do not think through Koinon |

The entry script refuses a form on the wrong format (exit 64). A model that always thinks (Claude
Sonnet, Opus and Fable 5.x, GPT-6 Astra) has no off form: the API refuses "off" for it, and a stored
"off" runs as on. No form carries a beta-only field, so the gateway pins no beta and drops the
client's `anthropic-beta`, OpenCode's interleaved-thinking value included.
`test/harnesses/thinking_test.go` runs real model IDs on both formats (Haiku, Sonnet, Opus and Fable
5.x on both Claude routes, Luna and DeepSeek on chat completions, an older Claude and the contract
ID) and asserts the exact body for on and off against the fake model in strict mode, which refuses
a thinking type the model does not take as Anthropic and OpenRouter do.

A session saved under one entry loads under the other (measured with OpenCode 2.2.0, both
directions, `test/harnesses/anthropic_test.go`): the replayed history is complete, a turn with a tool
call included, the model receives it on the next request, and that request is in the new entry's
format. agentd therefore needs no new session when a task's model changes format. OpenCode rewrites
characters outside `[a-zA-Z0-9_-]` in a tool-call ID to `_` for Messages (`scrubToolCallID`), so a
session whose call was `functions.write:0` replays as `functions_write_0`; the spec checks it.

## Phone-home, updates, telemetry and plugins

| What | Switch | Source |
| --- | --- | --- |
| Update check and self-upgrade (`opencode.ai`) | `OPENCODE_DISABLE_AUTOUPDATE=1`, `"update": "disable"` | `packages/cli/src/services/updater.ts` |
| Model catalog refresh (`models.opencode.ai`) | `OPENCODE_DISABLE_MODELS_FETCH=1`; the bundled snapshot is used | `packages/cli/src/server-process.ts`, `packages/core/src/models-dev.ts` |
| Project configuration from the session directory upward | `OPENCODE_DISABLE_PROJECT_CONFIG=1` | `packages/cli/src/server-process.ts`, `packages/core/src/config/discovery.ts` |
| Skills from `~/.claude` and `~/.agents` | plugin `-opencode.config.compatibility` | `packages/core/src/config/plugin/compatibility.ts` |
| Built-in skills `opencode` and `report` (`report` files issues with the GitHub CLI) | plugin `-opencode.skill`; the permission `skill` `*` deny (allow a skill by ID to deliver it) is a second layer | `packages/core/src/plugin/skill.ts`, `packages/core/src/skill.ts` |
| Built-in provider plugins (Copilot, Bedrock, OpenRouter, …) | plugin `-opencode.provider.*`; `opencode.provider.opencode` is guarded and stays (see below) | `packages/core/src/plugin/provider/`, `packages/core/src/plugin/internal.ts` |
| Web search | plugins `-opencode.websearch.*`, `-opencode.tool.websearch` | `packages/core/src/tool/plugin/websearch.ts` |
| Built-in `webfetch`, for an agent without web egress | `--no-web-tools` (set by koinon-agentd unless `agentconfig.Config.WebEgress`, ADR 0080) adds the permission rule `webfetch` `*` deny to the config overlay; a denied tool is not offered to the model, so the agent uses the org's MCP tools, which its instructions name. The contract suite checks the tool list with and without the switch | `packages/core/src/permission.ts` |
| Subagents (`subagent` tool) | the permission rule `subagent` `*` deny in the config, top level and `agents.explore`; a denied tool is not offered to the model. Children outlive a cancelled turn and agentd does not see their events (old issue 261) | `packages/core/src/tool.ts`, `packages/core/src/tool/plugin/subagent.ts` |
| Logins for well-known integrations | plugin `-opencode.wellknown` | `packages/core/src/wellknown/plugin.ts` |
| ripgrep download from GitHub | `rg` installed; OpenCode uses `rg` from `PATH` | `packages/core/src/ripgrep/binary.ts` |
| Telemetry | OpenTelemetry export only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, which the entry script unsets | `packages/util/src/observability/otlp.ts` |
| Session sharing | `"share": "disabled"` (no sharing runtime in 2.0.24; the key is still in the 2.2.0 schema) | `packages/schema/src/config.ts:42` |
| `.claude` roots | `OPENCODE_DISABLE_CLAUDE_CODE=1` (new in 2.1+; the compatibility plugin is off as well) | `packages/cli/src/server-process.ts:127-131` |
| Free models of the built-in `opencode` provider | `OPENCODE_DISABLE_FREE_MODELS=1` (new in 2.1+) | `packages/core/src/plugin/provider/opencode.ts:164`, `packages/core/src/model.ts:99` |

Plugin negations (`-id`, `-prefix.*`) are parsed in `packages/core/src/config/plugin/source.ts`
and matched in `packages/core/src/plugin/supervisor.ts`. The entry script also unsets every
`OPENCODE_*` variable and `TMPDIR` before its own exports (OpenCode's temporary directory follows
`os.tmpdir()`, and the config exempts `/tmp/opencode/*`), so nothing can move OpenCode's config,
database, binaries or server credentials (for example `OPENCODE_CONFIG*`,
`OPENCODE_DB`, `OPENCODE_PASSWORD`, `OPENCODE_SERVER_PASSWORD`, …; `packages/cli/src/env.ts`,
`packages/cli/src/server-process.ts`, `packages/cli/src/database-path.ts`).

`opencode acp` talks to its own server (`opencode serve --stdio --port 0` on 127.0.0.1,
`packages/cli/src/services/standalone.ts`) over HTTP with a client that honours `HTTPS_PROXY`, so
the entry script adds `127.0.0.1,localhost` to `NO_PROXY`; without it `acp` fails with "OpenCode
server is unavailable" (`packages/cli/src/acp/error.ts`).

OpenCode's built-in `opencode` provider cannot be removed (the plugin is guarded). Its free models
(`*-free`, `big-pickle`) are hidden by `OPENCODE_DISABLE_FREE_MODELS=1`; the provider itself stays
and is unreachable anyway: the sandbox policy denies `opencode.ai`, and agentd selects the koinon
model explicitly. The entry script also unsets `OPENCODE_API_KEY`, which would activate it.

With these, the contract run shows no outbound request other than to the model and MCP
endpoints.

## Model retries

Exactly one layer retries a failed model request, with Koinon's policy (ADR 0056 amendment
2026-10-31, point 3; old issue 341). For this harness that layer is OpenCode: the harness table sets
`RetriesModelRequests` (`internal/harness`), so koinon-agentd does not repeat the turn, and it
launches the entry script with the policy from its own config (`ModelRetries`, `RetryBackoff`,
`MaxRetryAfter` in `internal/agentd`):

| Switch | Default | Means |
| --- | --- | --- |
| `--model-tries N` | 3 | tries per model request, the first included (at most 11, OpenCode's own limit) |
| `--model-retry-backoff-ms MS` | 1000 | the first wait; it doubles per retry, up to a minute, with ±20% jitter |
| `--model-max-retry-after-ms MS` | 60000 | the longest `Retry-After` waited out; a longer one ends the retries and the turn fails as throttled |
| `--compaction-tokens N` | none | the prompt size at which OpenCode compacts the conversation (at least 1000); koinon-agentd always passes it (the model's first price tier, at most 170000; ADR 0070 amendment 2026-11-11). Rendered as `limit.input` N+30000 of the model and `compaction.buffer` 30000, because OpenCode (2.2.6 source; the harness spec "koinon-opencode compaction point" checks the shipped harness) never compacts a model whose window is unknown; the 30000 leaves the summary request 14000 tokens above the point (the window less OpenCode's `RESERVE_MIN` of 16000), as OpenCode's own defaults do for a 200000-token window |

OpenCode 2.2.0 has no config key for its retries. Its own schedule
(`packages/core/src/model-retry.ts`) is 10 retries, exponential from 2 s with a 10 s cap per gap,
and a `Retry-After` of up to 15 minutes; with koinon-agentd's former 3 turn retries on top, one failed
step could cost about 44 gateway requests. Before each retry OpenCode asks the plugin hook
`session.retry` (`packages/core/src/session/runner/retry.ts`), with the try about to be made, its
proposed decision and the error, which carries the provider's `Retry-After`
(`packages/core/src/session/to-session-error.ts`). Koinon's plugin `koinon.retry`
([`plugins/koinon-retry/index.js`](plugins/koinon-retry/index.js), installed as
`/usr/local/lib/koinon/opencode/plugins/koinon-retry`) replaces that decision from the switches,
which reach it as the options of its entry in the read-only config's `plugins` list. Koinon never
retries a refusal, as the plugin's own rule: a client error other than 408, 409 and 429 (a bad key,
no access, a malformed request) and anything marked `x-should-retry: false` end after one request. It
also keeps OpenCode's "no" (a failure that is not retryable, the gateway's `x-should-retry: false`,
which OpenCode reads before the hook, a fourth timeout) and only shortens the schedule, never
lengthens it. A refusal the gateway cannot mark, such as the standalone gateway's budget 429 with a
`Retry-After` of hours, ends on the `Retry-After` cap, and koinon-agentd fails the turn as throttled. The directory is outside every config
root, so nothing loads the plugin without its options. No fork of OpenCode: the hook is its
public plugin API.

Worst case for one failed step, with the defaults: 3 gateway requests (the run's request cap and the
ledger count each one), and at most 1 s + 2 s of backoff (±20%), or up to 2 × 60 s when the provider
asks for its `Retry-After`. `test/harnesses/retry_test.go` asserts the count and the waits against the
fake model.

Known limit: if the plugin fails to load (a broken file in the image), OpenCode logs it and runs on
without it (`packages/core/src/plugin/supervisor.ts`), back on its own 10 retries, and koinon-agentd
repeats no turn either. Neither the entry script nor koinon-agentd can see this: OpenCode loads
plugins after the entry script has handed over to it, and reports a load failure only in its log,
not over ACP. The plugin is fixed in the image, so the harness contract suite
(`test/harnesses/retry_test.go`) is what catches it: every image that suite passes loads the plugin
and makes 3 tries, not 11. The request cap still bounds a run that hits it.

What the plugin does not reach: OpenCode's one-shot requests (the session title) retry on their own
path (`retryOneShot`, at most 4 tries and only when the provider clearly did not run the request: a
429, a 503 or 529, an unreachable host), and an agent plugin loaded with `--plugin-installs` runs in
the same process and could register its own `session.retry` hook; the request cap still bounds both.

## koinon.core

`koinon.core` is Koinon's one owned OpenCode plugin (ADR 0093), the first entry of the read-only
config's `plugins` list, before `koinon.retry`
([`plugins/koinon-core/index.ts`](plugins/koinon-core/index.ts), installed as
`/usr/local/lib/koinon/opencode/plugins/koinon-core`). A new customisation of OpenCode's behaviour
goes into this plugin as a module, not as a new ad-hoc plugin. This first slice only registers the
load report command below; the permission, title, request-label and shell-environment modules of
ADR 0093 decisions 3.2-3.5 are later PRs. It is advisory only (ADR 0093 decision 2): every boundary
(egress, credentials, isolation, tool approvals, budgets and request caps) stays in OpenShell and
agentgateway, not in this plugin.

**Loading.** A configured plugin entry must be a directory; OpenCode's `Host.resolve`
(`packages/plugin/src/host.ts`) tries `<dir>/server`, then `<dir>/index`, through Bun's own module
resolution. `index.ts` is therefore the only root module the directory may hold: a later module
goes in a subdirectory, never a second file at the root (a `server.*`, `tui.*`, `rpc.*` or
`gui-experimental.*` name, or a `package.json` with `name` and `exports`, changes what loads
instead). The module uses only `import type` from `@opencode/plugin`, which TypeScript erases at
compile time, so it is `export default { id, setup } satisfies Plugin.Plugin` with no run-time npm
dependency and nothing installed in the sandbox: OpenCode's bundled Bun loads the plain `.ts` file.

**Setup failure.** `setup` runs inside OpenCode's own `Effect.promise`
(`packages/plugin/src/promise/adapter.ts`); a throw (a bad option, or a hook this pin lacks) is
caught per plugin (`packages/core/src/plugin.ts`), closes that plugin's scope — releasing
everything it registered — logs `failed to load plugin`, and lets every other plugin keep running.
A duplicate plugin ID does not stop loading either: the supervisor
(`packages/core/src/plugin/supervisor.ts`) keeps the first of a duplicate ID and fails only the
later one, before any `setup` runs, in built-in-then-configured-document order. `koinon.core` is
listed first and Koinon's config directories hold no other plugin, so an impostor (a user plugin
reusing a Koinon ID, loaded only with `harness.pluginInstalls`, see [Plugins](#plugins) below)
always loses.

**Load report.** `setup` registers an ACP command, `koinon-core-ready-v1` (the contract version is
in the name: agentd keeps only command names from ACP, never descriptions), last, after every
other module's hooks are registered and awaited. A plugin command reaches ACP's
`available_commands_update`, sent per session after the `session/new` (or `load`/`resume`/`fork`)
response and re-sent whenever the command list changes (`packages/cli/src/acp/sessions.ts`,
`catalog.ts`); this is the same channel and the same kind of wait agentd already uses for
`koinon:github_koinon-ready` (ADR 0052 decision 4). A missing report therefore means `koinon.core`
(or a module inside it) failed to load, and agentd's own wait for it (ADR 0093 decision 3.1,
Delivery step 4) is not part of this PR: today only `test/harnesses` checks for the command.

**ADR 0093 corrections**, found reading OpenCode `v2.2.6`
(`dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06`) for this slice; each gets a dated amendment in the ADR:

1. The shell environment hook (decision 3.5) is `ctx.shell.hook("create.before", ...)`, not
   `shell.env`: https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/plugin/src/promise/shell.ts#L3-L17
2. A duplicate plugin ID does not stop loading (decisions 4 and the Context section said it did):
   the later duplicate fails and the first stays, which is why `koinon.core` being listed first is
   enough: https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/core/src/plugin/supervisor.ts#L94-L113
3. The request-purpose hook (decision 3.4) is `model.request` (`kind`, `headers`), not a state kept
   from the compaction hook; the kind comes ready-made as one of four values, `generate` included:
   https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/core/src/session/model-request.ts#L337-L362
4. The permission hook (decision 3.2) can loosen a configured `ask` to `allow` (not only tighten
   `allow` to `ask`); the ADR's module must write only `ask`, never rely on the hook being
   one-directional: https://github.com/anomalyco/opencode/blob/dc2ffcf8bf26f9e669b6aa22b95f50e12be15e06/packages/core/src/permission.ts#L176-L215
5. The types package pin (decision 4) is `@opencode/plugin@2.2.6`, matching the image's OpenCode,
   not `2.2.5`.

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
OpenCode with its full privileges (old issue 84): a broken one can keep OpenCode from ever getting
ready, which agentd's readiness timeout bounds; the sandbox policy and agentgateway, not
OpenCode, remain the boundary.

## Tools in the image

The agent works on repositories and data files and has no network (egress is the model and MCP
endpoints only), so the image carries what it needs to look at large inputs cheaply, and nothing
it could use to install more (ADR 0052, amendment 2026-10-28; what is left out is in ADR 0015's
v0.5 self-installed tools). Koinon's instructions tell the agent how to use them (`internal/agentd`,
`workingWithData`).

| Tool | Version | Source | Licence |
| --- | --- | --- | --- |
| git, ca-certificates | 2.43.0 | Ubuntu main | GPL-2.0; MPL-2.0 and others |
| ripgrep (`rg`) | 15.2.0 | GitHub release, static musl build (replaces Ubuntu's universe 14.1.0) | Unlicense or MIT |
| jq | 1.7.1 | Ubuntu main | MIT |
| sqlite3 | 3.45.1 | Ubuntu main | public domain |
| poppler-utils (`pdftotext`, `pdfinfo`) | 24.02.0 | Ubuntu main | **GPL-2.0 or GPL-3.0** |
| file, tree, unzip, zstd, xz-utils | 5.45, 2.1.1, 6.0, 1.5.5, 5.4.5 | Ubuntu (tree: universe) | file BSD-2-Clause, tree GPL-2.0+, unzip Info-ZIP, zstd BSD-3-Clause or GPL-2.0, xz public domain and GPL-2.0+ |
| python3, python-is-python3 | CPython 3.12.3 | Ubuntu main | PSF-2.0 |
| fd | 10.5.0 | GitHub release, static musl build | MIT or Apache-2.0 |
| yq (mikefarah) | 4.54.1 | GitHub release | MIT |
| ast-grep | 0.51.0 | GitHub release (`sg` is not installed: it would shadow `/usr/bin/sg`) | MIT |
| DuckDB CLI | 2.0.0 | GitHub release | MIT |

- **Pins.** The release binaries are fetched per architecture by `koinon-fetch` (`harnesses/common`:
  curl, then `sha256sum -c` against the pin). CI passes its `GITHUB_TOKEN` as the BuildKit secret
  `github_token` (`--secret id=github_token,env=GITHUB_TOKEN`), so the downloads are authenticated and
  not rate limited per runner IP (#145); without the secret they are anonymous, and the token is
  never an ARG, ENV, log line or layer. Each
  digest is the release asset's `digest` from the GitHub API, also hashed locally (yq's and ripgrep's match the
  upstream `checksums` / `.sha256` files; fd, ast-grep and DuckDB publish none), and the build runs each tool's
  `--version`. The licence texts of yq, ast-grep and DuckDB are fetched the same way into
  `/usr/share/doc/koinon-tools/`; fd's and ripgrep's ship in their tarballs. The DuckDB and ast-grep
  archives carry no bundled third-party notices; those live upstream (DuckDB: `third_party/` and
  `LICENSE` in github.com/duckdb/duckdb at v2.0.0; ast-grep: the dependency licences of its
  `Cargo.lock` in github.com/ast-grep/ast-grep at 0.51.0). Every apt package comes from one
  `snapshot.ubuntu.com` snapshot (`UBUNTU_SNAPSHOT` in the Dockerfile) at an exact version, so a
  rebuild gives the same packages; bump both on purpose to take security updates. The base has no
  CA bundle, so a throwaway stage borrows one over HTTP for the snapshot's HTTPS (apt checks the
  indexes against the Ubuntu archive keyring either way) and only that file is copied.
- **GPL.** poppler-utils is GPL-licensed and shipped unmodified from Ubuntu, with git, bash and
  coreutils: the same obligation. The source of the exact package versions is on
  `snapshot.ubuntu.com` under the snapshot ID below (`apt source` against that snapshot).
- **Snapshot freshness.** The snapshot is `20261027T000000Z` (`UBUNTU_SNAPSHOT` in the
  Dockerfile). Security fixes for poppler, python3, git and the other packages reach the image only
  when this ID and the pinned versions are bumped (`apt-cache policy` against the new snapshot).
  Cadence: at least monthly, and on any Ubuntu security notice for poppler-utils, python3, git,
  sqlite3 or their libraries. Automating it is a follow-up.
- **python3** is the standard library only (json, csv, sqlite3, re, gzip, zipfile, statistics,
  decimal, ...). No pip, `ensurepip`, `python3-venv`, uv or ruff; the build fails if pip appears.
  The entry script sets `PYTHONDONTWRITEBYTECODE=1`, so an agent's imports leave no `__pycache__`
  in a repository.
- **DuckDB** is installed as `/usr/local/libexec/duckdb`; `/usr/local/bin/duckdb` is a wrapper
  (`koinon-duckdb`) that passes `/etc/koinon/duckdb/init.sql` as `-cmd` and everything else
  through (`-c`, `-json`, a database file, standard input). The defaults: `memory_limit`
  768 MiB, `threads` 2, `temp_directory` `/tmp/duckdb` capped at `max_temp_directory_size` 1 GiB (it
  spills there instead of exhausting a 1-2 GiB VM), extension autoinstall and autoload off (no network; the CSV, JSON and Parquet
  readers are built in), community extensions off. The configuration is not locked: a query may
  `SET` its own values. `-cmd` is used rather than `-init` because `-init` prints a coloured
  "Loading resources" line to stderr on every start. `/tmp` is deliberately not under `/sandbox`
  (the checkpointed home). What backs `/tmp` in a Kata sandbox, and whether 1 GiB fits, is to be
  checked on the home lab (RT1).
- **Formatters.** `"formatter": false` in the config (`packages/schema/src/config.ts:70`;
  `packages/core/src/config/plugin/formatter.ts:30-31`) and no formatter binary in the image;
  `uv format` would download Ruff.
- **Size.** The root file system as `du -sxk /` inside the container reports it (`docker image
  inspect` is no measure: its size is compressed or layer-summed depending on the image store),
  before this change (commit `530bd1d`) and after: linux/arm64 429,148 KiB to 650,916 KiB (+217
  MiB, 419 to 636 MiB); linux/amd64 409,084 KiB to 632,836 KiB (+219 MiB, 400 to 618 MiB). The
  four release binaries are about 140 MB of it (DuckDB 72, ast-grep 52, yq 13, fd 3); the rest is
  apt, mostly poppler's dependencies (cairo, freetype, NSS, fonts) and Python's standard library.
  `test/harnesses` fails when an image grows past its budget (`image size`, measured size plus about
  10%: 716,000 KiB arm64, 696,000 KiB amd64; `sizeBudget` in `opencode_test.go`).
- **Not here** (ADR 0015's v0.5 self-installed tools, or never): pip and wheels (pandas, numpy),
  uv, language servers, formatters, pandoc, markitdown, miller, scc, lnav, difftastic, and
  anything AGPL (mupdf, ripgrep-all).

## Notes

- OpenCode is the glibc binary from npm (`@opencode/cli-linux-<arch>`), verified by sha256 at
  build time (`ADD --checksum`); the sha512 npm integrity values are in the Dockerfile.
- Ubuntu packages come from a pinned snapshot at exact versions, the base image is pinned by
  digest (see [Tools in the image](#tools-in-the-image)).
