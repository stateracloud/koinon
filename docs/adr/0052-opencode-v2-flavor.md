# 0052. OpenCode flavor on OpenCode v2

- Status: Accepted
- Date: 2026-10-22
- Amends: [0048](0048-openshell-spike-results.md) (on PR #15) (the OpenCode version and the
  OpenCode details of decision 4; decision 4's flavor contract stands)
- Verification: spike on 2026-10-22 (below); re-pin tracked in
  [#54](https://github.com/stateracloud/koinon/issues/54)

## Context

ADR 0048 (on PR #15) verified the flavors with OpenCode 1.19.3. OpenCode's current release line is v2
(`@opencode/cli`), which reworks configuration, providers, plugins and the ACP agent. Andre
decided on 2026-10-22 to build the v0.1 flavor on v2 rather than ship on 1.x and replace it
shortly after.

A spike on 2026-10-22 rebuilt the flavor on `@opencode/cli` 2.0.24 (tag `v2.0.24`, commit
`e7a34f0`) and read its source:

- The flavor contract suite (`test/flavors`) passed 17 of 17 after adapting tool argument names,
  the out-of-workspace permission check and the unknown-session error.
- In the home lab (Kata, the real sandbox policy, workspace `kw-9f57984957089e62`) the loopback
  child server, `session/new`, `session/load` in a new harness process and a prompt through the
  fake model all passed; egress showed only the model and MCP endpoints.
- What changed for Koinon in v2:
  - Configuration lives in a global config directory (`OPENCODE_CONFIG_DIR`) with new keys
    (`providers`, `mcp.servers`, `plugins`, `update`); `XDG_CONFIG_HOME` no longer selects it.
  - `opencode acp` is a client of its own `opencode serve` child on 127.0.0.1, reached over HTTP
    through any configured proxy.
  - Any built-in provider whose environment variable is set activates on its own
    (`OPENAI_API_KEY` activates `openai`).
  - The built-in `opencode` provider is guarded: it cannot be removed and lists ten free models
    (nine `*-free` and `big-pickle`) from a bundled snapshot.
  - A new session reports a model OpenCode chose (`opencode/fledge-alpha-free` in the spike) until
    one is selected; there is no `session/set_model`, only `session/set_config_option`.
  - MCP servers connect after `session/new` returns; ACP has no MCP status, but OpenCode turns
    MCP prompts into commands and announces them with `available_commands_update`.
  - A read outside the session directory asks with permission `external_directory` and arrives
    over ACP with tool kind `read`, which koinon-agentd's fixed mapping allowed.
  - 2.0.24 ignores the `instructions` config key (read from 2.0.30) and has no `/etc/opencode`
    system config (from 2.0.28). It loads `AGENTS.md` from the global config directory and
    re-reads it on change.
  - Plugins run inside OpenCode with its privileges; the 1.x switches (`OPENCODE_PURE`,
    `OPENCODE_DISABLE_DEFAULT_PLUGINS`) are gone, replaced by a `plugins` list with negations.

## Decision

1. The OpenCode flavor uses **`@opencode/cli` 2.0.24 only**, the `linux-x64` and `linux-arm64`
   npm tarballs pinned by sha256 (checked against npm's sha512 integrity). OpenCode 1.x support is
   dropped.
2. **Configuration** is `/etc/koinon/opencode/opencode.jsonc`, read-only in the image and made the
   global config with `OPENCODE_CONFIG_DIR`. It defines the `koinon` provider
   (`aisdk:@ai-sdk/openai-compatible`) and the `koinon` MCP server (`codemode: false`), sets
   `update: "disable"`, and turns off the built-in plugins Koinon does not want
   (`-opencode.config.compatibility`, `-opencode.provider.*`, `-opencode.websearch.*`,
   `-opencode.tool.websearch`, `-opencode.wellknown`). The entry script sets `HOME` and the XDG
   directories under `/sandbox`, `OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_DISABLE_MODELS_FETCH`
   and `OPENCODE_DISABLE_PROJECT_CONFIG`, adds `127.0.0.1,localhost` to `NO_PROXY`, unsets every
   variable that moves OpenCode's config, database, server credentials or telemetry, and moves
   the key from `OPENAI_API_KEY` to `KOINON_MODEL_KEY`.
3. **Instructions** are a file koinon-agentd owns: before every harness start it writes
   `/sandbox/.koinon/AGENTS.md` from the run's agent version, through a temporary file and a
   rename, and records its hash in the run's session data; a failed write fails the run. Before
   every prompt it checks the hash and restores a changed or missing file (event
   `run.instructions_restored`). The image links the global `AGENTS.md` to it; before every prompt
   agentd also checks that the link is still a symbolic link to the file and restores it if not
   (same event; a link that cannot be restored fails the run). The entry script refuses to start
   (exit 64) when the file is missing, empty or a link.
4. **Readiness:** Koinon's MCP server publishes the prompt `koinon-ready`, which agentgateway
   admits for every key. After `session/new` or `session/load`, agentd waits (bounded) for the
   command `koinon:github_koinon-ready` and then, as a fixed settle, one second for OpenCode's tool
   registration; a timeout fails the run. Andre's decision (2026-10-22) is the `koinon-ready`
   prompt, with a fixed delay as the fallback if the prompt does not get through. That fallback is
   not built: today a prompt that does not arrive fails the run, and the only fixed delay is the
   one-second settle after the ready signal.
5. **Model:** agentd selects `koinon/<model>` with `session/set_config_option` after every
   session start or load and fails the run unless the answer shows it. It never records or trusts
   the model a session reports on its own.
6. **Permissions:** agentd refuses permission requests for paths outside the workspace in every
   preset (plan item AG1), whatever tool kind they arrive with.
7. **Plugins** stay off by default. With `harness.pluginInstalls`, the entry script lists each
   directory under `~/.config/opencode/plugins/` in `OPENCODE_CONFIG_CONTENT`; nothing else in the
   home or a repository can add one.

### Workarounds tied to 2.0.24

These go away or change when the flavor is re-pinned ([#54](https://github.com/stateracloud/koinon/issues/54)):

- the `AGENTS.md` link instead of the `instructions` key (2.0.30 reads the key; using both would
  load the file twice);
- the one-second settle after the ready command (tool registration is debounced separately from
  the command path);
- the plugin list built by the entry script (2.0.28 adds a system config directory,
  `/etc/opencode`, which the flavor would have to keep empty or own);
- the explicit `session/set_config_option` against OpenCode's own default model.

## Alternatives considered

- **Stay on 1.19.3:** works today, but v0.1 would ship on a line that has to be replaced.
- **Pin 2.0.30 or later now** _(reconstructed)_: would remove the `AGENTS.md` link, but the spike
  verified 2.0.24 end to end and later releases change semantics again (see risks); re-pinning is
  #54.
- **A fixed delay instead of the readiness prompt:** simpler, but a guess; the prompt shows that
  the server is actually connected, so it is the primary mechanism. A fixed delay stays the
  fallback if the prompt does not get through (decision 4); the short settle after the ready
  signal covers only a documented debounce.
- **Pass the MCP server in `session/new`'s `mcpServers`:** OpenCode builds that config without
  `codemode: false`, and Hermes and pi take no client MCP servers (ADR 0048, on PR #15).
- **Instructions in the image or in config:** they are per agent version; only a file agentd
  writes per run follows a version bump without recreating the sandbox.

## Consequences

- Positive: the flavor tracks OpenCode's maintained line; instructions follow the run's agent
  version; the model and the MCP tools are known before the first prompt.
- Negative: more Koinon code depends on OpenCode internals (command naming, the debounce, the
  `AGENTS.md` lookup), each checked by the contract suite.
- Accepted risks:
  - OpenCode's built-in `opencode` provider lists ten free models (`*-free`) that cannot be removed;
    they cannot be reached either, because the sandbox policy denies `opencode.ai`, and agentd
    selects the koinon model explicitly.
  - With `pluginInstalls` on, plugins run in-process with OpenCode's full power
    ([#53](https://github.com/stateracloud/koinon/issues/53)); a broken plugin can keep OpenCode from
    getting ready, which the readiness timeout bounds.
  - OpenCode's `execute` tool can call `fetch`; the policy-enforced egress contains it like any
    other process in the sandbox.
  - v2 changes semantics quickly (2.0.28 and 2.0.30 already change config and instructions);
    every re-pin reruns the contract suite and rereads the source.
- Follow-ups: #54 (re-pin), #53 (home configuration and plugins).

## Open questions

- Whether `enabled_providers`, which 2.0.24 still parses into `provider.use` deny policies, can
  hide the guarded `opencode` provider (not verified at runtime).

## Upstream verification

Checked on 2026-10-22 against OpenCode `v2.0.24` (commit
`e7a34f09bfd9134dfade5a8ddb843f7030bc9a69`) and npm (`@opencode/cli-linux-x64` and
`@opencode/cli-linux-arm64` 2.0.24 integrity values): the contract suite in Docker and the
home-lab run described in Context. Source references are in `flavors/opencode/README.md`.

## Links

- [0035](0035-v01-flavors.md), [0048](0048-openshell-spike-results.md) (on PR #15),
  [0049](0049-v01-scope-decisions.md) (on PR #16)
- [flavors/opencode/README.md](../../flavors/opencode/README.md)
- https://github.com/anomalyco/opencode/tree/v2.0.24
- https://www.npmjs.com/package/@opencode/cli
