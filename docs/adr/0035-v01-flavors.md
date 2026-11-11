# 0035. v0.1 flavors: OpenCode, Hermes and pi over ACP

- Status: Accepted; amended by [0059](0059-v01-opencode-only.md) and [0112](0112-harness-definitions.md)
- Date: 2026-10-21
- Decision: [design.md#35](../design.md)

## Context

A *flavor* selects the harness image an agent runs; everything else about the agent is the same
across flavors ([0009](0009-agent-runtime.md)). `agentd` drives the harness over the Agent Client
Protocol (ACP) ([0016](0016-harness-and-deployment.md)), so each flavor needs:

- an ACP server
- session persistence and reload ([0029](0029-run-level-resume.md))
- a configurable model endpoint, pointed at agentgateway
  ([0040](0040-agentgateway-model-and-mcp-gateway.md))
- support for instruction files for memory ([0033](0033-file-based-memory.md))

The project is open source and model-agnostic, so the v0.1 harnesses should be open source and work
with any model.

## Decision

- **v0.1 ships three flavors.** All are MIT-licensed and all are driven over ACP:
  - **OpenCode**, through `opencode acp`
  - **Hermes**, through `hermes acp`
  - **pi**, through the `pi-acp` adapter, which wraps pi's RPC mode
- **Claude Code and Codex come later**, using the `claude-agent-acp` and `codex-acp` adapters.
- **Flavor images** are built on the OpenShell base sandbox image
  ([0015](0015-agent-environment-and-browser.md)). OpenShell publishes no harness images, so Koinon
  builds them.
- **Each flavor ships a contract:**
  - the ACP command
  - the resume mode (`session/resume` or `session/load`), plus quirks
  - how the model base URL is configured
  - instruction file names
  - home exclude paths

  Flavor contract tests in CI check each contract ([0025](0025-testing-strategy.md)).

## Alternatives considered

- **Claude Code and Codex first** _(reconstructed)_: popular, but tied to one vendor's models.
  Claude Code's ACP adapter is Apache-2.0; the `codex-acp` license is unclear (GitHub shows
  NOASSERTION). Kept for later.
- **One flavor only, for example OpenCode** _(reconstructed)_: less work, but it would not prove
  that the flavor contract is general.
- **A Koinon-built harness** _(reconstructed)_: full control, but it duplicates mature open source
  agents.

## Consequences

- Positive:
  - Three independent harnesses keep the flavor interface honest.
  - All work with OpenAI-compatible endpoints through agentgateway.
  - All have permissive licenses.
- Negative, from the per-harness caveats:
  - **pi has no native ACP**: `pi-acp` is a third-party, "MVP-style" adapter (v0.0.34) with its own
    session map (`~/.pi/pi-acp/session-map.json`). It does not forward client-provided MCP servers
    to pi, so pi agents get MCP tools only through pi's own config, written by Koinon _(inferred)_.
  - **pi-acp offers `session/load` but not `session/resume`**, so resumes replay history.
  - **OpenCode requires the session's original `cwd`** on load, which Koinon must persist.
  - **Hermes's `resume_session` silently starts a new session** on an unknown ID; `agentd` must
    detect this.
  - Hermes is a general agent with a messaging gateway; Koinon uses only its ACP mode, and its
    gateway features stay disabled _(inferred)_.
- Follow-ups:
  - Flavor contract spec and a test harness, run against the fake model server.
  - Build and sign the three flavor images.
  - Consider contributing resume support to pi-acp upstream.

## Open questions

- Should Koinon fork or vendor pi-acp if upstream stalls?
- Is Hermes's broader tool set (messaging, self-improvement) disabled by config, or by OpenShell
  policy, or both?
- Which flavor does the v0.1 demo template use ([0036](0036-v01-demo.md))?

## Amendment (2026-10-26)

v0.1 ships OpenCode only; Hermes and pi are removed and come back, if at all, in a later release.
The flavor seam (descriptor table, template field, database column, API field) stays. See
[0059](0059-v01-opencode-only.md).

## Upstream verification

Checked 2026-10-21:

- **OpenCode** (anomalyco/opencode, MIT):
  - `opencode acp` speaks JSON-RPC over stdio, with load, resume, list and fork.
  - It accepts an OpenAI-compatible provider (`@ai-sdk/openai-compatible`, `baseURL`).
  - https://opencode.ai/docs/acp/
- **Hermes** (NousResearch/hermes-agent, MIT, v0.22.2): `hermes acp`, with load, resume and list,
  and `model.base_url` / `OPENAI_BASE_URL`. https://github.com/NousResearch/hermes-agent
- **pi** (earendil-works/pi, MIT): no ACP, `pi --mode rpc`, `models.json` `baseUrl`.
  https://github.com/earendil-works/pi
- **pi-acp** (svkozak/pi-acp, MIT, v0.0.34): `loadSession` and `list`, no resume, and MCP servers
  from the client aren't forwarded. https://zed.dev/acp/agent/pi
- **Later adapters:** `agentclientprotocol/claude-agent-acp` (Apache-2.0) and
  `zed-industries/codex-acp` (license unverified).

## Amendment (2026-11-11)

[0112](0112-harness-definitions.md) replaces the flavor concept: an agent selects a harness, and a harness definition
(built-in or custom) records how to obtain and run it. A harness is added by registering a definition,
not by a table entry and an image. The flavor contract of this ADR becomes the contract of ADR 0112,
decisions 5 to 9. The rename is groundwork before the core release; the rest is built after it.

## Links

- [0016 Execution backend and agentd](0016-harness-and-deployment.md), [0029 Run-level resume](0029-run-level-resume.md)
- [0037 Tooling](0037-tooling.md) (ACP Go SDK), [0040 agentgateway](0040-agentgateway-model-and-mcp-gateway.md)
