# 0052. OpenCode flavor on OpenCode v2

- Status: Accepted
- Date: 2026-10-22
- Amended by: [0079](0079-dependency-cooldown.md) (the minimum age in the 2026-10-28 amendment)
- Amends: [0048](0048-openshell-spike-results.md) (PR 15 in the previous repository) (the OpenCode version and the
  OpenCode details of decision 4; decision 4's flavor contract stands)
- Verification: spike on 2026-10-22 (below); re-pin tracked in
  old issue 54

## Context

ADR 0048 (PR 15 in the previous repository) verified the flavors with OpenCode 1.19.3. OpenCode's current release line is v2
(`@opencode/cli`), which reworks configuration, providers, plugins and the ACP agent. procoduck
decided on 2026-10-22 to build the v0.1 flavor on v2 rather than ship on 1.x and replace it
shortly after.

A spike on 2026-10-22 rebuilt the flavor on `@opencode/cli` 2.0.24 (tag `v2.0.24`, commit
`e7a34f0`) and read its source:

- The flavor contract suite (`test/flavors`) passed 17 of 17 after adapting tool argument names,
  the out-of-workspace permission check and the unknown-session error.
- In a maintainer cluster (Kata, the real sandbox policy) the loopback
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
   registration; a timeout fails the run. procoduck's decision (2026-10-22) is the `koinon-ready`
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

These go away or change when the flavor is re-pinned (old issue 54):

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
  old issue 54.
- **A fixed delay instead of the readiness prompt:** simpler, but a guess; the prompt shows that
  the server is actually connected, so it is the primary mechanism. A fixed delay stays the
  fallback if the prompt does not get through (decision 4); the short settle after the ready
  signal covers only a documented debounce.
- **Pass the MCP server in `session/new`'s `mcpServers`:** OpenCode builds that config without
  `codemode: false`, and Hermes and pi take no client MCP servers (ADR 0048, PR 15 in the previous repository).
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
    (old issue 53); a broken plugin can keep OpenCode from
    getting ready, which the readiness timeout bounds.
  - OpenCode's `execute` tool can call `fetch`; the policy-enforced egress contains it like any
    other process in the sandbox.
  - v2 changes semantics quickly (2.0.28 and 2.0.30 already change config and instructions);
    every re-pin reruns the contract suite and rereads the source.
- Follow-ups: old issue 54 (re-pin), old issue 53 (home configuration and plugins).

## Open questions

- Whether `enabled_providers`, which 2.0.24 still parses into `provider.use` deny policies, can
  hide the guarded `opencode` provider (not verified at runtime).

## Upstream verification

Checked on 2026-10-22 against OpenCode `v2.0.24` (commit
`e7a34f09bfd9134dfade5a8ddb843f7030bc9a69`) and npm (`@opencode/cli-linux-x64` and
`@opencode/cli-linux-arm64` 2.0.24 integrity values): the contract suite in Docker and the
home-lab run described in Context. Source references are in `flavors/opencode/README.md`.

## Links

- [0035](0035-v01-flavors.md), [0048](0048-openshell-spike-results.md) (PR 15 in the previous repository),
  [0049](0049-v01-scope-decisions.md) (on PR old issue 16)
- [flavors/opencode/README.md](../../flavors/opencode/README.md)
- https://github.com/anomalyco/opencode/tree/v2.0.24 (original pin), https://github.com/anomalyco/opencode/tree/v2.2.0 (amendment 2026-10-28)
- https://www.npmjs.com/package/@opencode/cli

## Amendment 2026-10-28: re-pin to 2.2.0 (old issue 54)

procoduck decided on 2026-10-28 to move the OpenCode flavor from `@opencode/cli` 2.0.24 to **2.2.0**
(published 2026-10-27), **waiving the project's two-week minimum age for this update only**. (Amended 2026-10-29: [0079](0079-dependency-cooldown.md) replaces the unwritten two-week rule with a 3-day minimum and a security fast path; this waiver was made under the old rule and stands as recorded.) The
reasons: 2.2.0 bundles Bun 1.4.4 (verified in the x64 binary: `Bun v1.4.4 (38afee6c8) Linux x64`;
the Bun release is from 2026-10-22), which fixes security issues in TLS, HTTP/1.1-3, WebSocket,
`fetch` and Brotli ([bun.com/blog/bun-v1.4.4#security](https://bun.com/blog/bun-v1.4.4#security)),
and 2.2.0 makes external-directory permission checks use the project's filesystem
([anomalyco/opencode#59089](https://github.com/anomalyco/opencode/pull/59089)). The known GHSA
advisories were all fixed in 1.x and never affected 2.0.24. The repository is now
`github.com/anomalyco/opencode` (formerly `sst/opencode`).

Pin: `@opencode/cli-linux-x64` and `-arm64` 2.2.0 from `registry.npmjs.org`, sha256 checked by
`ADD --checksum` after comparing the tarballs with npm's `dist.integrity` (values in
`flavors/opencode/Dockerfile`); OpenCode tag `v2.2.0`, commit
`1097e2668b1e08ad63bdf0b1c4860b9bc01e420c`. Decision 1's "2.0.24 only" now reads 2.2.0.

What changed for old issue 54:

- **Instructions.** The config's `instructions` key names `/sandbox/.koinon/AGENTS.md`; the
  `AGENTS.md` link in the global config directory is gone. The descriptor's `InstructionLink` for
  OpenCode is empty; agentd still restores the file's hash before every prompt. The link-restore
  code and `LinkScript` stay in agentd for a flavor that needs a link; the integration specs that
  exercised it through OpenCode were removed.
- **`/etc/opencode`.** Not used. 2.2.0 reads it as a managed *system* directory, but the global
  directory would then be `~/.config/opencode` in the agent's home, a writable configuration
  source. `OPENCODE_CONFIG_DIR=/etc/koinon/opencode` stays (flavors/opencode/README.md,
  "Global config directory").
- **Environment variables and `plugins`.** Re-checked against the 2.2.0 source: the unset list
  stays; the new provider plugins (`databricks`, `neon`, `openai-context-caps`) fall under
  `-opencode.provider.*`; 2.2.0 adds three network-capable source files, none of which can run in
  Koinon's configuration: `core/src/usage-limits.ts` (OpenCode Console budgets; needs a Console
  credential, which the sandbox never has), `core/src/config/plugin/instruction.ts` (fetches URL
  entries of `instructions`; ours is a path) and `core/src/plugin/websearch/tinyfish.ts` (turned
  off by `-opencode.websearch.*`). Added: `OPENCODE_DISABLE_CLAUDE_CODE=1`, `OPENCODE_DISABLE_FREE_MODELS=1`, and unsetting
  `OPENCODE_API_KEY` and `OPENCODE_REPO_CLONE_GITHUB_BASE_URL`.
- **Permissions (sandbox posture).** 2.2.0 dropped OpenCode's default `external_directory: ask`
  and the exemptions for its own directories, and the explore subagent allows every external
  directory; without a change a `read` of `/etc/passwd` ran with no permission request. The
  config now sets the rule and the exemptions explicitly, for all agents and for `explore`.
- **ACP.** `session/load` is used as before (the contract suite's resume and unknown-session specs
  pass unchanged: `session not found`); `session/resume` is still advertised and unused. The model
  option is still `koinon/<model>`, selected with `session/set_config_option`. Permission requests
  gained a `name` field on the tool call.

Still tied to the pin: the one-second settle after the ready command, the plugin list built by the
entry script, and the explicit model selection.

Verification: `make flavor-opencode` and the flavor contract suite (`test/flavors`) against the
2.2.0 image, as recorded in the pull request.

## Amendment 2026-10-28: data and scripting tools in the image

procoduck decided on 2026-10-28 that the OpenCode image ships the v0.1 "core" data tools including
DuckDB, and the smallest useful Python. The agent reads repositories, logs, CSV, JSON, Parquet,
SQLite files and PDFs in a sandbox with 1-2 GiB and no network; OpenCode cuts a tool result at
2,000 lines or 50 KiB, so tools that answer with counts, schemas and selected fields keep the
context small. This is a change to the image, not to the harness contract of decisions 1-7.

- **Tools.** From Ubuntu 24.04 (one `snapshot.ubuntu.com` snapshot, exact versions): `jq`,
  `sqlite3`, `poppler-utils`, `file`, `tree`, `unzip`, `zstd`, `xz-utils`, `python3` and
  `python-is-python3`. From release binaries pinned by sha256 per architecture: fd 10.5.0,
  yq 4.54.1 (mikefarah), ast-grep 0.51.0 and the DuckDB CLI 2.0.0. Versions, licences and sizes
  are in `flavors/opencode/README.md`.
- **Python** is Ubuntu's `python3` (3.12, main, Canonical security maintenance): the whole
  standard library, no pip, `ensurepip`, `venv` package or wheels. `python3-minimal` lacks `json`
  and `sqlite3`; python-build-standalone is larger and has its own CVE tracking. The entry script
  sets `PYTHONDONTWRITEBYTECODE=1`.
- **DuckDB** runs behind a wrapper with `/etc/koinon/duckdb/init.sql`: `memory_limit` 768 MiB,
  `threads` 2, spill directory `/tmp/duckdb`, extension autoinstall and autoload and community
  extensions off. Not locked, so a query can change its own limits.
- **No formatter.** `"formatter": false` is set explicitly; the image has no `uv` or `ruff`
  (`uv format` downloads Ruff).
- **Licences.** poppler-utils is GPL, shipped unmodified from Ubuntu, the same obligation as git,
  bash and coreutils already in the image (corresponding source: Ubuntu's). AGPL tools (mupdf,
  ripgrep-all) stay out.
- **Instructions.** koinon-agentd puts a short "Working with large data" section after its
  preamble in `/sandbox/.koinon/AGENTS.md` (`internal/agentd/instructions.go`).
- **Guards.** The Dockerfile fails the build when the standard library the agent relies on is
  incomplete, when pip is present, or when a tool or the DuckDB defaults do not work. The flavor
  suite runs each tool as UID 10001 without a network and fails when an image exceeds its size
  budget.
- **Left out** (ADR 0015's v0.5 self-installed tools, or by licence): pip and wheels (pandas,
  numpy), uv, language servers and formatters, pandoc, markitdown, miller, scc, lnav,
  difftastic, mupdf, ripgrep-all.
- **Cost.** The root file system grows by about 217 MiB on arm64 (419 to 636 MiB) and 219 MiB on
  amd64 (400 to 618 MiB). Dropping DuckDB (72 MB) is the lean variant.

Verification: `make flavor-opencode` and the flavor contract suite against the new image, as
recorded in the pull request.

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.

## Amendment 2026-11-10: re-pin to 2.2.6 (#422)

The OpenCode flavor moves from 2.2.0 to **2.2.6** (`@opencode/cli-linux-x64` and `-arm64` from
`registry.npmjs.org`, sha256 in `flavors/opencode/Dockerfile`). Release notes 2.2.1 to 2.2.6 were
read; nothing in them changes a decision above. The workarounds and source references in this ADR
and in `flavors/opencode/README.md` were measured on 2.2.0 and stay worded as 2.2.0 until the flavor
e2e (`test/flavors`) has re-measured them.
