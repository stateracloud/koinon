# pi flavor

[pi](https://pi.dev) 1.3.0 (`@earendil-works/pi-coding-agent`) driven over ACP through the
[pi-acp](https://www.npmjs.com/package/pi-acp) 0.0.34 adapter by `koinon-agentd` through
OpenShell's non-TTY exec (ADR 0035, 0048, 0049). The image holds no Koinon binary.

Build: `make flavor-pi` (loads `koinon/flavor-pi:dev`; nothing is pushed).
Contract test: `make test-flavors` (`test/flavors`, needs Docker).

## Contract

| Item (ADR 0035) | pi flavor |
| --- | --- |
| ACP command | `/usr/local/bin/koinon-pi --model-base-url URL --model NAME --mcp-url URL`, run as UID 10001 in a session working directory under `/sandbox` |
| Resume mode | `session/load` (replays history). pi-acp has **no `session/resume`** (it advertises `list` and `delete`), so agentd always loads |
| Unknown session ID | `session/load` fails with JSON-RPC `-32602` `Unknown sessionId: <id>` |
| Model endpoint | `--model-base-url` and `--model` as provider `koinon` (`openai-completions`) in pi's `models.json`; key from `$OPENAI_API_KEY`, resolved by pi at request time |
| MCP | `--mcp-url`, server `koinon` in pi's `mcp.json` with `"exposure": "direct"` and `Authorization: Bearer ${OPENAI_API_KEY}`; pi-acp takes no client MCP servers. Tools reach the model as `mcp__koinon__<tool>` |
| State | `/sandbox/.pi/agent` (`PI_CODING_AGENT_DIR`): sessions in `sessions/`, rendered config, `auth.json` (empty); pi-acp's session map in `/sandbox/.pi/pi-acp` |
| Home exclude paths | `/sandbox/.pi/agent/models-store.json`, `/sandbox/.cache` |
| Permission requests | none: pi has no permission prompts for its tools, and pi-acp sends `session/request_permission` only for extension dialogs (none are installed). The sandbox is the boundary |

## How per-agent values reach the config

agentd execs the entry script with the agent's values as arguments, as for OpenCode (see
`../opencode/README.md`). pi differs in two ways that shape the flavor:

1. pi substitutes environment variables only in `apiKey` and `headers`, not in `baseUrl`, model
   ids, `defaultModel` or MCP `url` (`dist/core/resolve-config-value.js`,
   `dist/core/provider-composer.js`, `dist/extensions/mcp/runtime.js`).
2. pi takes a lock file next to `settings.json` even to read it
   (`dist/core/settings-manager.js`, proper-lockfile), and on failure silently runs with empty
   settings. Its agent directory must be writable.

So the read-only files in [`/etc/koinon/pi/`](etc/koinon/pi) are **templates**:
[`koinon-pi`](koinon-pi) checks the arguments (no quotes, backslashes, `$`, braces, whitespace or
control characters; URLs must be `http(s)://`), and
[`koinon-pi-render.mjs`](koinon-pi-render.mjs) writes `models.json`, `settings.json` and
`mcp.json` into `/sandbox/.pi/agent` on every start, replacing whole-value `@KOINON_*@`
placeholders in the parsed JSON (never in the text). It then reads each file back and refuses
to start (exit 65) unless it is non-empty JSON carrying the Koinon provider, model and MCP
server with direct exposure: in spike 7 an empty `mcp.json` made pi run without the Koinon tools,
silently (pi ignores a missing or empty `mcp.json`, `dist/extensions/mcp/config.js`).

`"exposure": "direct"` declares the MCP tools to the model like built-in tools. pi's default
exposure lists them only to its code mode, where the model never found them (spike 7 finding 3;
`docs/mcp.md` in the package).

pi-acp starts one `pi --mode rpc` per session; `PI_ACP_PI_COMMAND` points it at the pinned pi
by absolute path.

## Phone-home, updates and telemetry

Spike 7 saw pi reach for `pi.dev` (refused by policy). Sources are the published packages
`@earendil-works/pi-coding-agent@1.3.0` and `pi-acp@0.0.34` (paths inside each package).

| What | Switch | Source |
| --- | --- | --- |
| Model catalog refresh (`pi.dev/api/models/providers/…`) in RPC mode | `PI_OFFLINE=1` | `dist/main.js`, `dist/core/model-runtime.js`, `dist/core/remote-catalog-provider.js` |
| Version check (`pi.dev/api/latest-version`) | `PI_OFFLINE=1`, `PI_SKIP_VERSION_CHECK=1` | `dist/utils/version-check.js` |
| Install telemetry (`pi.dev/api/report-install`) and attribution headers | `PI_TELEMETRY=0`, `"enableInstallTelemetry": false` | `dist/core/telemetry.js`, `dist/core/provider-attribution.js` |
| `fd` and `rg` downloads from GitHub | `ripgrep` and `fd-find` installed; `PI_OFFLINE=1` | `dist/utils/tools-manager.js` |
| Package and extension installs | none listed in settings; `PI_OFFLINE=1` | `dist/core/package-manager.js` |
| pi-acp's `npm view @earendil-works/pi-coding-agent version` on every `session/new` (no switch) | the image ships Node without npm, npx and corepack | `pi-acp` `dist/index.js` |
| pi-acp startup banner | `"quietStartup": true` | `pi-acp` `dist/index.js` |

The `@earendil-works/pi-telemetry` package pi depends on holds interfaces only, with no
exporter. Run with `--network none`, a session starts without any request beyond the model and
MCP endpoints.

## Supply chain

- Node.js 24.22.1 is the official linux tarball, verified by sha256 at build time
  (`ADD --checksum`, values from nodejs.org's `SHASUMS256.txt`).
- pi, pi-acp and all 121 dependencies are pinned by version and sha512 integrity in
  [`npm/package-lock.json`](npm/package-lock.json) and installed with `npm ci
  --ignore-scripts`; regenerate with `npm install --package-lock-only --ignore-scripts` in
  `npm/` when bumping.
- Ubuntu packages follow the archive at build time; the base image is pinned by digest.

## Risk: pi-acp is pre-1.0

pi-acp 0.0.34 ([svkozak/pi-acp](https://github.com/svkozak/pi-acp)) is a small third-party
adapter with one maintainer and no 1.0 release, sitting between Koinon and pi.
It is pinned exactly and its behaviour is covered by the contract suite. If upstream stalls or
breaks the ACP surface Koinon uses (initialize, `session/new`, `session/load`, `session/prompt`,
`session/cancel`), the fallback is to vendor it at the pinned version (MIT-licensed, a single
`dist/index.js`) or fork it under `procoduck`; pi's own RPC mode, which pi-acp wraps, is the
stable interface underneath.
